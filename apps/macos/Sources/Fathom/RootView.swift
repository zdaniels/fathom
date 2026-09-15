import SwiftUI

// RootView is the top-level container of the popover window. It branches
// on whether settings are configured (Settings.isConfigured) — first run
// shows the SettingsView so the user can paste their token + confirm
// gateway URL; subsequent loads show the chat.

struct RootView: View {
    @EnvironmentObject var settings: Settings
    @EnvironmentObject var chat: ChatViewModel
    @State private var showingSettings = false

    var body: some View {
        VStack(spacing: 0) {
            header
            Divider()
            if settings.isConfigured && !showingSettings {
                ChatView()
                    .onAppear {
                        if chat.currentThread == nil {
                            // First-open: full bootstrap.
                            chat.bootstrap()
                        } else {
                            // Subsequent opens: refresh the thread list so
                            // threads created on the phone / CLI since last
                            // open appear in the picker without manual
                            // "Refresh."
                            Task { await chat.refreshThreads(autoSelect: false) }
                        }
                    }
            } else {
                SettingsView(onDone: { showingSettings = false })
            }
        }
        .background(Color(NSColor.windowBackgroundColor))
        // NOTE: deliberately no .onDisappear teardown. MenuBarExtra
        // closes the popover constantly (every click-away), but the
        // app process keeps running. Tearing down here would kill
        // the SSE subscription + periodic refresh every time the user
        // looks away, which is the opposite of the "always synced"
        // experience we want. ChatViewModel teardown is called from
        // app termination instead (NSApp.terminate cascades).
    }

    private var header: some View {
        HStack(spacing: 10) {
            if let mark = NSImage(named: "AppMark") {
                Image(nsImage: mark)
                    .resizable()
                    .interpolation(.high)
                    .frame(width: 20, height: 20)
            } else {
                Image(systemName: "water.waves")
                    .foregroundStyle(Color.accentColor)
                    .imageScale(.large)
            }
            Text("Fathom")
                .font(.system(size: 14, weight: .semibold))

            if settings.isConfigured {
                Text("·")
                    .foregroundStyle(.tertiary)
                ThreadPicker()
            }

            Spacer()
            if settings.isConfigured {
                Button(action: { Task { await chat.createAndSwitch() } }) {
                    Image(systemName: "square.and.pencil")
                        .foregroundStyle(.secondary)
                }
                .buttonStyle(.plain)
                .help("New chat")

                // Server settings is a local-only surface — the gateway 404s
                // it for anything but a same-machine browser. Only show the
                // button when this app is pointed at a local gateway, so a
                // remote (relay-connected) Mac client doesn't get a dead link.
                if gatewayIsLocal {
                    Button(action: { openServerSettings() }) {
                        Image(systemName: "slider.horizontal.3")
                            .foregroundStyle(.secondary)
                    }
                    .buttonStyle(.plain)
                    .help("Server settings (features, policy, models) — opens in browser")
                }

                Button(action: { showingSettings.toggle() }) {
                    Image(systemName: "gearshape")
                        .foregroundStyle(.secondary)
                }
                .buttonStyle(.plain)
                .help("App settings")
            }
            Button(action: { NSApp.terminate(nil) }) {
                Image(systemName: "xmark.circle.fill")
                    .foregroundStyle(.secondary)
            }
            .buttonStyle(.plain)
            .help("Quit")
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 10)
    }

    // gatewayIsLocal is true when the configured gateway is on this machine
    // (loopback host). Server settings is only reachable from the gateway
    // host itself, so the button is hidden otherwise.
    private var gatewayIsLocal: Bool {
        guard let host = URLComponents(string: settings.gatewayURL)?.host else { return false }
        return host == "127.0.0.1" || host == "localhost" || host == "::1"
    }

    // openServerSettings points the default browser at the gateway's web
    // settings page (/settings). That page is where shell/web-search/policy/
    // model toggles live — distinct from this app's own SettingsView, which
    // only configures the Mac client (gateway URL + token). Only invoked when
    // gatewayIsLocal, so the appended /settings resolves to a local gateway.
    private func openServerSettings() {
        var base = settings.gatewayURL.trimmingCharacters(in: .whitespaces)
        while base.hasSuffix("/") { base.removeLast() }
        guard let url = URL(string: base + "/settings") else { return }
        NSWorkspace.shared.open(url)
    }
}

// ThreadPicker is a SwiftUI Menu showing the current thread title plus a
// dropdown of every other thread. Tapping a row switches the chat;
// "New chat" creates one. Refreshes when the menu opens.
struct ThreadPicker: View {
    @EnvironmentObject var chat: ChatViewModel

    var body: some View {
        Menu {
            Section("Conversations") {
                ForEach(chat.threads) { t in
                    Button {
                        if chat.currentThread?.id != t.id {
                            Task { await chat.switchThread(t) }
                        }
                    } label: {
                        HStack {
                            Text(t.displayTitle)
                            if chat.currentThread?.id == t.id {
                                Image(systemName: "checkmark")
                            }
                        }
                    }
                }
            }
            Divider()
            Button {
                Task { await chat.createAndSwitch() }
            } label: {
                Label("New chat", systemImage: "square.and.pencil")
            }
            Button {
                Task { await chat.refreshThreads(autoSelect: false) }
            } label: {
                Label("Refresh", systemImage: "arrow.clockwise")
            }
        } label: {
            HStack(spacing: 4) {
                Text(chat.currentThread?.displayTitle ?? "Threads")
                    .font(.system(size: 12))
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
                    .truncationMode(.tail)
                Image(systemName: "chevron.down")
                    .font(.system(size: 9, weight: .semibold))
                    .foregroundStyle(.tertiary)
            }
        }
        .menuStyle(.borderlessButton)
        .fixedSize()
    }
}
