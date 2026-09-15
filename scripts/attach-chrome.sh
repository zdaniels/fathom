#!/bin/sh
# Launch the user's real Chrome with the remote-debugging port open so
# Fathom's browser skill can attach to the logged-in session.
#
# Run once per Fathom session, leave the Chrome window open, then in
# another terminal:
#
#   export FANTAZM_BROWSER_CDP_URL=http://localhost:9222
#   fathom chat
#
# Inside chat: "open my Gmail in the browser" — uses your real
# logged-in Gmail, not an empty Chromium.
#
# Why this and not "just edit the Chrome shortcut": Chrome won't open a
# remote-debugging instance if another normal Chrome is already running
# under the same user-data-dir. This script uses a SEPARATE user-data
# at ~/.fantazm/chrome-attach so it always succeeds.
#
# Honors:
#   FANTAZM_CHROME_PORT      default 9222
#   FANTAZM_CHROME_BIN       auto-detected per platform
#   FANTAZM_CHROME_PROFILE   default ~/.fantazm/chrome-attach

set -eu

PORT="${FANTAZM_CHROME_PORT:-9222}"
PROFILE="${FANTAZM_CHROME_PROFILE:-$HOME/.fantazm/chrome-attach}"

CHROME="${FANTAZM_CHROME_BIN:-}"
if [ -z "$CHROME" ]; then
  case "$(uname -s)" in
    Darwin)
      for p in "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
               "$HOME/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
               "/Applications/Brave Browser.app/Contents/MacOS/Brave Browser"; do
        [ -x "$p" ] && CHROME="$p" && break
      done ;;
    Linux)
      for p in google-chrome chrome chromium chromium-browser brave-browser; do
        if command -v "$p" >/dev/null 2>&1; then CHROME="$(command -v "$p")"; break; fi
      done ;;
  esac
fi

if [ -z "$CHROME" ] || [ ! -e "$CHROME" ]; then
  echo "attach-chrome: couldn't find Chrome/Chromium/Brave on this machine." >&2
  echo "Set FANTAZM_CHROME_BIN to the path of your browser binary." >&2
  exit 1
fi

mkdir -p "$PROFILE"

echo "Launching Chrome with remote debugging on :$PORT"
echo "  binary:  $CHROME"
echo "  profile: $PROFILE"
echo
echo "After Chrome is up, in another terminal run:"
echo
echo "  export FANTAZM_BROWSER_CDP_URL=http://localhost:$PORT"
echo "  fathom chat"
echo
echo "Press Ctrl+C in this terminal to close the attach session."
echo

exec "$CHROME" \
  --remote-debugging-port="$PORT" \
  --user-data-dir="$PROFILE" \
  --no-first-run \
  --no-default-browser-check
