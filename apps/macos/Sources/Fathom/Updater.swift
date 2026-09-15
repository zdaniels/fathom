import Foundation
import Sparkle
import SwiftUI

// Updater wraps Sparkle's SPUStandardUpdaterController so the rest of
// the app sees a tiny "are there updates?" + "check now" surface, not
// Sparkle's full delegate protocol.
//
// What Sparkle needs to actually work in production:
//
//  1. The app must be code-signed (or, for testing, signed ad-hoc + with
//     SUPublicEDKey set to "" so signature verification is bypassed —
//     never ship that to users).
//  2. SUFeedURL in Info.plist points at an appcast.xml hosted somewhere
//     stable (GitHub Pages, S3, your darklake.ai site).
//  3. Each release's .zip is signed with the EdDSA private key (via
//     `sign_update` from Sparkle's bin/ directory) and the appcast.xml
//     entry includes the resulting `sparkle:edSignature` attribute.
//  4. SUPublicEDKey in Info.plist contains the public half of the
//     EdDSA key pair.
//
// Until those are set up, Sparkle is wired but inert — calls to
// checkForUpdates() will fail gracefully with a "no feed" error. That's
// fine for dev builds; the UI just doesn't surface an "update available"
// button until releases start publishing the appcast.

@MainActor
final class Updater: ObservableObject {
    static let shared = Updater()

    private let controller: SPUStandardUpdaterController

    @Published private(set) var canCheckForUpdates = true

    private init() {
        // startingUpdater: true means Sparkle begins its background
        // check on a timer (default every 24h, configurable in the
        // app's Info.plist via SUScheduledCheckInterval). userDriver:
        // nil pulls in the standard UI driver (alert sheets etc.).
        self.controller = SPUStandardUpdaterController(
            startingUpdater: true,
            updaterDelegate: nil,
            userDriverDelegate: nil,
        )
        // SwiftUI binding for the "Check for updates…" menu / button.
        self.controller.updater.publisher(for: \.canCheckForUpdates)
            .receive(on: DispatchQueue.main)
            .assign(to: &$canCheckForUpdates)
    }

    func checkForUpdates() {
        controller.checkForUpdates(nil)
    }

    var currentFeedURL: String {
        controller.updater.feedURL?.absoluteString ?? "(no feed configured)"
    }
}
