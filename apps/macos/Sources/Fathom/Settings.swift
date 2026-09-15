import Foundation
import SwiftUI

// Settings is the persistent app config: gateway URL + API token. URL is
// in UserDefaults (not secret), token is in the macOS Keychain. Singleton
// because there's only one app-wide config; @Published surfaces changes
// into SwiftUI views.

@MainActor
final class Settings: ObservableObject {
    static let shared = Settings()

    @Published var gatewayURL: String {
        didSet { UserDefaults.standard.set(gatewayURL, forKey: "gatewayURL") }
    }
    @Published private(set) var hasToken: Bool

    var isConfigured: Bool { hasToken && !gatewayURL.isEmpty }

    private init() {
        self.gatewayURL = UserDefaults.standard.string(forKey: "gatewayURL")
            ?? "http://127.0.0.1:8790"
        // Token storage strategy: the source of truth is the file at
        // ~/.fathom/api-token (or $FANTAZM_TOKEN_FILE). The gateway
        // writes it on first boot; we read it every time.
        //
        // We DO NOT cache the token in Keychain. The previous design
        // wrote to Keychain on first read so subsequent reads were
        // fast — but on dev builds (ad-hoc-signed or even self-signed
        // with a new signature each rebuild), Keychain's ACL flags
        // each new binary as a different app and pops a password
        // prompt every time. Reading from a 0600 file has no such
        // re-prompt behavior and is plenty fast.
        self.hasToken = Self.readBootstrapToken() != nil
    }

    var token: String? {
        Self.readBootstrapToken()
    }

    func setToken(_ value: String) {
        // Used only from the manual setup pane (overriding the bootstrap
        // file location). Writes to a user-owned file in the same dir
        // the bootstrap file lives in, so subsequent reads pick it up.
        Self.writeBootstrapToken(value)
        hasToken = true
        objectWillChange.send()
    }

    func clearToken() {
        Self.clearBootstrapToken()
        hasToken = false
        objectWillChange.send()
    }

    private static func tokenFilePath() -> String {
        if let override = ProcessInfo.processInfo.environment["FANTAZM_TOKEN_FILE"], !override.isEmpty {
            return override
        }
        return (NSHomeDirectory() as NSString).appendingPathComponent(".fathom/api-token")
    }

    private static func writeBootstrapToken(_ value: String) {
        let path = tokenFilePath()
        let dir = (path as NSString).deletingLastPathComponent
        try? FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        let data = value.data(using: .utf8) ?? Data()
        try? data.write(to: URL(fileURLWithPath: path))
        // Owner-only.
        try? FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: path)
    }

    private static func clearBootstrapToken() {
        try? FileManager.default.removeItem(atPath: tokenFilePath())
    }

    // readBootstrapToken reads the contents of ~/.fathom/api-token
    // (or $FANTAZM_TOKEN_FILE if set), trimmed. Returns nil if missing
    // or empty. The gateway writes this on first-boot token generation.
    static func readBootstrapToken() -> String? {
        let path: String
        if let override = ProcessInfo.processInfo.environment["FANTAZM_TOKEN_FILE"], !override.isEmpty {
            path = override
        } else {
            path = (NSHomeDirectory() as NSString).appendingPathComponent(".fathom/api-token")
        }
        guard let data = try? Data(contentsOf: URL(fileURLWithPath: path)) else { return nil }
        let s = String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
        return s.isEmpty ? nil : s
    }
}

// Keychain wrapper retired — we now store the token as a 0600 file at
// ~/.fathom/api-token. The Keychain ACL system flags every code-signing
// identity change as a different app, popping a password prompt on each
// dev rebuild. The file approach matches how the CLI's vault master.key
// works and gives the same realistic security for personal mode
// (filesystem perms, owner-only).
//
// Left as a private no-op enum so any historical references compile but
// also so the file's structure stays familiar.
private enum Keychain {
    static let service = "ai.darklake.fathom"

    static func read(account: String) -> String? { nil }
    static func write(account: String, value: String) {}
    static func delete(account: String) {}
}
