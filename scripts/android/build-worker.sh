#!/usr/bin/env bash
# Builds syncr-worker, rclone and restic for Android (make android-worker).
#
#   scripts/android/build-worker.sh [ABI...]
#
# ABI is arm64-v8a (the default) or x86_64 (the emulator), with cgo and the
# NDK pinned in scripts/android/ndk-version (ANDROID_NDK_ROOT or
# ANDROID_NDK_HOME). The binaries land under their plain names in
# dist/android/<ABI>/ (OUT_DIR overrides dist/android). Each is checked to be
# a position-independent executable whose LOAD segments are aligned to
# 16 KB. Cgo matters: without it Go's resolver reads /etc/resolv.conf, which
# Android lacks, so DNS would fail.
#
# The ABI "host" builds the same three binaries for this machine without
# cgo (dist/android/host/) and then runs `syncr-worker tools status` against
# them, which proves that the worker's own version check accepts rclone and
# restic built this way.
#
# rclone and restic are compiled from source at exactly the versions the
# worker pins (internal/worker/tools/manifest_default.go, printed by
# `go run ./scripts/android/pins`). Their sources come through the Go module
# system, so the Go checksum database verifies them; nothing is added to
# this repository's go.mod. They get the version flags and build tags of
# their official release builds, minus self-update (and rclone's cmount).
#
# syncr-worker gets the version ldflags of .goreleaser.yaml: VERSION
# (default dev-<short commit>, like a snapshot), COMMIT and BUILD_TIME (the
# commit's date) from the environment or from git.
set -euo pipefail

# shellcheck source=scripts/android/lib.sh
. "$(dirname "$0")/lib.sh"

GO=${GO:-go}
ROOT=$(android_repo_root)
OUT_DIR=${OUT_DIR:-$ROOT/dist/android}
mkdir -p "$OUT_DIR"
OUT_DIR=$(cd "$OUT_DIR" && pwd)
VERSION_PKG=github.com/Zeptiny/Syncr/internal/platform/version
android_select_toolchain

usage() {
	awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"
}

abis=()
for arg in "$@"; do
	case "$arg" in
	-h | --help) usage; exit 0 ;;
	-*) android_die "unknown flag $arg (see --help)" ;;
	*) abis+=("$arg") ;;
	esac
