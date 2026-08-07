#!/bin/sh
# Install EInkRelay without a boot hook or a KUAL dependency. This script is
# foreground-only: starting the Guardian is an explicit action in start.sh.
set -eu

APP_DIR=/mnt/us/einkrelay
STATE_DIR=/var/local/einkrelay
RECEIPT="$STATE_DIR/install.receipt"
# KUAL is optional. When it is present its extension directory is the only
# on-device way to start EInkRelay without an SSH session, so the launcher entry
# is installed alongside the app rather than left as a manual copy step.
KUAL_EXT_ROOT=/mnt/us/extensions
KUAL_EXT_DIR="$KUAL_EXT_ROOT/einkrelay"
SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
PACKAGE_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
FBINK_SHA256=${1:-}

fail() {
	printf '%s\n' "$1" >&2
	exit 1
}

[ "$(id -u)" = 0 ] || fail 'install must run as root on the Kindle'
case "$FBINK_SHA256" in
	????????????????????????????????????????????????????????????????) ;;
	*) fail 'usage: scripts/install.sh <fbink-sha256>' ;;
esac
case "$FBINK_SHA256" in
	*[!0123456789abcdefABCDEF]*) fail 'FBInk SHA-256 must be hexadecimal' ;;
esac

[ -f "$PACKAGE_DIR/eink-relay" ] || fail 'release binary is missing'
[ -f "$PACKAGE_DIR/bin/fbink" ] || fail 'FBInk asset is missing'
[ -f "$PACKAGE_DIR/assets/fonts/manifest.json" ] || fail 'font manifest is missing'
[ -f "$PACKAGE_DIR/scripts/start.sh" ] || fail 'start.sh is missing'
[ -f "$PACKAGE_DIR/scripts/stop.sh" ] || fail 'stop.sh is missing'

# Files created from here on are private by default, including the token.
umask 077
mkdir -p "$APP_DIR/bin" "$APP_DIR/fonts" "$STATE_DIR"
chmod 0755 "$APP_DIR" "$APP_DIR/bin" "$APP_DIR/fonts"
chmod 0700 "$STATE_DIR"

cp "$PACKAGE_DIR/eink-relay" "$APP_DIR/eink-relay"
chmod 0755 "$APP_DIR/eink-relay"
cp "$PACKAGE_DIR/bin/fbink" "$APP_DIR/bin/fbink"
chmod 0755 "$APP_DIR/bin/fbink"
cp "$PACKAGE_DIR/assets/fonts/manifest.json" "$APP_DIR/fonts/manifest.json"
chmod 0644 "$APP_DIR/fonts/manifest.json"

# The installed directory has to be self-sufficient: the launcher entry below
# runs from it, and the unpacked package is a staging area an operator is free
# to delete once the install has succeeded.
cp "$PACKAGE_DIR/scripts/start.sh" "$APP_DIR/start.sh"
cp "$PACKAGE_DIR/scripts/stop.sh" "$APP_DIR/stop.sh"
chmod 0755 "$APP_DIR/start.sh" "$APP_DIR/stop.sh"

# Preflight always receives public release integrity metadata; it never falls
# back to a mere executable check and never receives a bearer credential.
EINKRELAY_FBINK_PATH="$APP_DIR/bin/fbink" "$APP_DIR/eink-relay" preflight -sha256 "$FBINK_SHA256"

# Font downloads are an explicit installation concern, never a render-path
# side effect.  A missing or tampered font therefore makes installation fail
# closed rather than producing an app that can only render images.
EINKRELAY_FONT_DIR="$APP_DIR/fonts" EINKRELAY_FONT_MANIFEST="$APP_DIR/fonts/manifest.json" \
	"$APP_DIR/eink-relay" fonts ensure

if [ -L "$STATE_DIR/token" ]; then
	fail 'token path must not be a symbolic link'
fi
if [ ! -e "$STATE_DIR/token" ]; then
	token=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
	[ "${#token}" -eq 64 ] || fail 'unable to generate token'
	# No trailing newline: the service rejects any byte outside 0x21-0x7e, so a
	# terminated file would fail token validation on every start.
	printf '%s' "$token" >"$STATE_DIR/token"
	unset token
fi
[ -f "$STATE_DIR/token" ] || fail 'token path is not a regular file'
chmod 0600 "$STATE_DIR/token"

# Install the KUAL launcher entry only when KUAL is already present.  This
# never creates the extensions root: doing so on a device without KUAL would
# leave a directory nothing owns.  EInkRelay does not depend on KUAL — this is
# a convenience entry point, and the contract boundary is unchanged (no /etc,
# no boot hook, no autostart).
kual_installed=0
if [ -d "$KUAL_EXT_ROOT" ] && [ -f "$PACKAGE_DIR/extensions/einkrelay/menu.json" ]; then
	mkdir -p "$KUAL_EXT_DIR/bin"
	chmod 0755 "$KUAL_EXT_DIR" "$KUAL_EXT_DIR/bin"
	cp "$PACKAGE_DIR/extensions/einkrelay/menu.json" "$KUAL_EXT_DIR/menu.json"
	cp "$PACKAGE_DIR/extensions/einkrelay/bin/start.sh" "$KUAL_EXT_DIR/bin/start.sh"
	cp "$PACKAGE_DIR/extensions/einkrelay/bin/stop.sh" "$KUAL_EXT_DIR/bin/stop.sh"
	chmod 0644 "$KUAL_EXT_DIR/menu.json"
	chmod 0755 "$KUAL_EXT_DIR/bin/start.sh" "$KUAL_EXT_DIR/bin/stop.sh"
	kual_installed=1
fi

# The receipt contains only fixed, installer-owned paths; it contains no token
# value or caller-controlled deletion target. The receipt records itself last.
tmp_receipt="$RECEIPT.tmp.$$"
{
	printf '%s\n' "file $APP_DIR/eink-relay"
	printf '%s\n' "file $APP_DIR/bin/fbink"
	printf '%s\n' "file $APP_DIR/fonts/manifest.json"
	printf '%s\n' "file $APP_DIR/fonts/NotoSansCJKsc-Regular.otf"
	printf '%s\n' "file $APP_DIR/start.sh"
	printf '%s\n' "file $APP_DIR/stop.sh"
	if [ "$kual_installed" -eq 1 ]; then
		printf '%s\n' "file $KUAL_EXT_DIR/menu.json"
		printf '%s\n' "file $KUAL_EXT_DIR/bin/start.sh"
		printf '%s\n' "file $KUAL_EXT_DIR/bin/stop.sh"
	fi
	printf '%s\n' "file $STATE_DIR/token"
	printf '%s\n' "file $RECEIPT"
} >"$tmp_receipt"
chmod 0600 "$tmp_receipt"
mv -f "$tmp_receipt" "$RECEIPT"

if [ "$kual_installed" -eq 1 ]; then
	printf '%s\n' 'installed; start it from the KUAL menu ("EInkRelay: 启动") or run /mnt/us/einkrelay/start.sh'
else
	printf '%s\n' 'installed; run /mnt/us/einkrelay/start.sh explicitly to start EInkRelay (KUAL not present, no launcher entry added)'
fi
