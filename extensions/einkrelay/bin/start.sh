#!/bin/sh
# Start EInkRelay from the KUAL menu.
#
# This runs from the installed directory, not from the unpacked release: the
# package is a staging area an operator is free to delete once install.sh has
# succeeded, and a launcher that stops working when they do is not a launcher.
APP_DIR=/mnt/us/einkrelay
STATE_DIR=/var/local/einkrelay
LOG="$STATE_DIR/guardian.log"

[ -x "$APP_DIR/start.sh" ] || exit 1

# "A guardian is running" is not the same as "the panel is ours". After a REST
# or corner-tap exit the Guardian keeps supervising the service with the
# activity record set inactive, and the native interface is back. Treating that
# as already-started would make this menu entry silently do nothing, which is
# exactly how a user ends up with no way back in.
#
# Re-entering exclusive mode means restarting the Guardian, because the entry
# decision is made once at startup from the activity record. So: no-op only
# when it is genuinely already exclusive, otherwise stop what is there and
# start cleanly.
if pidof eink-relay >/dev/null 2>&1; then
	if grep -q '"active":[[:space:]]*true' "$STATE_DIR/activity.json" 2>/dev/null; then
		exit 0
	fi
	for pid in $(pidof eink-relay 2>/dev/null); do
		case "$(tr '\0' ' ' </proc/"$pid"/cmdline 2>/dev/null)" in
			*guard*) kill -TERM "$pid" 2>/dev/null || : ;;
		esac
	done
	waited=0
	while [ "$waited" -lt 20 ] && pidof eink-relay >/dev/null 2>&1; do
		sleep 1
		waited=$((waited + 1))
	done
	pidof eink-relay >/dev/null 2>&1 && exit 1
fi

# Keep the log bounded. It lives on the small root partition, and a device that
# fills it has bigger problems than a missing diagnostic.
if [ -f "$LOG" ] && [ "$(wc -c <"$LOG")" -gt 262144 ]; then
	rm -f "$LOG"
fi

# setsid detaches the Guardian from the launcher session so it survives KUAL
# returning to the home screen. Without it the Guardian is killed with the
# launcher's process group and the panel is never handed back.
setsid "$APP_DIR/start.sh" >>"$LOG" 2>&1 </dev/null &

# Report success only once the Guardian is actually serving. A bare `sleep 2;
# pidof` reports success for a process that is about to die on a bad token or a
# missing FBInk, and reports failure for a device that is merely slow to start.
# The control socket is the first thing that exists only after the Guardian has
# got past configuration, state and exclusive entry, so it is the honest
# readiness signal — and it is the same socket the stop entry needs.
# "No process" only means failure once a process has actually been seen.
# start.sh runs `eink-relay resume` and only then `exec eink-relay guard`, so
# there is a real window right after launch in which nothing called
# eink-relay exists yet. Treating that window as "it exited" made the entry
# report failure for a launch that was in fact succeeding.
waited=0
seen=0
while [ "$waited" -lt 30 ]; do
	if [ -S "$STATE_DIR/guardian.sock" ]; then
		exit 0
	fi
	if pidof eink-relay >/dev/null 2>&1; then
		seen=1
	elif [ "$seen" -eq 1 ]; then
		# It came up and then exited; the reason is in the log.
		exit 1
	fi
	sleep 1
	waited=$((waited + 1))
done
exit 1
