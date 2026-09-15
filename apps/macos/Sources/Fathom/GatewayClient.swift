import Foundation

enum GatewayError: Error {
    case unauthorized
    case http(status: Int, body: String)
    case malformed
}

/// Tracks the wall-clock time of the most-recently-yielded byte on an
/// SSE stream. Used by the streamThread watchdog to recycle a wedged
/// URLSession when no heartbeats or events have arrived for a while.
actor LastByteTracker {
    private var lastBump: Date = Date()

    func bump() {
        lastBump = Date()
    }

    func staleness() -> TimeInterval {
        Date().timeIntervalSince(lastBump)
    }
}

// MARK: - Thread / Message DTOs (mirror internal/threads.{Thread,Message})

struct ThreadDTO: Identifiable, Codable, Hashable {
    let id: String
    let userId: String
    let title: String
    let createdAt: Date
    let updatedAt: Date

    enum CodingKeys: String, CodingKey {
        case id
        case userId = "user_id"
        case title
        case createdAt = "created_at"
        case updatedAt = "updated_at"
    }

    var displayTitle: String { title.isEmpty ? "New chat" : title }
}

struct MessageDTO: Identifiable, Codable, Hashable {
    let id: String
    let threadId: String
    let role: String
    let content: String
    let deviceId: String?
    let createdAt: Date

    enum CodingKeys: String, CodingKey {
        case id
        case threadId = "thread_id"
        case role
        case content
        case deviceId = "device_id"
        case createdAt = "created_at"
    }
}

/// Server-sent event payload from /threads/{id}/stream.
struct StreamEventDTO: Codable {
    let type: String
    let message: MessageDTO?
    let error: String?
}

// GatewayClient is the URLSession wrapper around the Fathom gateway. As of
// the thread rewrite it speaks /api/v1/threads/* (persistent + multi-
// device), with /api/v1/message kept as a fallback for back-compat.
final class GatewayClient {
    private let session: URLSession

    init() {
        let cfg = URLSessionConfiguration.ephemeral
        // Local LLMs with many tools (qwen3:14b + 30+ tools) can genuinely
        // take 60-90s to think then invoke a skill that itself takes 5-15s
        // (WhatsApp Baileys handshake). Bumped from 90s.
        cfg.timeoutIntervalForRequest = 300
        cfg.timeoutIntervalForResource = 600
        cfg.waitsForConnectivity = false
        self.session = URLSession(configuration: cfg)
    }

    // MARK: - Thread CRUD

    func listThreads() async throws -> [ThreadDTO] {
        struct Resp: Decodable { let threads: [ThreadDTO]? }
        let resp: Resp = try await getJSON("/api/v1/threads")
        return resp.threads ?? []
    }

    func createThread(title: String = "") async throws -> ThreadDTO {
        let body = try JSONSerialization.data(withJSONObject: ["title": title])
        return try await postJSON("/api/v1/threads", body: body)
    }

    /// Fetches thread metadata + the tail of message history (up to 50).
    func getThread(_ id: String) async throws -> (ThreadDTO, [MessageDTO]) {
        struct Resp: Decodable { let thread: ThreadDTO; let messages: [MessageDTO]? }
        let resp: Resp = try await getJSON("/api/v1/threads/\(id)")
        return (resp.thread, resp.messages ?? [])
    }

    /// Sends a user message to a thread. Returns the persisted user + agent
    /// messages. The same messages are also published to SSE subscribers, so
    /// `streamThread` callers will see them; dedup by id at the call site.
    func sendThreadMessage(threadId: String, text: String) async throws
        -> (user: MessageDTO, agent: MessageDTO)
    {
        struct Resp: Decodable {
            let userMessage: MessageDTO
            let agentMessage: MessageDTO
            enum CodingKeys: String, CodingKey {
                case userMessage = "user_message"
                case agentMessage = "agent_message"
            }
        }
        let body = try JSONSerialization.data(withJSONObject: ["text": text])
        let resp: Resp = try await postJSON(
            "/api/v1/threads/\(threadId)/messages", body: body)
        return (resp.userMessage, resp.agentMessage)
    }

