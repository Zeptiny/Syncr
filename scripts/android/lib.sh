# shellcheck shell=bash
# Shared by scripts/android/build-worker.sh and android/spike/build-kit.sh:
# the pinned NDK, the per-ABI Go environment and the ELF checks. Source it
# from bash with `set -euo pipefail`.

# android_repo_root prints the repository root (two levels above this file).
android_repo_root() {
	(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
}

# android_die prints its arguments to stderr and exits 1.
android_die() {
	echo "error: $*" >&2
	exit 1
}

# android_ndk_version prints the NDK version pinned in scripts/android/ndk-version.
android_ndk_version() {
	local file v
	file="$(android_repo_root)/scripts/android/ndk-version"
	v=$(grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' "$file") || android_die "no NDK version line in $file"
	[ "$(printf '%s\n' "$v" | wc -l)" -eq 1 ] || android_die "more than one NDK version line in $file"
	printf '%s\n' "$v"
}

# android_find_ndk sets NDK_ROOT, NDK_BIN and READELF from ANDROID_NDK_ROOT
# or ANDROID_NDK_HOME, and fails unless that NDK is the pinned version.
android_find_ndk() {
	local want have props
	want=$(android_ndk_version)
	NDK_ROOT=${ANDROID_NDK_ROOT:-${ANDROID_NDK_HOME:-}}
	if [ -z "$NDK_ROOT" ]; then
		android_die "set ANDROID_NDK_ROOT (or ANDROID_NDK_HOME) to the Android NDK $want.
  With the Android SDK: sdkmanager \"ndk;$want\", then ANDROID_NDK_ROOT=\$ANDROID_HOME/ndk/$want.
  Or unpack the NDK for Linux from https://developer.android.com/ndk/downloads (r${want%%.*})."
	fi
	props="$NDK_ROOT/source.properties"
	[ -f "$props" ] || android_die "$NDK_ROOT is not an Android NDK (no source.properties)"
	have=$(sed -n 's/^Pkg\.Revision *= *//p' "$props" | tr -d '\r')
	[ "$have" = "$want" ] || android_die "$NDK_ROOT is NDK $have; this build is pinned to $want (scripts/android/ndk-version)"
	NDK_BIN="$NDK_ROOT/toolchains/llvm/prebuilt/linux-x86_64/bin"
	[ -d "$NDK_BIN" ] || android_die "$NDK_BIN is missing: the build needs the NDK's linux-x86_64 toolchain"
	READELF="$NDK_BIN/llvm-readelf"
	[ -x "$READELF" ] || android_die "$READELF is missing"
}

# android_select_toolchain exports GOTOOLCHAIN as the toolchain this
# repository's go.mod selects (its toolchain line), so that syncr-worker,
# rclone, restic and the kit's test binaries are all built by the same Go,
# also inside the rclone and restic modules, whose go.mod files ask for
# older ones.
android_select_toolchain() {
	GOTOOLCHAIN=$(cd "$(android_repo_root)" && env -u GOOS -u GOARCH GOWORK=off "${GO:-go}" env GOVERSION)
	export GOWORK=off GOTOOLCHAIN
	echo "Go toolchain $GOTOOLCHAIN"
}

# The Android API level the binaries are built for: minSdk 30 (Android 11).
ANDROID_API=30

# android_go_env <abi> exports GOOS, GOARCH, CGO_ENABLED and CC for abi and
# sets ABI_MACHINE (the ELF machine llvm-readelf prints) and ANDROID_ABI.
# "host" is this machine's own GOOS/GOARCH without cgo, used to prove the
# version flags on a binary that runs here.
android_go_env() {
	ANDROID_ABI=$1
	case "$ANDROID_ABI" in
	arm64-v8a)
		export GOOS=android GOARCH=arm64 CGO_ENABLED=1
		export CC="$NDK_BIN/aarch64-linux-android${ANDROID_API}-clang"
		ABI_MACHINE=AArch64
		;;
	x86_64)
		export GOOS=android GOARCH=amd64 CGO_ENABLED=1
		export CC="$NDK_BIN/x86_64-linux-android${ANDROID_API}-clang"
		ABI_MACHINE=X86-64
		;;
	host)
		GOOS=$(env -u GOOS -u GOARCH "${GO:-go}" env GOOS)
		GOARCH=$(env -u GOOS -u GOARCH "${GO:-go}" env GOARCH)
		export GOOS GOARCH CGO_ENABLED=0
		unset CC
		ABI_MACHINE=
		;;
	*)
		android_die "unknown ABI $ANDROID_ABI (arm64-v8a, x86_64 or host)"
		;;
	esac
	if [ -n "${CC:-}" ] && [ ! -x "$CC" ]; then
		android_die "$CC is missing from the NDK"
	fi
}

