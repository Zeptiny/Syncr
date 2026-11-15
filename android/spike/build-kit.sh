#!/usr/bin/env bash
# Assembles the Android spike's phone test kit (step S; temporary: removed
# before the Android PR leaves draft) into dist/android/spike/:
#
#   bin/       syncr-worker, rclone, restic (make android-worker) and gofacts
#   tests/     go test -c binaries of self-contained worker packages, with
#              the integration tests (-tags integration), and the testdata
#              they read under tests/tree/ (the repository's layout)
#   termux-spike.sh, README.md
#   SHA256SUMS of every file above
#
#   android/spike/build-kit.sh [ABI]
#
# ABI is arm64-v8a (the default) or x86_64; "host" builds a kit for this
# machine, to try termux-spike.sh here. Missing binaries in
# dist/android/<ABI>/ are built first with scripts/android/build-worker.sh.
# KIT_DIR overrides dist/android/spike.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=scripts/android/lib.sh
. "$HERE/../../scripts/android/lib.sh"

GO=${GO:-go}
ROOT=$(android_repo_root)
ABI=${1:-arm64-v8a}
KIT_DIR=${KIT_DIR:-$ROOT/dist/android/spike}
BIN_SRC=$ROOT/dist/android/$ABI

# The worker packages whose tests run on the phone: "<name> <package dir>".
# They need no network; the integration tests among them need only
# SYNCR_TEST_RCLONE, SYNCR_TEST_RESTIC and local folders (t.TempDir, which
# follows Termux's TMPDIR). Tests that cannot run on a phone are skipped by
# termux-spike.sh (test_skip, with the reasons): the MinIO and SFTP tests
# (Docker) and one restic test whose fake restic is a /bin/sh script. Mount
# tests skip themselves without a usable /dev/fuse.
TEST_PACKAGES=(
	"proc internal/worker/proc"
	"localfs internal/worker/localfs"
	"mount internal/worker/mount"
	"datadir internal/worker/datadir"
	"atomicfile internal/worker/atomicfile"
	"rclone-runner internal/worker/runner/rclone"
	"restic-runner internal/worker/runner/restic"
)
# Files outside a package's testdata that its tests read, relative to the
# repository root (the selection test reads the web's golden filter file).
EXTRA_TEST_FILES=(web/src/lib/rclone-filters.golden.json)

if [ ! -x "$BIN_SRC/syncr-worker" ] || [ ! -x "$BIN_SRC/rclone" ] || [ ! -x "$BIN_SRC/restic" ]; then
	"$ROOT/scripts/android/build-worker.sh" "$ABI"
fi

if [ "$ABI" != host ]; then
	android_find_ndk
fi
android_select_toolchain
android_go_env "$ABI"
mapfile -t BUILD_FLAGS < <(android_build_flags)
LDFLAGS=$(android_ldflags)

case "$KIT_DIR" in
"" | /) android_die "KIT_DIR must name a kit directory" ;;
esac
rm -rf "$KIT_DIR"
mkdir -p "$KIT_DIR/bin" "$KIT_DIR/tests/tree"
echo "== kit for $ABI ($GOOS/$GOARCH) -> $KIT_DIR"

cp "$BIN_SRC/syncr-worker" "$BIN_SRC/rclone" "$BIN_SRC/restic" "$KIT_DIR/bin/"
(cd "$ROOT" && "$GO" build "${BUILD_FLAGS[@]}" -ldflags "$LDFLAGS" -o "$KIT_DIR/bin/gofacts" ./android/spike/gofacts)

: >"$KIT_DIR/tests/index.txt"
for entry in "${TEST_PACKAGES[@]}"; do
	read -r name dir <<<"$entry"
	echo "test binary $name ($dir)"
	(cd "$ROOT" && "$GO" test -c -tags integration "${BUILD_FLAGS[@]}" -ldflags "$LDFLAGS" \
		-o "$KIT_DIR/tests/$name.test" "./$dir")
	mkdir -p "$KIT_DIR/tests/tree/$dir"
	if [ -d "$ROOT/$dir/testdata" ]; then
		cp -R "$ROOT/$dir/testdata" "$KIT_DIR/tests/tree/$dir/"
	fi
	printf '%s %s\n' "$name" "$dir" >>"$KIT_DIR/tests/index.txt"
done
for f in "${EXTRA_TEST_FILES[@]}"; do
	mkdir -p "$KIT_DIR/tests/tree/$(dirname "$f")"
	cp "$ROOT/$f" "$KIT_DIR/tests/tree/$f"
done

if [ "$ABI" != host ]; then
	for f in "$KIT_DIR"/bin/* "$KIT_DIR"/tests/*.test; do
		android_check_elf "$f"
	done
fi

cp "$HERE/termux-spike.sh" "$HERE/README.md" "$KIT_DIR/"
chmod 0755 "$KIT_DIR/termux-spike.sh" "$KIT_DIR"/bin/* "$KIT_DIR"/tests/*.test
(cd "$KIT_DIR" && find . -type f ! -path ./SHA256SUMS | sed 's|^\./||' | LC_ALL=C sort |
	xargs -d '\n' sha256sum -- >SHA256SUMS)

echo "kit contents:"
(cd "$KIT_DIR" && du -sh . && ls -l bin tests)
