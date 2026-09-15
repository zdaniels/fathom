// swift-tools-version: 5.10
//
// Fathom macOS menu-bar app. Zero external SwiftPM dependencies — uses
// only AppKit + SwiftUI + Foundation + Speech + Security from the system
// SDK. Keeps the build trivial and the app footprint tiny.
//
// `swift build -c release` produces the binary; `make build` wraps it
// into a notarizable .app bundle (see Makefile).
//
// Supported macOS versions: 14 (Sonoma), 15 (Sequoia), 26 (Tahoe).
// .v14 is the floor — the SwiftUI APIs used (onChange/onKeyPress with
// initial:, MenuBarExtra .window style) require macOS 14+.

import PackageDescription

let package = Package(
    name: "Fathom",
    platforms: [.macOS(.v14)],
    products: [
        .executable(name: "Fathom", targets: ["Fathom"]),
    ],
    dependencies: [
        // Sparkle for auto-update. Pinned to 2.x; checking 2.6+ at build
        // time. Sparkle requires EdDSA-signed update artifacts; the
        // public key embeds in Info.plist (SUPublicEDKey) at release-
        // time. See apps/macos/README.md for the release workflow.
        .package(url: "https://github.com/sparkle-project/Sparkle", from: "2.6.0"),
    ],
    targets: [
        .executableTarget(
            name: "Fathom",
            dependencies: [
                .product(name: "Sparkle", package: "Sparkle"),
            ],
            path: "Sources/Fathom",
            resources: [],
            swiftSettings: [
                .enableUpcomingFeature("BareSlashRegexLiterals"),
                .enableUpcomingFeature("ConciseMagicFile"),
            ],
            linkerSettings: [
                // Frameworks live in Contents/Frameworks at runtime
                // (assembled by the Makefile), not next to the binary
                // in Contents/MacOS. The linker default is "search
                // alongside the binary" — we add the parent dir's
                // Frameworks/ explicitly so dyld finds Sparkle.framework
                // when the bundled app runs.
                .unsafeFlags([
                    "-Xlinker", "-rpath",
                    "-Xlinker", "@executable_path/../Frameworks",
                    "-Xlinker", "-rpath",
                    "-Xlinker", "@loader_path/Frameworks",
                ]),
            ],
        ),
    ],
)
