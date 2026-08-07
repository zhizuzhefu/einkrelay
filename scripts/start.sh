#!/bin/sh
# Explicitly enter exclusive mode and run the Guardian in the foreground.
# No bearer token is read, printed, exported, or passed as an argument.
set -eu

APP_DIR=/mnt/us/einkrelay
STATE_DIR=/var/local/einkrelay
BINARY="$APP_DIR/eink-relay"

fail() {
	printf '%s\n' "$1" >&2
	exit 1
}

[ "$(id -u)" = 0 ] || fail 'start must run as root on the Kindle'
[ -x "$BINARY" ] || fail 'EInkRelay is not installed'
[ -f "$STATE_DIR/token" ] || fail 'EInkRelay token is missing; run install first'
[ ! -L "$STATE_DIR/token" ] || fail 'EInkRelay token path is unsafe'
chmod 0600 "$STATE_DIR/token"

# resume is the only entry point that sets activity Active=true. It runs before
# guard so a fresh installation enters exclusive mode, while a deliberate exit
# remains inactive until this explicit command is invoked again.
"$BINARY" resume >/dev/null
exec "$BINARY" guard
