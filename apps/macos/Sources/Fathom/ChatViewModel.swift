import Foundation
import SwiftUI

// MARK: - ChatMessage (was: ChatTurn paired user+agent)
//
// We model each persisted message as its own row instead of pairing
// user+agent into a "turn." That's what the threads API stores, and
// it makes rendering cleanly handle:
//   - messages from other devices (phone → Mac)
//   - the agent emitting multiple replies (rare, but possible)
//   - replays of history with mixed authors
//
// Each row carries a stable id from the server so SSE re-deliveries
// can be deduped.

struct ChatMessage: Identifiable, Hashable {
    let id: String
    let role: String     // "user" | "agent" | "system"
    let content: String
    let deviceId: String?
    let createdAt: Date

    init(_ dto: MessageDTO) {
        self.id = dto.id
        self.role = dto.role
        self.content = dto.content
        self.deviceId = dto.deviceId
        self.createdAt = dto.createdAt
    }

    init(id: String, role: String, content: String, deviceId: String?, createdAt: Date) {
        self.id = id
        self.role = role
        self.content = content
        self.deviceId = deviceId
        self.createdAt = createdAt
    }

    var isUser: Bool { role == "user" }
}

// ChatViewModel owns the live thread + its messages + the SSE stream.
// Loading order on .onAppear:
//   1. fetch /api/v1/threads          → populate `threads`
//   2. pick a currentThread            (last-used from UserDefaults, else newest)
//   3. fetch /api/v1/threads/{id}     → populate `messages`
//   4. subscribe SSE                   → live updates from other devices
@MainActor
final class ChatViewModel: ObservableObject {
    @Published var threads: [ThreadDTO] = []
    @Published var currentThread: ThreadDTO?
    @Published var messages: [ChatMessage] = []
    /// True when the agent is processing — driven by both local sends
    /// and remote agent_thinking SSE events so every device watching
    /// the thread shows the spinner.
    @Published var busy = false
    @Published var lastError: String?

    private let client = GatewayClient()
    private var streamTask: Task<Void, Never>?
    private var seenIds: Set<String> = []
    private var refreshTask: Task<Void, Never>?

    private let lastThreadKey = "fathom.macos.lastThread"

    // MARK: - Thread lifecycle

    func bootstrap() {
        Task { await refreshThreads(autoSelect: true) }
        startPeriodicRefresh()
    }

