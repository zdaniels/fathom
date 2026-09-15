#!/usr/bin/env sh
# Fathom installer — detects OS/arch, downloads the right binary from the
# latest GitHub release, and drops it into a writeable bin dir.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/zdaniels/fathom/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/zdaniels/fathom/main/install.sh | FANTAZM_VERSION=v0.1.0 sh
#   curl -fsSL https://raw.githubusercontent.com/zdaniels/fathom/main/install.sh | FANTAZM_INSTALL_DIR=$HOME/bin sh

set -eu

REPO="${FATHOM_REPO:-${FANTAZM_REPO:-zdaniels/fathom}}"
VERSION="${FATHOM_VERSION:-${FANTAZM_VERSION:-latest}}"
INSTALL_DIR="${FATHOM_INSTALL_DIR:-${FANTAZM_INSTALL_DIR:-${FANTAZM_BIN_DIR:-/usr/local/bin}}}"

# --- detect platform -----------------------------------------------------
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch="x86_64" ;;
  aarch64|arm64) arch="arm64" ;;
  *) printf 'Unsupported architecture: %s\n' "$arch" >&2; exit 1 ;;
esac
case "$os" in
  darwin) os="Darwin" ;;
  linux) os="Linux" ;;
  *) printf 'Unsupported OS: %s (Windows: use the .zip from GitHub Releases)\n' "$os" >&2; exit 1 ;;
esac

# --- check node (skills need it) -----------------------------------------
if ! command -v node >/dev/null 2>&1; then
  printf '\nWarning: node is not on PATH. Fathom needs Node 22+ to run installed skills.\n' >&2
  printf 'Install Node from https://nodejs.org or `brew install node`. The core agent still works without it.\n\n' >&2
fi

# --- resolve version -----------------------------------------------------
if [ "$VERSION" = "latest" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | grep '"tag_name"' | head -1 | sed -E 's/.*"([^"]+)".*/\1/')
  if [ -z "$VERSION" ]; then
    printf 'Could not resolve latest version. Set FANTAZM_VERSION explicitly.\n' >&2
    exit 1
  fi
fi

# --- download + unpack ---------------------------------------------------
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

tarball="fathom_${VERSION#v}_${os}_${arch}.tar.gz"
url="https://github.com/$REPO/releases/download/$VERSION/$tarball"

printf 'Downloading %s ...\n' "$url"
if ! curl -fsSL "$url" -o "$tmpdir/$tarball"; then
  printf 'Download failed. Browse releases: https://github.com/%s/releases\n' "$REPO" >&2
  exit 1
fi

# --- verify checksum -----------------------------------------------------
# Never run an unverified binary off the internet. goreleaser publishes a
# checksums.txt alongside the release; we fetch it, look up our tarball's
# expected SHA-256, and refuse to install on any mismatch (or if we can't
# verify at all). FANTAZM_SKIP_CHECKSUM=1 is an explicit, documented escape
# hatch for air-gapped/mirror setups — but it is opt-in, never the default.
#
# Two layers:
#   INTEGRITY    — the SHA-256 below proves the tarball matches checksums.txt,
#                  defeating CDN/MITM tampering of the tarball in transit.
#   AUTHENTICITY — checksums.txt is signed (cosign keyless, Sigstore) by the
#                  release workflow. When cosign is installed we verify that
#                  signature FIRST, so checksums.txt itself is trusted before
#                  we compare against it.
#
# IMPORTANT — fail closed: if cosign is installed, a MISSING signature is fatal,
# not a downgrade to integrity-only. Otherwise the very attacker this defends
# against (release-asset replacement) could simply delete the .sig and skip the
# authenticity check. Releases published before signing won't have a sig — those
# install with FANTAZM_ALLOW_UNSIGNED=1 (explicit, documented opt-out).
#
# The identity is pinned to the exact signing workflow + a version tag and
# anchored (^…$), so only this repo's release.yml on a v* tag is trusted — not
# any other workflow in the repo that happens to have id-token: write.
base_url="https://github.com/$REPO/releases/download/$VERSION"
checksum_url="$base_url/checksums.txt"
cosign_identity='^https://github\.com/zdaniels/fathom/\.github/workflows/release\.yml@refs/tags/v.*$'
cosign_issuer='https://token.actions.githubusercontent.com'
if [ "${FANTAZM_SKIP_CHECKSUM:-0}" = "1" ]; then
  printf 'WARNING: FANTAZM_SKIP_CHECKSUM=1 set — skipping integrity verification.\n' >&2
