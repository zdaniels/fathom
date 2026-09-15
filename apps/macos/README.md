<p align="center">
  <img src="../../assets/logo/fathom-logo-400.png" alt="Fathom" width="180" />
</p>

# Fathom for macOS

A native menu-bar app that talks to your local Fathom gateway. Click the
iceberg in the menu bar (or hit `Cmd+Shift+Space` from anywhere) and
chat with your agent without opening a terminal.

## Status

Pre-1.0. Builds locally. Signed/notarized binary distribution comes once
the Apple Developer cert is in place — for now you build it yourself.

## Build & run

You need:
- macOS **14 (Sonoma)**, **15 (Sequoia)**, or **26 (Tahoe)** — anything 14+
- Xcode command line tools (`xcode-select --install`)
- The Fathom gateway running somewhere (`fathom start` in another terminal,
  or `fathom service install` for a persistent launchd agent)

```bash
cd apps/macos

# One-time: create a stable self-signed code-signing identity so macOS
# remembers your Microphone / Speech / Login Item permissions across
# rebuilds. Without this every rebuild gets a fresh identity hash and
# you re-grant everything.
./scripts/setup-codesign.sh

make build       # produces build/Fathom.app — auto-picks the dev cert
make run         # builds + launches
```

The first time you launch it, the API token is auto-loaded from
`~/.fantazm/api-token` (the gateway writes it there on first boot). No
manual paste needed.

## What works today (v0.4.0)

- Menu-bar icon + popover chat window
- Talks to local gateway over REST (`POST /api/v1/message`)
- API token in Keychain, gateway URL in UserDefaults
- `Cmd+Shift+Space` global hotkey to surface the chat
- Test-connection button against `/api/v1/health`
- **Native macOS notifications** for scheduled job completions (long-polls
  `/api/v1/events`; seeds the high-water mark on launch so you don't get
  spammed with old events)
- **Mic dictation** via the Speech framework — click the mic icon in the
  chat input to dictate. On-device only (`requiresOnDeviceRecognition =
  true`); audio never leaves your machine. Click again to stop; the
  partial transcript streams into the input as you speak.
- **`system_run` agent tool** (gateway-side, not app-side) — the agent
  can drive any Mac app via AppleScript. "send Sarah an iMessage" or
  "make a reminder for tomorrow at 9am" both work. Policy-gated as
  shell-exec so your `shell: deny` default catches it.

- **Auto-start at login** via SMAppService — toggle in Settings.
  Uses the modern macOS 13+ API; respects the user's System Settings →
  Login Items pane.
- **Sparkle auto-update** wired up — checks once a day for an appcast
  at https://fantazm/appcast.xml. Real updates require:
  - A `Developer ID Application` code-signing cert
  - An EdDSA key pair (generate via Sparkle's `generate_keys`)
  - `SUPublicEDKey` set in Info.plist to the EdDSA public half
  - Each released .zip signed with `sign_update` and listed in the
    appcast XML
  Until that pipeline runs, Sparkle quietly does nothing (no popups,
  no errors). See the "Release workflow" section below.

## What's planned

- Reading scheduled job replies inline in the chat history rather than
  only via the notification banner
- Multiple model picker per turn (mirroring the CLI's `/use`)

## Release workflow (real signing)

1. **One-time:** generate a Sparkle EdDSA key pair:
   ```bash
   .build/checkouts/Sparkle/bin/generate_keys
   ```
   Stores the private key in your macOS Keychain; prints the public key
   to stdout. Paste the public key into `Resources/Info.plist`'s
   `SUPublicEDKey` value. Commit that change.

2. **One-time:** get a `Developer ID Application` cert from the Apple
   Developer Program. Note the Common Name (`Developer ID Application: Your Name (TEAMID)`).

3. **Per release:**
   ```bash
   make build CODESIGN_IDENTITY="Developer ID Application: Your Name (TEAMID)"
   xcrun notarytool submit build/Fathom.app --wait \
     --apple-id you@example.com --team-id TEAMID --password APP_SPECIFIC_PWD
   xcrun stapler staple build/Fathom.app
   ditto -c -k --sequesterRsrc --keepParent build/Fathom.app Fathom-vX.Y.Z.zip
   .build/checkouts/Sparkle/bin/sign_update Fathom-vX.Y.Z.zip
   ```
   The `sign_update` output goes into the next `<item>` block in
   `appcast.xml` as `sparkle:edSignature` + the `length` attribute.

4. Upload the .zip + appcast.xml to whatever serves
   `https://fantazm/`. Existing clients pick up the update on
   their next 24-hour check (or sooner if the user clicks "Check for
   updates…" in Settings).

## Code signing

The Makefile signs with ad-hoc by default (good enough for `make run` on
your own machine). For a release build:

```bash
make build CODESIGN_IDENTITY="Developer ID Application: Your Name (TEAMID)"
xcrun notarytool submit build/Fathom.app --wait --apple-id ...
xcrun stapler staple build/Fathom.app
```

## Architecture

Zero external SwiftPM dependencies — only system frameworks (AppKit,
SwiftUI, Foundation, Security, Carbon). ~600 LOC across 8 Swift files.

```
Sources/Fathom/
  FathomApp.swift         SwiftUI App + MenuBarExtra entry
  RootView.swift          first-run vs chat branching
  ChatView.swift          turn history + input box + mic button
  ChatViewModel.swift     state, async send
  GatewayClient.swift     URLSession wrapper around the REST API
  Settings.swift          gateway URL + token (Keychain)
  SettingsView.swift      first-run setup pane + login item + updates
  HotkeyMonitor.swift     Carbon RegisterEventHotKey for ⌘⇧Space
  NotificationCenter.swift  long-polls /api/v1/events, posts UNNotifications
  Dictation.swift         AVAudioEngine + SFSpeechRecognizer (on-device only)
  LoginItem.swift         SMAppService.mainApp wrapper for auto-start
  Updater.swift           Sparkle SPUStandardUpdaterController wrapper
```
