#!/bin/sh
# Reproducible ARMv7 release build.
#
# The build output never lands in the repository working tree unless Git is
# already ignoring it.  A tracked build artifact has broken the delivery gate
# before, so this script fails closed rather than trusting the caller: the
# repository root itself is always refused, and any path inside the checkout
# must be confirmed ignored by `git check-ignore` before a single byte is
# written.
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
repo_root=$(CDPATH='' cd -- "$script_dir/.." && pwd -P)

binary_name=eink-relay
checksum_name=eink-relay.sha256
buildinfo_name=eink-relay.buildinfo

fail() {
	printf '%s\n' "$1" >&2
	exit 1
}

usage() {
	printf '%s\n' "usage: $0 <absolute-output-directory>" >&2
	printf '%s\n' 'the directory must be outside the repository, or a path Git ignores' >&2
	exit 2
}

[ "$#" -eq 1 ] || usage
case "$1" in
	/*) output_arg=$1 ;;
	*) usage ;;
esac

# Refuse the repository root before creating anything, so a mistaken invocation
# does not even leave an empty directory behind.
[ "$output_arg" != "$repo_root" ] ||
	fail 'the repository root must never be used as the release output directory'

# Pick the checksum tool up front.  Neither sha256sum nor shasum is universally
# present and the release workstation is not required to be Linux; failing here
# is clearer than failing later with an empty digest.
if command -v sha256sum >/dev/null 2>&1; then
	checksum_tool=sha256sum
elif command -v shasum >/dev/null 2>&1; then
	checksum_tool=shasum
elif command -v openssl >/dev/null 2>&1; then
	checksum_tool=openssl
else
	fail 'no SHA-256 tool available; install sha256sum, shasum or openssl'
fi

# Every tool reads the file on stdin, so no file name is ever parsed as an
# option and no name appears in the output that has to be stripped.
sha256_of() {
	case "$checksum_tool" in
		sha256sum) sha256sum <"$1" | cut -d' ' -f1 ;;
		shasum) shasum -a 256 <"$1" | cut -d' ' -f1 ;;
		openssl) openssl dgst -sha256 <"$1" | sed 's/.*[= ]//' ;;
	esac
}

# The ownership check below needs a resolved path, which needs the directory to
# exist.  Remember whether this invocation is the one that created it, so a
# refusal can undo it: a rejected in-repository target must not leave an empty
# directory behind in the working tree.  Only the leaf is removed, and only when
# it is still empty, so nothing that predates this run is ever touched.
directory_existed=1
[ -d "$output_arg" ] || directory_existed=0
mkdir -p -- "$output_arg"
output_dir=$(CDPATH='' cd -- "$output_arg" && pwd -P)

reject() {
	if [ "$directory_existed" -eq 0 ]; then
		rmdir -- "$output_dir" 2>/dev/null || :
	fi
	fail "$1"
}

# Re-check after symlink resolution: the argument may have pointed into the
# repository through a link.
[ "$output_dir" != "$repo_root" ] ||
	reject 'the repository root must never be used as the release output directory'

case "$output_dir" in
"$repo_root"/*)
	command -v git >/dev/null 2>&1 ||
		reject 'git is required to prove an in-repository output directory is ignored'
	for artifact in "$binary_name" "$checksum_name" "$buildinfo_name"; do
		git -C "$repo_root" check-ignore -q -- "$output_dir/$artifact" ||
			reject "$output_dir/$artifact is not ignored by Git; choose a directory outside the repository or add it to .gitignore"
	done
	;;
esac

# The normal gate proves the CJK golden baseline and performs a separate
# temporary ARMv7 build.  It deliberately requires EINKRELAY_FONT_DIR rather
# than treating a skipped golden test as release evidence.
(cd "$repo_root" && sh scripts/check.sh)

temporary_binary="$output_dir/.$binary_name.tmp.$$"
temporary_checksum="$output_dir/.$checksum_name.tmp.$$"
temporary_buildinfo="$output_dir/.$buildinfo_name.tmp.$$"
cleanup() {
	rm -f -- "$temporary_binary" "$temporary_checksum" "$temporary_buildinfo"
}
trap cleanup EXIT HUP INT TERM

# Stamp the revision into the binary so /v1/status can answer "which build is
# on this device". Without it every release reports the compile-time default,
# `dev`, and an operator looking at a misbehaving Kindle has no way to tell a
# three-week-old install from the one they just pushed. `--dirty` is part of it
# on purpose: a release built from uncommitted work should say so out loud.
if command -v git >/dev/null 2>&1 &&
	git -C "$repo_root" rev-parse --git-dir >/dev/null 2>&1; then
	stamped_version=$(git -C "$repo_root" describe --tags --always --dirty 2>/dev/null || echo dev)
else
	stamped_version=dev
fi
case "$stamped_version" in
	'' | *[!0-9A-Za-z.\-_]*) fail 'the derived version stamp is not a plain identifier' ;;
esac

# The one supported target, with the exact settings the delivery record cites.
# -trimpath removes the build directory from the binary so the same source
# produces the same bytes from any checkout path.
(
	cd "$repo_root" &&
		CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
			go build -trimpath -ldflags="-s -w -X main.version=$stamped_version" -o "$temporary_binary" ./cmd/eink-relay
)

# Rerunning the script simply replaces both files; nothing accumulates and no
# half-written artifact is ever left carrying its final name.
mv -f -- "$temporary_binary" "$output_dir/$binary_name"

digest=$(sha256_of "$output_dir/$binary_name")
case "$digest" in
	*[!0123456789abcdef]*) fail 'the computed SHA-256 is not lowercase hexadecimal' ;;
	????????????????????????????????????????????????????????????????) ;;
	*) fail 'the computed SHA-256 is not a 64-character digest' ;;
esac

# Two spaces: the format both `sha256sum -c` and `shasum -a 256 -c` read back.
printf '%s  %s\n' "$digest" "$binary_name" >"$temporary_checksum"
mv -f -- "$temporary_checksum" "$output_dir/$checksum_name"

# Verify against what is actually on disk rather than trusting the variable
# still held in memory.
recorded=$(cut -d' ' -f1 <"$output_dir/$checksum_name")
reread=$(sha256_of "$output_dir/$binary_name")
[ "$recorded" = "$digest" ] || fail 'the written checksum file does not match the built artifact'
[ "$reread" = "$digest" ] || fail 'the artifact changed between hashing and verification'

# Record what this artifact is reproducible *against*.  -trimpath removes the
# checkout path, so the same source built by the same Go toolchain with the same
# module versions produces the same bytes from any directory.  It does NOT make
# the build bit-for-bit stable across different Go toolchain versions: a
# different compiler emits different code.  A digest is therefore only
# meaningful next to the toolchain that produced it, which is what this file
# records.  `go version -m` is read back off the artifact itself rather than
# reconstructed, so it reports the module versions and build settings actually
# linked in, not the ones this script believes it asked for.
{
	printf '%s\n' "artifact: $binary_name"
	printf '%s\n' "sha256: $digest"
	printf '%s\n' "target: GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0"
	printf '%s\n' "flags: -trimpath -ldflags=-s -w -X main.version=$stamped_version"
	printf '%s\n' "version: $stamped_version"
	printf '%s\n' "toolchain: $(go version)"
	printf '%s\n' 'build settings and module versions as stamped into the artifact:'
	go version -m "$output_dir/$binary_name"
} >"$temporary_buildinfo"
mv -f -- "$temporary_buildinfo" "$output_dir/$buildinfo_name"

printf '%s\n' "release artifact: $output_dir/$binary_name"
printf '%s\n' "verified SHA-256: $digest"
printf '%s\n' "build record:     $output_dir/$buildinfo_name"
