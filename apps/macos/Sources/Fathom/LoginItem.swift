import Foundation
import ServiceManagement

// LoginItem wraps SMAppService.mainApp — the modern (macOS 13+) way to
// register an app to auto-launch at login. Older apps used
// LSSharedFileList (deprecated) or wrote LaunchAgent plists by hand.
// SMAppService.mainApp does the right thing automatically: registers
// the current app bundle, persists across reboots, and respects the
// user's System Settings → Login Items toggle.
//
// Returns enum cases instead of throwing so the UI can render the
// state directly (enabled / disabled / requiresApproval).

@MainActor
final class LoginItem: ObservableObject {
    enum State: Equatable {
        case disabled
        case enabled
        case requiresApproval
        case notSupported
    }

    @Published private(set) var state: State = .disabled

    private let service: SMAppService

    init() {
        self.service = SMAppService.mainApp
        refresh()
    }

    func refresh() {
        switch service.status {
        case .enabled: state = .enabled
        case .requiresApproval: state = .requiresApproval
        case .notRegistered, .notFound: state = .disabled
        @unknown default: state = .notSupported
        }
    }

    var isEnabled: Bool {
        if case .enabled = state { return true }
        return false
    }

    func enable() {
        do {
            try service.register()
            refresh()
        } catch {
            // The most common failure here is "requires approval" — the
            // user opens System Settings → Login Items and Fathom appears
            // there waiting to be toggled on. We refresh to surface that.
            refresh()
        }
    }

    func disable() {
        do {
            try service.unregister()
            refresh()
        } catch {
            refresh()
        }
    }

    func toggle() {
        if isEnabled { disable() } else { enable() }
    }
}