elif ! curl -fsSL "$checksum_url" -o "$tmpdir/checksums.txt"; then
  printf 'Could not download checksums.txt to verify the binary. Aborting.\n' >&2
  printf 'Set FANTAZM_SKIP_CHECKSUM=1 to bypass at your own risk.\n' >&2
  exit 1
else
  # --- authenticity: verify the cosign signature over checksums.txt --------
  if command -v cosign >/dev/null 2>&1; then
    if curl -fsSL "$base_url/checksums.txt.sig" -o "$tmpdir/checksums.txt.sig" &&
       curl -fsSL "$base_url/checksums.txt.pem" -o "$tmpdir/checksums.txt.pem"; then
      if cosign_out=$(cosign verify-blob \
            --certificate "$tmpdir/checksums.txt.pem" \
            --signature "$tmpdir/checksums.txt.sig" \
            --certificate-identity-regexp "$cosign_identity" \
            --certificate-oidc-issuer "$cosign_issuer" \
            "$tmpdir/checksums.txt" 2>&1); then
        printf 'Signature verified (cosign) — checksums.txt is authentic.\n'
      else
        printf 'cosign signature verification FAILED for checksums.txt — aborting.\n' >&2
        printf '%s\n' "$cosign_out" >&2
        printf 'The release may be tampered with.\n' >&2
        exit 1
      fi
    elif [ "${FANTAZM_ALLOW_UNSIGNED:-0}" = "1" ]; then
      printf 'WARNING: no signature found for %s and FANTAZM_ALLOW_UNSIGNED=1 — integrity-only.\n' "$VERSION" >&2
    else
      printf 'No cosign signature found for %s, but cosign is installed — refusing to install.\n' "$VERSION" >&2
      printf 'A missing signature can mean the release predates signing OR that an attacker stripped it.\n' >&2
      printf 'If this release predates signing, set FANTAZM_ALLOW_UNSIGNED=1 to install with integrity-only.\n' >&2
      exit 1
    fi
  else
    printf '*** NOTE: cosign not installed — verifying INTEGRITY ONLY (no authenticity check). ***\n' >&2
    printf '          A release-asset-replacement attacker is NOT defended against without cosign.\n' >&2
    printf '          Install cosign (https://docs.sigstore.dev) for tamper-evident releases.\n' >&2
  fi

  expected=$(grep " $tarball\$" "$tmpdir/checksums.txt" | awk '{print $1}' | head -1)
  if [ -z "$expected" ]; then
    printf 'No checksum entry for %s in checksums.txt. Aborting.\n' "$tarball" >&2
    exit 1
  fi
  if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "$tmpdir/$tarball" | awk '{print $1}')
  elif command -v shasum >/dev/null 2>&1; then
    actual=$(shasum -a 256 "$tmpdir/$tarball" | awk '{print $1}')
  else
    printf 'Neither sha256sum nor shasum found; cannot verify integrity. Aborting.\n' >&2
    printf 'Set FANTAZM_SKIP_CHECKSUM=1 to bypass at your own risk.\n' >&2
    exit 1
  fi
  if [ "$expected" != "$actual" ]; then
    printf 'Checksum mismatch for %s!\n  expected: %s\n  actual:   %s\nAborting — the download may be corrupt or tampered with.\n' \
      "$tarball" "$expected" "$actual" >&2
    exit 1
  fi
  printf 'Checksum verified (sha256: %s)\n' "$actual"
fi

tar -xzf "$tmpdir/$tarball" -C "$tmpdir"

# --- install -------------------------------------------------------------
if [ ! -w "$INSTALL_DIR" ]; then
  printf '\nInstall dir %s is not writable; using sudo.\n' "$INSTALL_DIR"
  sudo mv "$tmpdir/fathom" "$INSTALL_DIR/fathom"
  sudo chmod 0755 "$INSTALL_DIR/fathom"
else
  mv "$tmpdir/fathom" "$INSTALL_DIR/fathom"
  chmod 0755 "$INSTALL_DIR/fathom"
fi

printf '\nInstalled fathom %s to %s\n' "$VERSION" "$INSTALL_DIR/fathom"
printf '\nNext:\n  fathom init     # set up\n  fathom chat     # talk to your agent\n\n'
