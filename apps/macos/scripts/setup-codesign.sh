#!/bin/sh
# Create a self-signed code-signing identity for stable dev builds.
#
# Why: macOS TCC (Privacy & Security) keys permissions on the code-signing
# identity hash. Ad-hoc signed apps get a different hash every rebuild,
# so each `make build` invalidates mic / speech / accessibility / login-
# item permissions and re-prompts. A self-signed cert with a stable name
# fixes that — the cert stays in your login Keychain, the signature is
# deterministic, and TCC remembers permissions across rebuilds.
#
# Why this calls into the GUI: macOS's `security` CLI has been unreliable
# for years when it comes to importing key+cert pairs and forming code-
# signing identities. The PKCS12 path silently drops the key on some
# OS versions, the separate-import path doesn't auto-pair them on others,
# and Apple has never published a stable scripted path that survives a
# macOS major-version bump. Apple's own docs route you through Keychain
# Access's Certificate Assistant — so we do too, but pre-fill the form
# and tell you the four buttons to click.

set -eu

IDENTITY_NAME="${FANTAZM_CODESIGN_NAME:-Fathom Dev}"

# Already installed?
if security find-identity -p codesigning -v 2>/dev/null | grep -q "$IDENTITY_NAME"; then
  echo "Already have a \"$IDENTITY_NAME\" code-signing identity. You're set."
  echo
  echo "Build with:  make build CODESIGN_IDENTITY=\"$IDENTITY_NAME\""
  echo "(The Makefile auto-detects it, so plain \`make build\` also works.)"
  exit 0
fi

cat <<EOF
You don't have a "$IDENTITY_NAME" code-signing identity yet.

The reliable way to create one is through Keychain Access's Certificate
Assistant. The script will open it for you in a moment. Click through
this exact sequence:

  1.  Menu bar: Keychain Access → Certificate Assistant
                → Create a Certificate…
  2.  Name:               $IDENTITY_NAME
  3.  Identity Type:      Self Signed Root
  4.  Certificate Type:   Code Signing
  5.  ☐ Let me override defaults  (leave UNCHECKED)
  6.  Click "Create" → "Continue" past the security warning →
      "Done"

That's it. The cert + key land in your login Keychain as a matched pair
that \`security find-identity -p codesigning\` will recognize.

EOF

read -p "Press Enter to open Keychain Access (or Ctrl+C to skip)…" _

# Open the Certificate Assistant directly. The application name is
# slightly different across macOS versions; we try a few.
open -a "Keychain Access" 2>/dev/null || true

cat <<EOF

After you finish the wizard, run this script again to verify:

  ./scripts/setup-codesign.sh

Or skip the verify and just build:

  make build

The Makefile picks the identity up automatically once it's in your
Keychain.
EOF
