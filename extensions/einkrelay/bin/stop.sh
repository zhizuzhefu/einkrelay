#!/bin/sh
# Restore the panel and stop EInkRelay from the KUAL menu.
APP_DIR=/mnt/us/einkrelay

[ -x "$APP_DIR/stop.sh" ] || exit 1

# stop.sh is the recovery: it asks the Guardian to leave exclusive mode and
# falls back to restoring the panel directly when the Guardian is gone.
"$APP_DIR/stop.sh" || :

# Then end the Guardian itself. The pid is resolved from the command line of
# each `eink-relay` process rather than with `pgrep -f "eink-relay guard"`:
# pgrep matches on the full command line of *every* process, including the shell
# running this script, whose arguments contain that very string. Matching itself
# and then signalling the match is a real way to kill the wrong process — it
# happened during development on this device.
for pid in $(pidof eink-relay 2>/dev/null); do
	case "$(tr '\0' ' ' </proc/"$pid"/cmdline 2>/dev/null)" in
		*guard*) kill -TERM "$pid" 2>/dev/null || : ;;
	esac
done

waited=0
while [ "$waited" -lt 20 ]; do
	pidof eink-relay >/dev/null 2>&1 || exit 0
	sleep 1
	waited=$((waited + 1))
done
exit 0
