#!/bin/sh
# Stop is deliberately a recovery operation, not a process killer.  The
# Guardian owns the only complete recovery path; the small native fallback is
# retained for a crashed or unavailable Guardian.
set -u

state_dir=/var/local/einkrelay
guardian_socket="$state_dir/guardian.sock"

if [ "$(id -u)" != 0 ]; then
	printf '%s\n' 'stop must run as root on the Kindle' >&2
	exit 1
fi

# BusyBox nc supports Unix sockets on the target image.  Check the protocol
# reply rather than merely the client exit status, then leave the Guardian to
# supervise the inactive service normally.
if [ -S "$guardian_socket" ] && command -v nc >/dev/null 2>&1; then
	reply=$(printf 'EXIT\n' | nc -U "$guardian_socket" 2>/dev/null) || reply=
	if [ "$reply" = "OK" ]; then
		exit 0
	fi
fi

# A missing socket means the Guardian is gone, so recover directly.  This is
# the same recovery Lifecycle.Exit performs, in the same order, so both paths
# leave the device in the same state.
#
# Nothing is stopped or started.  An earlier version of this script stopped and
# restarted `framework` and `lab126_gui`; that is what made a failed exit
# unrecoverable, and the upstart job state it checked afterwards reported
# success regardless.  On this firmware the native interface is event driven
# (Xorg + awesome + blanket): it never learns that its pixels were overwritten,
# so the only thing that gives the panel back is one wake, which makes it
# redraw itself.
#
# `lipc-set-prop` exits 0 for a property that does not exist, so its exit status
# proves nothing and is not used as evidence here either.
status=0
lipc-set-prop com.lab126.powerd preventScreenSaver 0 >/dev/null 2>&1 || status=1
lipc-set-prop com.lab126.powerd wakeUp 1 >/dev/null 2>&1 || status=1

# Keep the persisted intent consistent with the recovered device.  Without
# this record a later Guardian start would see an old active request and could
# immediately re-enter exclusive mode after a successful direct fallback.
if [ "$status" -eq 0 ]; then
	activity_tmp="$state_dir/activity.$$.tmp"
	activity_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
	if ! (umask 077; printf '{"active":false,"failsafe":false,"reason":"rest_exit","at":"%s"}\n' "$activity_at" > "$activity_tmp") || ! mv -f "$activity_tmp" "$state_dir/activity.json"; then
		rm -f "$activity_tmp"
		exit 1
	fi
fi
exit "$status"