    /// Subscribes to the per-thread SSE stream. Invokes onEvent for each
    /// event, on the cooperative cancellation point of the consuming task.
    /// Returns when the stream closes; caller's surrounding Task should
    /// re-subscribe with backoff.
    ///
    /// Stale-connection watchdog: URLSession.AsyncBytes can wedge — the
    /// socket stays open but the iterator stops yielding bytes (no
    /// heartbeats, no events) until the app is relaunched. Two
    /// confirmed reproductions in May 2026, both fixed by relaunch.
    /// We work around it with a watchdog Task that cancels the
    /// streaming URLSession if no bytes arrive for 45s. The gateway
    /// heartbeats every 25s (see `internal/gateway/threads.go:479`),
    /// so 45s gives us a comfortable margin while staying responsive.
    /// Cancellation surfaces as a `URLError.cancelled` here, which the
    /// caller's retry loop catches → fresh socket on next attempt.
    func streamThread(threadId: String, onEvent: @escaping (StreamEventDTO) -> Void) async throws {
        let url = try await buildURL(path: "/api/v1/threads/\(threadId)/stream")
        var req = URLRequest(url: url)
        req.setValue("text/event-stream", forHTTPHeaderField: "Accept")
        req.setValue("Bearer \(await Settings.shared.token ?? "")",
                     forHTTPHeaderField: "Authorization")
        // No timeout on SSE — these stay open indefinitely. The watchdog
        // below covers the wedged-connection case that timeouts can't.
        let cfg = URLSessionConfiguration.ephemeral
        cfg.timeoutIntervalForRequest = 0
        cfg.timeoutIntervalForResource = 0
        let streamSession = URLSession(configuration: cfg)
        let (bytes, response) = try await streamSession.bytes(for: req)
        guard let http = response as? HTTPURLResponse else {
            throw GatewayError.malformed
        }
        if http.statusCode == 401 { throw GatewayError.unauthorized }
        if http.statusCode != 200 {
            throw GatewayError.http(status: http.statusCode, body: "")
        }

        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601withFractional

        // Stale-detection scaffolding (see doc comment above).
        let lastByte = LastByteTracker()
        let watchdog = Task {
            while !Task.isCancelled {
                try? await Task.sleep(nanoseconds: 5_000_000_000)
                if Task.isCancelled { return }
                if await lastByte.staleness() > 45 {
                    streamSession.invalidateAndCancel()
                    return
                }
            }
        }
        defer { watchdog.cancel() }

        // NOTE: URLSession's AsyncBytes.lines DROPS empty lines. The SSE
        // spec uses an empty line as the event-dispatch separator, so a
        // strict per-spec parser never fires here. Workaround: dispatch
        // each `data:` line independently. The Fathom gateway emits
        // exactly one data line per event (see writeSSEEvent), so this
        // is correct in practice even though it doesn't honor the
        // spec's "multiple data lines join with \n" rule. If we ever
        // wire a server that emits multi-line events, switch to raw
        // bytes via URLSessionDataDelegate instead.
        for try await line in bytes.lines {
            await lastByte.bump()
            if line.hasPrefix(":") { continue } // heartbeat comment
            if line.hasPrefix("event:") { continue } // type comes from the JSON
            if line.hasPrefix("data:") {
                let raw = String(line.dropFirst("data:".count))
                    .trimmingCharacters(in: .whitespaces)
                if raw.isEmpty { continue }
                if let data = raw.data(using: .utf8),
                   let evt = try? decoder.decode(StreamEventDTO.self, from: data) {
                    onEvent(evt)
                }
            }
        }
    }

    // MARK: - Health (settings test button)

    func ping() async -> Bool {
        do {
            let url = try await buildURL(path: "/api/v1/health")
            let (_, response) = try await session.data(from: url)
            return (response as? HTTPURLResponse)?.statusCode == 200
        } catch {
            return false
        }
    }

    // MARK: - Generic JSON helpers

    private func getJSON<T: Decodable>(_ path: String) async throws -> T {
        let url = try await buildURL(path: path)
        var req = URLRequest(url: url)
        req.setValue("Bearer \(await Settings.shared.token ?? "")",
                     forHTTPHeaderField: "Authorization")
        let (data, response) = try await session.data(for: req)
        guard let http = response as? HTTPURLResponse else {
            throw GatewayError.malformed
        }
        if http.statusCode == 401 { throw GatewayError.unauthorized }
        if http.statusCode != 200 {
            throw GatewayError.http(
                status: http.statusCode,
                body: String(data: data, encoding: .utf8) ?? "")
        }
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601withFractional
        return try decoder.decode(T.self, from: data)
    }

    private func postJSON<T: Decodable>(_ path: String, body: Data) async throws -> T {
        let url = try await buildURL(path: path)
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.httpBody = body
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.setValue("Bearer \(await Settings.shared.token ?? "")",
                     forHTTPHeaderField: "Authorization")
        let (data, response) = try await session.data(for: req)
        guard let http = response as? HTTPURLResponse else {
            throw GatewayError.malformed
        }
        if http.statusCode == 401 { throw GatewayError.unauthorized }
        if http.statusCode != 200 {
            throw GatewayError.http(
                status: http.statusCode,
                body: String(data: data, encoding: .utf8) ?? "")
        }
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601withFractional
        return try decoder.decode(T.self, from: data)
    }

    @MainActor
    private func buildURL(path: String) throws -> URL {
        let base = Settings.shared.gatewayURL
            .trimmingCharacters(in: CharacterSet(charactersIn: "/ "))
        guard let url = URL(string: base + path) else {
            throw GatewayError.malformed
        }
        return url
    }
}

// MARK: - Date decoding helper
//
// The gateway sends timestamps as RFC 3339 with fractional seconds
// (`2026-05-24T05:33:44.601602Z`). The stdlib `.iso8601` strategy
// rejects fractional seconds; provide a custom one that handles both
// fractional and integer-second variants.

extension JSONDecoder.DateDecodingStrategy {
    static var iso8601withFractional: JSONDecoder.DateDecodingStrategy {
        return .custom { decoder in
            let container = try decoder.singleValueContainer()
            let s = try container.decode(String.self)
            let f = ISO8601DateFormatter()
            f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
            if let d = f.date(from: s) { return d }
            f.formatOptions = [.withInternetDateTime]
            if let d = f.date(from: s) { return d }
            throw DecodingError.dataCorruptedError(
                in: container,
                debugDescription: "not an ISO 8601 date: \(s)")
        }
    }
}
