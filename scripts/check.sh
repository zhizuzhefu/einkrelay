#!/bin/sh
set -eu

verification_dir="$(mktemp -d)"
trap 'rm -rf "$verification_dir"' EXIT HUP INT TERM

if [ -z "${EINKRELAY_FONT_DIR:-}" ]; then
	printf '%s\n' 'EINKRELAY_FONT_DIR must point to the manifest-pinned font for CJK golden verification' >&2
	exit 1
fi

cjk_log="$verification_dir/cjk.log"
go test -count=1 -run '^TestGoldenCJKMixedTypesetting$' -v ./cmd/eink-relay 2>&1 | tee "$cjk_log"
# The banner below is a claim about a specific test, so it is only printed once
# that test has been observed passing. A bare exit status would also be 0 when
# the pattern matches nothing ("no tests to run"), which would announce CJK
# evidence that was never produced.
if ! grep -q '^--- PASS: TestGoldenCJKMixedTypesetting' "$cjk_log"; then
	printf '%s\n' 'TestGoldenCJKMixedTypesetting did not report an explicit PASS' >&2
	exit 1
fi
printf '%s\n' 'CJK golden verification PASS: pinned font frame matched the committed SHA-256 baseline'

go vet ./...
go test -count=1 ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -o "$verification_dir/eink-relay" ./cmd/eink-relay

# The four device scripts are the trusted install/recovery boundary and are
# never exercised by `go test` as scripts.  A parse check is cheap and catches
# the class of mistake that would otherwise only surface on the Kindle, where
# a broken recovery script is exactly the thing that cannot be debugged.
for device_script in scripts/install.sh scripts/start.sh scripts/stop.sh scripts/uninstall.sh; do
	sh -n "$device_script" || {
		printf '%s\n' "$device_script is not valid POSIX shell" >&2
		exit 1
	}
done

printf '%s\n' 'local tests, shell parse checks and linux/armv7 cross-build passed; no hardware acceptance was performed'
