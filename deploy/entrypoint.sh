#!/bin/sh
set -eu
umask 077
mkdir -p "$HOME" "$XDG_CACHE_HOME"
exec fathom "$@"
