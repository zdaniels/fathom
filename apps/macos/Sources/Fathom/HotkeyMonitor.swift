import AppKit
import Carbon.HIToolbox

// HotkeyMonitor registers a global Cmd+Shift+Space hotkey via Carbon's
// RegisterEventHotKey (the only reliable way to grab a keystroke when
// another app has focus). NSEvent.addGlobalMonitorForEvents works but
// requires Accessibility permission AND only triggers when other apps
// are key — Carbon doesn't need either.
//
// We deliberately keep the Carbon dependency local to this file so the
// rest of the app stays pure Swift/SwiftUI.

final class HotkeyMonitor {
    private var ref: EventHotKeyRef?
    private var handler: () -> Void

    init(_ handler: @escaping () -> Void) {
        self.handler = handler
        register()
    }

    deinit {
        if let ref { UnregisterEventHotKey(ref) }
    }

    private func register() {
        // Cmd+Shift+Space — kVK_Space (49), cmdKey | shiftKey.
        let hotKeyID = EventHotKeyID(signature: OSType(0x46544C4D), id: 1) // 'FTLM'
        var hotKey: EventHotKeyRef?

        let modifiers: UInt32 = UInt32(cmdKey | shiftKey)
        let keyCode: UInt32 = UInt32(kVK_Space)
        let status = RegisterEventHotKey(
            keyCode, modifiers,
            hotKeyID, GetApplicationEventTarget(), 0, &hotKey,
        )
        guard status == noErr, let hotKey else {
            // Common cause: another app has already claimed this combo.
            // We don't surface this — users can rebind in a future
            // version; first-launch failure is silent rather than
            // alarming.
            return
        }
        self.ref = hotKey

        var eventType = EventTypeSpec(eventClass: OSType(kEventClassKeyboard), eventKind: UInt32(kEventHotKeyPressed))
        InstallEventHandler(
            GetApplicationEventTarget(),
            { _, _, userData -> OSStatus in
                guard let userData else { return noErr }
                let mon = Unmanaged<HotkeyMonitor>.fromOpaque(userData).takeUnretainedValue()
                DispatchQueue.main.async { mon.handler() }
                return noErr
            },
            1, &eventType,
            Unmanaged.passUnretained(self).toOpaque(),
            nil,
        )
    }
}
