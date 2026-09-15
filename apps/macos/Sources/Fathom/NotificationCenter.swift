import Foundation
import UserNotifications

// EventSubscriber long-polls the gateway's /api/v1/events endpoint and
// renders each event as a macOS notification. Currently watches for
// scheduled_job_done events; future event types (canary trips, audit
// alerts) can be added by extending render().
//
// Design notes:
//
//  - One long-running Task per app launch. Cancelled on deinit.
//  - On launch we DON'T notify for everything that happened while the
//    app was closed — we start watching from "current high-water mark".
//    That avoids spamming the user with old events on every relaunch.
//  - Errors are silently retried with backoff. The user shouldn't see
//    a "couldn't reach gateway" banner just because they closed the
//    laptop lid.

@MainActor
final class EventSubscriber {
    private var task: Task<Void, Never>?
    private let center = UNUserNotificationCenter.current()

    func start() {
        guard task == nil else { return }
        Task { await requestPermission() }
        task = Task { [weak self] in await self?.run() }
    }

    func stop() {
        task?.cancel()
        task = nil
    }

    private func requestPermission() async {
        do {
            _ = try await center.requestAuthorization(options: [.alert, .sound])
        } catch {
            // Permission denied or revoked — we just don't post notifications.
        }
    }

    private func run() async {
        // Seed sinceID at the current head so we don't replay history.
        var sinceID: Int64 = await fetchCurrentMaxID()
        var backoff: TimeInterval = 1.0
        while !Task.isCancelled {
            let result = await poll(since: sinceID, waitSec: 25)
            switch result {
            case .empty(let lastID):
                sinceID = max(sinceID, lastID)
                backoff = 1.0 // reset on success
            case .events(let evs, let lastID):
                sinceID = max(sinceID, lastID)
                for e in evs { await render(e) }
                backoff = 1.0
            case .unauthorized:
                // Don't burn cycles when the token is wrong; pause longer.
                try? await Task.sleep(nanoseconds: 30_000_000_000)
            case .error:
                try? await Task.sleep(nanoseconds: UInt64(backoff * 1_000_000_000))
                backoff = min(backoff * 2, 30)
            }
        }
    }

    // fetchCurrentMaxID does a single non-blocking poll to learn what
    // event id the gateway is at, so we only notify on *new* events.
    private func fetchCurrentMaxID() async -> Int64 {
        let r = await poll(since: 0, waitSec: 0)
        switch r {
        case .empty(let id), .events(_, let id): return id
        default: return 0
        }
    }

    private enum PollResult {
        case empty(lastID: Int64)
        case events(events: [GatewayEvent], lastID: Int64)
        case unauthorized
        case error
    }

    private func poll(since: Int64, waitSec: Int) async -> PollResult {
        let base = Settings.shared.gatewayURL.trimmingCharacters(in: CharacterSet(charactersIn: "/ "))
        guard let token = Settings.shared.token, !token.isEmpty else { return .unauthorized }
        guard var comp = URLComponents(string: "\(base)/api/v1/events") else { return .error }
        comp.queryItems = [
            URLQueryItem(name: "since", value: String(since)),
            URLQueryItem(name: "wait", value: String(waitSec)),
        ]
        guard let url = comp.url else { return .error }

        var req = URLRequest(url: url, timeoutInterval: TimeInterval(waitSec) + 10)
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        do {
            let (data, response) = try await URLSession.shared.data(for: req)
            guard let http = response as? HTTPURLResponse else { return .error }
            if http.statusCode == 401 { return .unauthorized }
            if http.statusCode != 200 { return .error }
            let decoded = try JSONDecoder().decode(EventsResponse.self, from: data)
            if decoded.events.isEmpty {
                return .empty(lastID: decoded.lastId)
            }
            return .events(events: decoded.events, lastID: decoded.lastId)
        } catch {
            return .error
        }
    }

    private func render(_ event: GatewayEvent) async {
        let content = UNMutableNotificationContent()
        content.sound = .default
        switch event.type {
        case "scheduled_job_done":
            content.title = "Fathom"
            content.subtitle = "Scheduled job complete"
            content.body = (event.detail["reply"]?.value as? String) ?? "(no reply)"
        default:
            content.title = "Fathom"
            content.body = "\(event.type): \(event.detail)"
        }
        let req = UNNotificationRequest(
            identifier: "fathom-\(event.id)",
            content: content,
            trigger: nil,
        )
        try? await center.add(req)
    }
}

// Wire types matching internal/gateway/events.go's JSON shape.
private struct EventsResponse: Decodable {
    let events: [GatewayEvent]
    let lastId: Int64
}

struct GatewayEvent: Decodable {
    let id: Int64
    let timestamp: String
    let type: String
    let detail: [String: AnyCodable]

    // Custom init to coerce mixed-type JSON values to a usable form.
    enum CodingKeys: String, CodingKey { case id, timestamp, type, detail }

    var detailAny: [String: Any] {
        var out: [String: Any] = [:]
        for (k, v) in detail { out[k] = v.value }
        return out
    }
}

// Quick AnyCodable wrapper so we can decode arbitrary JSON dict values
// (the gateway's Event.Detail is map[string]interface{}). Used only for
// notification rendering, not for app logic.
struct AnyCodable: Decodable {
    let value: Any
    init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if let v = try? c.decode(String.self) { self.value = v; return }
        if let v = try? c.decode(Int.self) { self.value = v; return }
        if let v = try? c.decode(Double.self) { self.value = v; return }
        if let v = try? c.decode(Bool.self) { self.value = v; return }
        if let v = try? c.decode([AnyCodable].self) { self.value = v.map(\.value); return }
        if let v = try? c.decode([String: AnyCodable].self) {
            var out: [String: Any] = [:]; for (k, vv) in v { out[k] = vv.value }
            self.value = out
            return
        }
        self.value = NSNull()
    }
}

extension GatewayEvent {
    // Convenience accessor so the render switch reads `event.detail["reply"]`.
    subscript(_ key: String) -> Any? {
        detail[key]?.value
    }
}

private extension Dictionary where Key == String, Value == AnyCodable {
    subscript(asAny key: String) -> Any? { self[key]?.value }
}