done
[ ${#abis[@]} -gt 0 ] || abis=(arm64-v8a)

needs_ndk=false
for abi in "${abis[@]}"; do
	[ "$abi" = host ] || needs_ndk=true
done
if $needs_ndk; then
	android_find_ndk
	echo "NDK $(android_ndk_version) at $NDK_ROOT"
fi

# The checksum database must verify the tool sources.
check_sumdb() {
	local v
	v=$(env -u GOOS -u GOARCH "$GO" env GOSUMDB)
	[ "$v" != off ] || android_die "GOSUMDB=off: the rclone and restic sources must be verified by the Go checksum database"
	for var in GONOSUMDB GOPRIVATE GOINSECURE; do
		v=$(env -u GOOS -u GOARCH "$GO" env "$var")
		[ -z "$v" ] || android_die "$var=$v is set: it could exempt the tool sources from the Go checksum database"
	done
	case " ${GOFLAGS:-} " in
	*-insecure*) android_die "GOFLAGS contains -insecure" ;;
	esac
}

# The pinned versions, from the manifest.
declare -A PIN=()
read_pins() {
	local name ver
	while read -r name ver; do
		PIN[$name]=$ver
	done < <(cd "$ROOT" && env -u GOOS -u GOARCH -u CC CGO_ENABLED=0 "$GO" run ./scripts/android/pins)
	[ -n "${PIN[rclone]:-}" ] && [ -n "${PIN[restic]:-}" ] || android_die "the manifest pins no rclone or no restic"
	[ ${#PIN[@]} -eq 2 ] || android_die "the manifest pins tools this script does not know how to build: ${!PIN[*]}"
}

# module_dir <module>@<version> downloads the module (verified against
# go.sum.golang.org) outside this repository and prints its directory in
# the module cache.
module_dir() {
	local tmp json dir
	tmp=$(mktemp -d)
	json=$(cd "$tmp" && env -u GOOS -u GOARCH -u CC GOFLAGS= "$GO" mod download -json "$1") || {
		rmdir "$tmp"
		android_die "go mod download $1 failed"
	}
	rmdir "$tmp"
	if grep -q '"Error"' <<<"$json"; then
		android_die "go mod download $1: $(sed -n 's/^\t"Error": //p' <<<"$json")"
	fi
	dir=$(sed -n 's/^\t"Dir": "\(.*\)",$/\1/p' <<<"$json")
	[ -d "$dir" ] || android_die "go mod download $1 gave no directory"
	printf '%s\n' "$dir"
}

# build_go <src dir> <package> <output> <tags> <ldflags extra...>
build_go() {
	local dir=$1 pkg=$2 out=$3 tags=$4
	shift 4
	local flags
	mapfile -t flags < <(android_build_flags)
	(cd "$dir" && GOFLAGS=-mod=readonly "$GO" build "${flags[@]}" ${tags:+-tags "$tags"} \
		-ldflags "$(android_ldflags "$@")" -o "$out" "$pkg")
}

build_rclone() {
	local out=$1 ver=${PIN[rclone]} dir
	dir=$(module_dir "github.com/rclone/rclone@v$ver")
	# The release build sets fs.Version to the tag (rclone's Makefile and
	# bin/cross-compile.go); without it rclone reports v<x>-DEV.
	grep -q "VersionTag = \"v$ver\"" "$dir/fs/versiontag.go" ||
		android_die "rclone $ver: fs/versiontag.go does not hold v$ver; check how this release sets its version"
	echo "rclone $ver from $dir"
	build_go "$dir" . "$out/rclone" noselfupdate "-X github.com/rclone/rclone/fs.Version=v$ver"
}

build_restic() {
	local out=$1 ver=${PIN[restic]} dir
	dir=$(module_dir "github.com/restic/restic@v$ver")
	# restic's release commit holds the version as a constant; the release
	# build adds the tags selfupdate and disable_grpc_modules
	# (helpers/build-release-binaries). Self-update is left out here.
	grep -q "const Version = \"$ver\"" "$dir/internal/global/global.go" ||
		android_die "restic $ver: internal/global/global.go does not hold \"$ver\"; check how this release sets its version"
	echo "restic $ver from $dir"
	build_go "$dir" ./cmd/restic "$out/restic" disable_grpc_modules
}

worker_version() {
	if git -C "$ROOT" rev-parse --verify -q HEAD >/dev/null 2>&1; then
		COMMIT=${COMMIT:-$(git -C "$ROOT" rev-parse HEAD)}
		VERSION=${VERSION:-dev-$(git -C "$ROOT" rev-parse --short HEAD)}
		BUILD_TIME=${BUILD_TIME:-$(TZ=UTC0 git -C "$ROOT" show -s --date=format-local:%Y-%m-%dT%H:%M:%SZ --format=%cd HEAD)}
	fi
	COMMIT=${COMMIT:-unknown}
	VERSION=${VERSION:-dev}
	BUILD_TIME=${BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
}

build_worker() {
	local out=$1
	echo "syncr-worker $VERSION (commit $COMMIT, built $BUILD_TIME)"
	build_go "$ROOT" ./cmd/syncr-worker "$out/syncr-worker" "" \
		"-X $VERSION_PKG.version=$VERSION" "-X $VERSION_PKG.commit=$COMMIT" "-X $VERSION_PKG.buildTime=$BUILD_TIME"
}

# verify_host <dir> runs the worker's tool resolution against the host
# build: both tools must resolve through their explicit paths with the
# pinned versions.
verify_host() {
	local out=$1 tmp status
	tmp=$(mktemp -d)
	"$out/rclone" version | head -n 1
	"$out/restic" version
	status=$(SYNCR_RCLONE_PATH="$out/rclone" SYNCR_RESTIC_PATH="$out/restic" "$out/syncr-worker" tools status --dir "$tmp/tools") || {
		printf '%s\n' "$status"
		rm -rf "$tmp"
		android_die "syncr-worker tools status rejects the host build"
	}
	rm -rf "$tmp"
	printf '%s\n' "$status"
	for tool in rclone restic; do
		grep -Eq "^  configured +ok +$out/$tool \(${PIN[$tool]}\)$" <<<"$status" ||
			android_die "the worker does not accept $out/$tool as $tool ${PIN[$tool]}"
	done
	echo "version check: the worker accepts rclone ${PIN[rclone]} and restic ${PIN[restic]} built from source"
}

check_sumdb
read_pins
worker_version
for abi in "${abis[@]}"; do
	android_go_env "$abi"
	out="$OUT_DIR/$abi"
	mkdir -p "$out"
	echo "== $abi ($GOOS/$GOARCH, CGO_ENABLED=$CGO_ENABLED) -> $out"
	build_worker "$out"
	build_rclone "$out"
	build_restic "$out"
	if [ "$abi" = host ]; then
		verify_host "$out"
	else
		for bin in syncr-worker rclone restic; do
			android_check_elf "$out/$bin"
		done
	fi
	ls -l "$out"
done