# Go flags shared by every Android build: no local paths or VCS stamps in
# the binaries, position-independent executables, and segments aligned to
# 16 KB so that phones with 16 KB pages (Android 15+) can load them.
ANDROID_EXTLDFLAGS='-extldflags=-Wl,-z,max-page-size=16384'
android_build_flags() {
	if [ "$ANDROID_ABI" = host ]; then
		printf '%s\n' -trimpath -buildvcs=false
	else
		printf '%s\n' -trimpath -buildvcs=false -buildmode=pie
	fi
}

# android_ldflags <extra...> prints the -ldflags value: strip, the extras,
# and for Android ABIs the NDK's linker with 16 KB alignment. External
# linking is forced: on arm64 Go links programs without cgo packages (the
# kit's small test binaries) with its own linker otherwise, which ignores
# -extldflags (it aligns to 64 KB there). android/amd64 always links
# externally.
android_ldflags() {
	local flags="-s -w"
	if [ $# -gt 0 ]; then
		flags="$flags $*"
	fi
	if [ "$ANDROID_ABI" != host ]; then
		flags="$flags -linkmode=external $ANDROID_EXTLDFLAGS"
	fi
	printf '%s\n' "$flags"
}

# android_check_elf <file> fails unless file is a position-independent
# executable (Type DYN) for ABI_MACHINE, loaded by Android's linker, whose
# LOAD segments are all aligned to at least 16 KB (0x4000). It prints one
# summary line.
android_check_elf() {
	local f=$1 header segs align min=16384 n=0 interp
	header=$("$READELF" -hW "$f") || android_die "$f is not an ELF file"
	grep -Eq '^ *Type: +DYN ' <<<"$header" || android_die "$f is not a PIE (ELF type $(sed -n 's/^ *Type: *//p' <<<"$header"))"
	grep -Eq "^ *Machine: +.*${ABI_MACHINE}" <<<"$header" || android_die "$f is not built for $ABI_MACHINE ($(sed -n 's/^ *Machine: *//p' <<<"$header"))"
	segs=$("$READELF" -lW "$f")
	interp=$(sed -n 's/.*Requesting program interpreter: \(.*\)\]$/\1/p' <<<"$segs")
	case "$interp" in
	/system/bin/linker64) ;;
	*) android_die "$f asks for the program interpreter '$interp', not /system/bin/linker64" ;;
	esac
	while read -r align; do
		n=$((n + 1))
		if [ $((align)) -lt $min ]; then
			android_die "$f has a LOAD segment aligned to $align (< 0x4000): it cannot load on phones with 16 KB pages"
		fi
	done < <(awk '$1 == "LOAD" { print $NF }' <<<"$segs")
	[ "$n" -gt 0 ] || android_die "$f has no LOAD segments"
	printf '%-14s PIE (DYN), %s, interpreter %s, %d LOAD segments aligned >= 0x4000 (%s)\n' \
		"$(basename "$f")" "$ABI_MACHINE" "$interp" "$n" \
		"$(awk '$1 == "LOAD" { print $NF }' <<<"$segs" | sort -u | paste -sd, -)"
}
