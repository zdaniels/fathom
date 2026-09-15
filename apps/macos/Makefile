# Fathom macOS app build.
#
# Targets:
#   make build   — compile the Swift binary AND wrap it in a .app bundle
#                  at build/Fathom.app. Run it with `open build/Fathom.app`.
#   make run     — build + open.
#   make clean   — wipe build/.
#   make sign    — codesign the .app bundle (requires Apple Developer cert
#                  in your keychain). Set CODESIGN_IDENTITY to the cert's
#                  Common Name; falls back to ad-hoc signing for local dev.
#
# The SwiftPM build produces .build/release/Fathom which is just the Mach-O
# binary; macOS needs a .app bundle (a directory with Contents/MacOS/
# binary + Contents/Info.plist) to launch as a proper menu-bar app.

APP_NAME   := Fathom
BUNDLE_ID  := ai.fantazm.fantazm
BUILD_DIR  := build
APP_BUNDLE := $(BUILD_DIR)/$(APP_NAME).app

# Pick a stable code-signing identity when one is installed in the user's
# Keychain — that's what makes TCC (mic, speech, accessibility, login-
# item) permissions persist across rebuilds.
#
# Why we DON'T use `-p codesigning -v`: that filter only returns identities
# whose certs are trusted in the system trust store. A self-signed "Fathom
# Dev" cert is NOT in the trust store (and shouldn't be — it's a dev cert,
# not a root CA), so it's filtered out. But `codesign` itself happily uses
# untrusted self-signed identities when you name them explicitly. We just
# need to find the cert's name in the keychain.
DEV_IDENTITY := $(shell security find-identity 2>/dev/null | sed -nE 's/.*"(Fathom Dev[^"]*)".*/\1/p' | head -1)
ifeq ($(CODESIGN_IDENTITY),)
  ifneq ($(DEV_IDENTITY),)
    CODESIGN_IDENTITY := $(DEV_IDENTITY)
  else
    CODESIGN_IDENTITY := -
  endif
endif

.PHONY: build run clean sign info

build: $(APP_BUNDLE)
	@echo
	@echo "  Built $(APP_BUNDLE)"
	@echo "  Run with: open $(APP_BUNDLE)"
	@echo

$(APP_BUNDLE): $(shell find Sources Resources -type f)
	@echo "→ swift build -c release"
	@swift build -c release --product $(APP_NAME)
	@echo "→ assembling .app bundle"
	@rm -rf "$(APP_BUNDLE)"
	@mkdir -p "$(APP_BUNDLE)/Contents/MacOS"
	@mkdir -p "$(APP_BUNDLE)/Contents/Resources"
	@mkdir -p "$(APP_BUNDLE)/Contents/Frameworks"
	@cp Resources/Info.plist "$(APP_BUNDLE)/Contents/Info.plist"
	@cp .build/release/$(APP_NAME) "$(APP_BUNDLE)/Contents/MacOS/$(APP_NAME)"
	@plutil -convert binary1 "$(APP_BUNDLE)/Contents/Info.plist"
	@# Brand assets: dock/finder icon + menubar template icon.
	@# The menubar PNGs are named so NSImage(named:) auto-resolves @2x/@3x
	@# on Retina displays. The .icns is what Info.plist's CFBundleIconFile
	@# points at (rendered in About panel, Spotlight, Finder).
	@cp Resources/AppIcon.icns "$(APP_BUNDLE)/Contents/Resources/AppIcon.icns"
	@cp Resources/MenuBarIcon.png    "$(APP_BUNDLE)/Contents/Resources/MenuBarIcon.png"
	@cp Resources/MenuBarIcon@2x.png "$(APP_BUNDLE)/Contents/Resources/MenuBarIcon@2x.png"
	@cp Resources/MenuBarIcon@3x.png "$(APP_BUNDLE)/Contents/Resources/MenuBarIcon@3x.png"
	@# AppMark — full-color iceberg medallion used inside the popover
	@# (RootView header). Different from MenuBarIcon, which is a black
	@# template silhouette tinted by macOS for the menubar.
	@cp Resources/AppMark.png "$(APP_BUNDLE)/Contents/Resources/AppMark.png"
	@# Sparkle ships as an xcframework — copy the macOS slice into the
	@# app's Frameworks/. The XPC services (Autoupdate / Downloader /
	@# Installer) live inside Sparkle.framework/XPCServices and need to
	@# come along for autoupdate to function. Plain `cp -R` is fine.
	@SPARKLE_FRAMEWORK=$$(find .build/artifacts -type d -name "Sparkle.framework" -path "*/macos-*" | head -1); \
	if [ -n "$$SPARKLE_FRAMEWORK" ]; then \
		echo "→ copying Sparkle.framework from $$SPARKLE_FRAMEWORK"; \
		cp -R "$$SPARKLE_FRAMEWORK" "$(APP_BUNDLE)/Contents/Frameworks/"; \
	else \
		echo "  (Sparkle.framework not found — autoupdate disabled)"; \
	fi
	@echo "→ codesigning ($(CODESIGN_IDENTITY))"
	@# Sign the framework's own components first (XPC services + Sparkle
	@# itself), then the outer .app. --deep alone misses XPC services in
	@# some Xcode versions, so we sign in dependency order explicitly.
	@if [ -d "$(APP_BUNDLE)/Contents/Frameworks/Sparkle.framework" ]; then \
		for xpc in "$(APP_BUNDLE)/Contents/Frameworks/Sparkle.framework/Versions/Current/XPCServices/"*.xpc; do \
			[ -d "$$xpc" ] && codesign --force --sign "$(CODESIGN_IDENTITY)" "$$xpc" 2>/dev/null; \
		done; \
		codesign --force --sign "$(CODESIGN_IDENTITY)" "$(APP_BUNDLE)/Contents/Frameworks/Sparkle.framework" 2>/dev/null; \
	fi
	@codesign --force --deep --sign "$(CODESIGN_IDENTITY)" "$(APP_BUNDLE)"

run: build
	@open "$(APP_BUNDLE)"

clean:
	rm -rf "$(BUILD_DIR)" .build

# For real release builds: get an "Developer ID Application" cert from
# the Apple Developer Program, then:
#   make build CODESIGN_IDENTITY="Developer ID Application: Your Name (TEAMID)"
#   make notarize  # not implemented — use Apple's `notarytool` directly

info:
	@echo "App bundle:  $(APP_BUNDLE)"
	@echo "Bundle ID:   $(BUNDLE_ID)"
	@echo "Identity:    $(CODESIGN_IDENTITY)"