    // startPeriodicRefresh polls /api/v1/threads every 10s while the
    // ViewModel is alive, so threads created or updated on another
    // device (phone, CLI, browser) surface in the picker without the
    // user having to switch sessions or restart the app.
    private func startPeriodicRefresh() {
        refreshTask?.cancel()
        refreshTask = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(nanoseconds: 10_000_000_000) // 10s
                guard let self else { return }
                await self.refreshThreads(autoSelect: false)
            }
        }
    }

    func refreshThreads(autoSelect: Bool) async {
        do {
            let list = try await client.listThreads()
            self.threads = list
            if autoSelect, currentThread == nil {
                let lastID = UserDefaults.standard.string(forKey: lastThreadKey)
                let picked = list.first(where: { $0.id == lastID }) ?? list.first
                if let picked {
                    await switchThread(picked)
                } else {
                    // No threads exist — create the first one.
                    await createAndSwitch()
                }
            }
        } catch GatewayError.unauthorized {
            lastError = "Token rejected. Settings → re-enter."
            Settings.shared.clearToken()
        } catch is CancellationError {
            // The periodic refresh Task was cancelled (likely teardown or
            // a structured-concurrency stack unwind) mid-fetch. Silent.
        } catch let e as URLError where e.code == .cancelled {
            // URLSession cancelled (Task cancellation in the inflight
            // request). Silent — not a real error from the user's POV.
        } catch {
            lastError = "Could not load threads: \(error.localizedDescription)"
        }
    }

    func switchThread(_ thread: ThreadDTO) async {
        // Tear down any in-flight stream before we move on.
        streamTask?.cancel()
        streamTask = nil

        currentThread = thread
        messages = []
        seenIds.removeAll()
        UserDefaults.standard.set(thread.id, forKey: lastThreadKey)

        do {
            let (refreshed, history) = try await client.getThread(thread.id)
            // Server's metadata can be newer (title etc.) — prefer it.
            currentThread = refreshed
            for dto in history { append(ChatMessage(dto)) }
        } catch GatewayError.unauthorized {
            lastError = "Token rejected. Settings → re-enter."
            Settings.shared.clearToken()
            return
        } catch {
            lastError = "Could not load history: \(error.localizedDescription)"
        }

        // Start the live subscription. Reconnect with backoff in-loop.
        let threadID = thread.id
        streamTask = Task { [weak self] in
            await self?.runStream(threadID: threadID)
        }
    }

    func createAndSwitch() async {
        do {
            let t = try await client.createThread()
            threads.insert(t, at: 0)
            await switchThread(t)
        } catch {
            lastError = "Could not create thread: \(error.localizedDescription)"
        }
    }

    private func runStream(threadID: String) async {
        var backoff: UInt64 = 500_000_000 // 0.5s in ns
        while !Task.isCancelled {
            do {
                try await client.streamThread(threadId: threadID) { [weak self] evt in
                    Task { @MainActor in
                        guard let self else { return }
                        // Only apply if we're still on this thread.
                        guard self.currentThread?.id == threadID else { return }
                        switch evt.type {
                        case "agent_thinking":
                            // Cross-device spinner: another device (or
                            // this one) just sent — show the agent is
                            // working until agent_done arrives.
                            self.busy = true
                        case "agent_done":
                            self.busy = false
                            if let m = evt.message {
                                self.append(ChatMessage(m))
                            }
                        case "agent_error":
                            self.busy = false
                            if let err = evt.error, !err.isEmpty {
                                self.lastError = "stream: \(err)"
                            }
                        default:
                            // user_message and anything else with a
                            // message field — append + dedup.
                            if let m = evt.message {
                                self.append(ChatMessage(m))
                            }
                            if let err = evt.error, !err.isEmpty {
                                self.lastError = "stream: \(err)"
                            }
                        }
                    }
                }
                return // clean EOF — caller is shutting down
            } catch is CancellationError {
                return
            } catch GatewayError.unauthorized {
                await MainActor.run {
                    self.lastError = "Token rejected. Settings → re-enter."
                    Settings.shared.clearToken()
                }
                return
            } catch {
                // Silent retry with backoff (cap 5s) — transient network blips
                // shouldn't add UI noise.
                try? await Task.sleep(nanoseconds: backoff)
                if backoff < 5_000_000_000 { backoff *= 2 }
                continue
            }
        }
    }

    // MARK: - Sending

    func send(_ text: String) async {
        guard let thread = currentThread else { return }

        // Optimistic render — show the user's message immediately so
        // the input doesn't appear to vanish during a long agent call.
        // Synthesize a local id; when the server's real user_message
        // comes back (via POST + SSE) it has a different id, so we
        // pre-stamp the real id once we know it to prevent a dup.
        let optimistic = ChatMessage(
            id: "local-\(UUID().uuidString)",
            role: "user",
            content: text,
            deviceId: nil,
            createdAt: Date()
        )
        append(optimistic)

        busy = true
        lastError = nil
        defer { busy = false }

        do {
            let (user, agent) = try await client.sendThreadMessage(
                threadId: thread.id, text: text)
            // Mark the server's user_message id as already-rendered so
            // the SSE redelivery doesn't double-render. We don't add
            // a second user bubble — the optimistic one IS the user
            // bubble, we're just claiming the server's id.
            seenIds.insert(user.id)
            // Agent message is genuinely new — render it.
            append(ChatMessage(agent))
            // Title may have auto-updated server-side; also refresh
            // the thread list so updated_at bumps the current thread
            // to the top and any cross-device new threads appear.
            await refreshThreads(autoSelect: false)
        } catch GatewayError.unauthorized {
            lastError = "Token rejected. Settings → re-enter."
            Settings.shared.clearToken()
        } catch let GatewayError.http(status, body) {
            lastError = "HTTP \(status): \(body.prefix(200))"
        } catch {
            lastError = "Network error: \(error.localizedDescription)"
        }
    }

    private func refreshCurrentThreadTitle() async {
        guard let id = currentThread?.id else { return }
        do {
            let (t, _) = try await client.getThread(id)
            currentThread = t
            // Also surface in the list.
            if let idx = threads.firstIndex(where: { $0.id == id }) {
                threads[idx] = t
            } else {
                threads.insert(t, at: 0)
            }
        } catch { /* best effort */ }
    }

    // MARK: - Internal

    private func append(_ msg: ChatMessage) {
        guard !seenIds.contains(msg.id) else { return }

        // If SSE just delivered the server's copy of a user message
        // that we've also rendered optimistically (local-<uuid> id),
        // swap the placeholder in place instead of appending a
        // duplicate. Without this, when the relay/network delivers
        // SSE before sendThreadMessage returns, the user sees two of
        // their own bubbles for the full agent-thinking window — the
        // POST-response side of the dedup only stamps the server id
        // into seenIds, which is too late for the bubble already in
        // the list. Matches the same fix on iOS.
        if msg.role == "user" {
            if let idx = messages.firstIndex(where: {
                $0.id.hasPrefix("local-") &&
                $0.role == "user" &&
                $0.content == msg.content
            }) {
                seenIds.remove(messages[idx].id)
                messages[idx] = msg
                seenIds.insert(msg.id)
                return
            }
        }

        seenIds.insert(msg.id)
        messages.append(msg)
    }

    func teardown() {
        streamTask?.cancel()
        streamTask = nil
        refreshTask?.cancel()
        refreshTask = nil
    }
}
