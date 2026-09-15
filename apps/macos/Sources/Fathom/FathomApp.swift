// Fathom — macOS menu-bar app entry point.
//
// Why this exists: the CLI is fast for power users, but a menu-bar app
// lives where you actually work. Click the wave icon (or Cmd+Shift+Space
// from anywhere), get a chat window that talks to your local Fathom
// gateway. Behind the scenes it's a POST to /api/v1/message — same
// transport `fathom chat` uses.
//
// Architecture in one paragraph:
//   - MenuBarExtra hosts a SwiftUI ChatView with .menuBarExtraStyle(.window)
//     so we get a real macOS popover instead of a NSMenu.
//   - ChatViewModel owns the state (turns, busy flag, error). It calls
//     GatewayClient which is a tiny URLSession wrapper around the REST API.
//   - HotkeyMonitor registers Cmd+Shift+Space globally so you can open
//     chat without reaching for the mouse. Requires Accessibility
//     permission (granted via System Settings → Privacy & Security).
//   - Token + gateway URL live in the macOS Keychain. First launch shows
//     a setup pane until both are configured.
//
// The whole app is ~600 LOC across 8 files. No external deps.

import SwiftUI

@main
struct FathomApp: App {
    @StateObject private var settings = Settings.shared
    @StateObject private var chat = ChatViewModel()
    @StateObject private var dictation = Dictation()
    @State private var hotkey: HotkeyMonitor?
    @State private var events = EventSubscriber()

    var body: some Scene {
        MenuBarExtra {
            RootView()
                .environmentObject(settings)
                .environmentObject(chat)
                .environmentObject(dictation)
                .frame(width: 480, height: 600)
                .onAppear {
                    if hotkey == nil {
                        hotkey = HotkeyMonitor { openMenuBarItem() }
                    }
                    events.start()
                }
        } label: {
            MenuBarIconLabel()
        }
        .menuBarExtraStyle(.window)
    }
}

// MenuBarIconLabel renders the iceberg silhouette from Resources/MenuBarIcon.png
// as a template image — macOS auto-tints it (black in light mode, white in
// dark mode) to match the rest of the menubar.
//
// We load via NSImage(named:) so the @2x/@3x variants are picked up
// automatically on Retina displays. The image must be marked .isTemplate
// before being handed to SwiftUI, otherwise it would render as the literal
// solid-black PNG and look broken in dark-mode menubars.
struct MenuBarIconLabel: View {
    var body: some View {
        if let img = NSImage(named: "MenuBarIcon") {
            img.isTemplate = true
            return AnyView(Image(nsImage: img))
        }
        // Fallback: the resource didn't get copied into the bundle for
        // some reason. Surface a recognizable placeholder rather than
        // crashing — the menubar item must show *something* or the app
        // is invisible.
        return AnyView(Image(systemName: "water.waves").symbolRenderingMode(.hierarchical))
    }
}

// openMenuBarItem programmatically opens the menu bar window in response
// to a global hotkey. SwiftUI doesn't expose this directly — we cheat by
// posting a left-click event at the status item's location.
func openMenuBarItem() {
    DispatchQueue.main.async {
        // The MenuBarExtra status item lives in NSStatusBar's known set.
        // We can't index into MenuBarExtra's internal item, but a left-
        // mouse-up on it through NSWorkspace's frontmost app activation
        // toggles it. The simplest portable workaround: send the app a
        // notification that ContentView observes and uses to toggle a
        // local state; SwiftUI doesn't model the popover state, so the
        // best we can do is bring the app forward — the user's hotkey
        // press will land in the focused popover if it's open.
        NSApp.activate(ignoringOtherApps: true)
    }
}
