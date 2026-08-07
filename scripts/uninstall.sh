#!/bin/sh
# Remove only files this installer recorded.  In particular, never remove the
# shared mount or state roots: they may contain unrelated Kindle applications.
set -eu

install_dir=/mnt/us/einkrelay
state_dir=/var/local/einkrelay
kual_dir=/mnt/us/extensions/einkrelay
receipt="$state_dir/install.receipt"

fail() {
	printf '%s\n' 'eink-relay uninstall: invalid install receipt' >&2
	exit 1
}

[ "$(id -u)" = 0 ] || {
	printf '%s\n' 'uninstall must run as root on the Kindle' >&2
	exit 1
}

# A receipt is an installer-owned, regular (not symlink) text file.  It must
# contain exactly the fixed six-file list emitted by install.sh: accepting an
# arbitrary path beneath either root would turn a modified receipt into a
# deletion capability for unrelated Kindle data.
[ -f "$receipt" ] && [ ! -L "$receipt" ] || fail

# The KUAL launcher trio is optional: install.sh only writes it when KUAL is
# already on the device.  Everything else is mandatory, and any path outside
# this fixed set makes the whole receipt invalid — accepting an arbitrary path
# beneath either root would turn a modified receipt into a deletion capability
# for unrelated Kindle data.
validate_receipt() {
	seen_binary=0
	seen_fbink=0
	seen_manifest=0
	seen_font=0
	seen_start=0
	seen_stop=0
	seen_token=0
	seen_receipt=0
	seen_kual_menu=0
	seen_kual_start=0
	seen_kual_stop=0
	while IFS=' ' read -r kind path extra || [ -n "${kind:-}${path:-}${extra:-}" ]; do
		[ "$kind" = file ] && [ -z "${extra:-}" ] || return 1
		case "$path" in
			"$install_dir/eink-relay") [ "$seen_binary" -eq 0 ] || return 1; seen_binary=1 ;;
			"$install_dir/bin/fbink") [ "$seen_fbink" -eq 0 ] || return 1; seen_fbink=1 ;;
			"$install_dir/fonts/manifest.json") [ "$seen_manifest" -eq 0 ] || return 1; seen_manifest=1 ;;
			"$install_dir/fonts/NotoSansCJKsc-Regular.otf") [ "$seen_font" -eq 0 ] || return 1; seen_font=1 ;;
			"$install_dir/start.sh") [ "$seen_start" -eq 0 ] || return 1; seen_start=1 ;;
			"$install_dir/stop.sh") [ "$seen_stop" -eq 0 ] || return 1; seen_stop=1 ;;
			"$kual_dir/menu.json") [ "$seen_kual_menu" -eq 0 ] || return 1; seen_kual_menu=1 ;;
			"$kual_dir/bin/start.sh") [ "$seen_kual_start" -eq 0 ] || return 1; seen_kual_start=1 ;;
			"$kual_dir/bin/stop.sh") [ "$seen_kual_stop" -eq 0 ] || return 1; seen_kual_stop=1 ;;
			"$state_dir/token") [ "$seen_token" -eq 0 ] || return 1; seen_token=1 ;;
			"$receipt") [ "$seen_receipt" -eq 0 ] || return 1; seen_receipt=1 ;;
			*) return 1 ;;
		esac
	done < "$receipt"
	# The launcher trio is all-or-nothing: a receipt naming one or two of them
	# was not written by this installer.
	kual_count=$((seen_kual_menu + seen_kual_start + seen_kual_stop))
	[ "$kual_count" -eq 0 ] || [ "$kual_count" -eq 3 ] || return 1
	[ "$seen_binary" -eq 1 ] && [ "$seen_fbink" -eq 1 ] && [ "$seen_manifest" -eq 1 ] && [ "$seen_font" -eq 1 ] &&
		[ "$seen_start" -eq 1 ] && [ "$seen_stop" -eq 1 ] && [ "$seen_token" -eq 1 ] && [ "$seen_receipt" -eq 1 ]
}

validate_receipt || fail

while IFS=' ' read -r kind path extra || [ -n "${kind:-}${path:-}${extra:-}" ]; do
	# Receipt validation above makes this a bounded, non-recursive unlink.
	rm -f -- "$path"
done < "$receipt"
