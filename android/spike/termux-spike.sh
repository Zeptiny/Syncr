#!/usr/bin/env bash
# Syncr Android spike: runs the real syncr-worker, rclone and restic on this
# phone in Termux and writes what it finds to one plain-text report,
# $HOME/syncr-spike/report.txt (also copied to the phone's Download folder).
# The report never holds a secret: no enrollment token, no worker secret, no
# restic password and no config file contents.
#
#   ./termux-spike.sh all           facts + tools + storage (about 20-40 min)
#   ./termux-spike.sh facts         Android, device, kernel, page size, phantom-process settings
#   ./termux-spike.sh tools         checksums, versions, DNS/TLS, the worker's own tests
#   ./termux-spike.sh storage       restic speed and change detection on shared storage
#   ./termux-spike.sh long-start    start a 10 min job, then lock the screen for 15 min
#   ./termux-spike.sh long-check    what happened to that job
#   ./termux-spike.sh worker        enroll a worker with your controller and start it
#   ./termux-spike.sh worker-check  summary of the worker's log
#   ./termux-spike.sh worker-stop   stop the worker
#   ./termux-spike.sh cleanup       remove everything this kit created
#
# Settings (environment): SPIKE_DATA_MB (300) and SPIKE_FILES (2000) size the
# synthetic data; SPIKE_LONG_MIN (10) is the long job's length;
# SPIKE_NO_WAKELOCK=1 runs the long job without a Termux wake lock;
# SPIKE_SHARED (/storage/emulated/0) is the shared storage root and
# SPIKE_HOME ($HOME/syncr-spike) the kit's working folder.
set -uo pipefail
umask 077

KIT=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
BIN=$KIT/bin
TESTS=$KIT/tests
WORK=${SPIKE_HOME:-$HOME/syncr-spike}
REPORT=$WORK/report.txt
SHARED=${SPIKE_SHARED:-/storage/emulated/0}
SPIKE_DIR=$SHARED/SyncrSpike
DATA_MB=${SPIKE_DATA_MB:-300}
FILES=${SPIKE_FILES:-2000}
LONG_MIN=${SPIKE_LONG_MIN:-10}
LONG=$WORK/long
WDIR=$WORK/worker

# prepare creates the working folder and makes the kit's programs
# executable (unzipping loses the permission).
prepare() {
	mkdir -p "$WORK/logs"
	chmod 0755 "$BIN"/* "$TESTS"/*.test 2>/dev/null
}

# --- output -------------------------------------------------------------

rec() { printf '%s\n' "$*" >>"$REPORT"; }
say() {
	printf '%s\n' "$*"
	rec "$*"
}
section() {
	say ""
	say "== $1 ($(date -u +%Y-%m-%dT%H:%M:%SZ))"
}
have() { command -v "$1" >/dev/null 2>&1; }

# capture <label> <command...> runs a command and records its exit code and
# its first 40 output lines (indented) in the report and on the screen.
capture() {
	local label=$1 out rc
	shift
	out=$("$@" 2>&1)
	rc=$?
	say "\$ $label (exit $rc)"
	if [ -n "$out" ]; then
		printf '%s\n' "$out" | head -n 40 | sed 's/^/    /' | tee -a "$REPORT"
	fi
	return $rc
}

# with_timeout <seconds> <command...>: timeout(1) where Termux has it.
with_timeout() {
	local s=$1
	shift
	if have timeout; then
		timeout "$s" "$@"
	else
		"$@"
	fi
}

now() {
	local t
	t=$(date +%s.%N)
	case "$t" in
	*N) date +%s ;;
	*) printf '%s\n' "$t" ;;
	esac
}
# secs <start> <end> prints the difference with one decimal.
secs() { awk -v a="$1" -v b="$2" 'BEGIN { printf "%.2f", b - a }'; }
# rate <bytes> <start> <end> prints MB/s.
rate() { awk -v n="$1" -v a="$2" -v b="$3" 'BEGIN { s = b - a; if (s <= 0) s = 0.001; printf "%.1f MB/s", n / 1048576 / s }'; }

# publish copies the report to the phone's Download folder when shared
# storage is set up.
publish() {
	if [ -d "$SHARED/Download" ] && [ -w "$SHARED/Download" ]; then
		if cp "$REPORT" "$SHARED/Download/syncr-spike-report.txt" 2>/dev/null; then
			echo "Report: $REPORT (copy: $SHARED/Download/syncr-spike-report.txt)"
			return
		fi
	fi
	echo "Report: $REPORT"
}

require_kit() {
	local missing=0
	for f in syncr-worker rclone restic gofacts; do
		[ -x "$BIN/$f" ] || {
			echo "missing $BIN/$f: unzip the whole kit, then run: chmod +x $KIT/termux-spike.sh" >&2
			missing=1
		}
	done
	[ $missing -eq 0 ] || exit 1
}

shared_ready() {
	ls "$SHARED" >/dev/null 2>&1 && touch "$SHARED/.syncr-spike-probe" 2>/dev/null && rm -f "$SHARED/.syncr-spike-probe"
}

require_shared() {
	if ! shared_ready; then
		say "Shared storage ($SHARED) is not writable from Termux."
		say "Run: termux-setup-storage   then allow the storage permission, and start this command again."
		exit 1
	fi
}

# sys_cmd <name> prints how to run an Android system command: from PATH
# (Termux wraps some), else from /system/bin; it fails when neither has it.
sys_cmd() {
	if have "$1"; then
		command -v "$1"
	elif [ -x "/system/bin/$1" ]; then
		echo "/system/bin/$1"
	else
		return 1
	fi
}

prop() {
	local cmd v
	if cmd=$(sys_cmd getprop); then
		v=$("$cmd" "$1" 2>/dev/null)
		printf '%s\n' "${v:-unset}"
	else
		echo "no getprop"
	fi
}

# --- facts --------------------------------------------------------------

page_size() {
	local v
	if have getconf && v=$(getconf PAGESIZE 2>/dev/null) && [ -n "$v" ]; then
		echo "$v (getconf)"
		return
	fi
	# AT_PAGESZ (6) from the auxiliary vector, 64-bit pairs.
	v=$(od -An -v -t u8 /proc/self/auxv 2>/dev/null | tr -s ' ' '\n' | grep -v '^$' |
		awk 'NR % 2 == 1 { t = $1; next } t == 6 { print; exit }')
	echo "${v:-unknown} (auxv)"
}

cmd_facts() {
	section "facts"
	say "kit: $("$BIN/syncr-worker" version 2>&1)"
	if sys_cmd getprop >/dev/null; then
		say "android: $(prop ro.build.version.release) (SDK $(prop ro.build.version.sdk), security patch $(prop ro.build.version.security_patch))"
		say "device: $(prop ro.product.manufacturer) $(prop ro.product.model) ($(prop ro.product.device)), ABI $(prop ro.product.cpu.abi)"
		say "build: $(prop ro.build.fingerprint)"
	else
		say "android: no getprop on PATH or in /system/bin (not Android?)"
	fi
	say "termux: version ${TERMUX_VERSION:-unknown}, from ${TERMUX_APK_RELEASE:-unknown} (GOOGLE_PLAY_STORE builds restrict exec)"
	say "kernel: $(uname -srm)"
	say "uname -n (the host name Go and restic see): $(uname -n)"
	say "page size: $(page_size)"
	say "cpus: $(nproc 2>/dev/null || echo unknown); $(grep -m1 MemTotal /proc/meminfo 2>/dev/null)"
	say "time zone: TZ=${TZ:-unset}, persist.sys.timezone=$(prop persist.sys.timezone), date: $(date)"
	say "user: $(id 2>&1)"
	say "open files limit: $(ulimit -n)"
	capture "df -h \$HOME" df -h "$HOME"
	if shared_ready; then
		capture "df -h $SHARED" df -h "$SHARED"
		capture "mount type of $SHARED" sh -c "grep -E ' (/storage/emulated|/mnt/user/[0-9]+/emulated)' /proc/self/mounts | head -n 5"
	else
		say "shared storage: not set up yet (run termux-setup-storage)"
	fi
	say "phantom-process settings (readable without adb only on some phones):"
	local cmd
	if cmd=$(sys_cmd settings); then
		capture "settings get global settings_enable_monitor_phantom_procs" with_timeout 30 "$cmd" get global settings_enable_monitor_phantom_procs
	else
		say "    no settings command"
	fi
	if cmd=$(sys_cmd device_config); then
		capture "device_config get activity_manager max_phantom_processes" with_timeout 30 "$cmd" get activity_manager max_phantom_processes
	else
		say "    no device_config command"
	fi
	if [ -r /proc/1/cgroup ]; then
		say "/proc/1/cgroup: readable"
	else
		say "/proc/1/cgroup: not readable (the worker's service library then treats 'run' as started by a service manager; harmless)"
	fi
	if [ -e /dev/fuse ]; then
		if [ -r /dev/fuse ] && [ -w /dev/fuse ]; then
			say "/dev/fuse: read-write"
		else
			say "/dev/fuse: present, not usable by this app (rclone mount unavailable, as expected)"
		fi
	else
		say "/dev/fuse: missing"
	fi
	say "wake lock: $(have termux-wake-lock && echo 'termux-wake-lock available' || echo 'no termux-wake-lock')"
	capture "gofacts (what Go sees, and DNS + TLS from Go)" with_timeout 60 "$BIN/gofacts"
}

# --- tools --------------------------------------------------------------

# test_skip <name> prints the -test.skip pattern for a kit test binary, and
# test_skip_reason why those tests cannot run on a phone:
#   - MinIO, SFTP: they start Docker containers (testcontainers).
#   - TestWaitDelayWithRealProcess: its fake restic is a "#!/bin/sh"
#     script. Android has no /bin/sh, and Go starts programs with execve
#     directly, so Termux's shebang rewriting (termux-exec) never applies.
test_skip() {
	case "$1" in
	rclone-runner) echo 'MinIO|SFTP' ;;
	restic-runner) echo 'MinIO|SFTP|^TestWaitDelayWithRealProcess$' ;;
	*) echo '' ;;
	esac
}
test_skip_reason() {
	case "$1" in
	rclone-runner) echo 'MinIO and SFTP need Docker' ;;
	restic-runner) echo 'MinIO and SFTP need Docker; TestWaitDelayWithRealProcess needs /bin/sh' ;;
	*) echo '' ;;
	esac
}

run_test_binary() {
	local name=$1 dir=$2 bin=$TESTS/$1.test log=$WORK/logs/test-$1.log skip t0 t1 rc pass fail skipped
	skip=$(test_skip "$name")
	t0=$(now)
	(
		cd "$TESTS/tree/$dir" || exit 1
		export SYNCR_TEST_RCLONE=$BIN/rclone SYNCR_TEST_RESTIC=$BIN/restic
		if [ -n "$skip" ]; then
			"$bin" -test.v -test.count=1 -test.timeout=30m -test.skip "$skip"
		else
			"$bin" -test.v -test.count=1 -test.timeout=30m
		fi
	) >"$log" 2>&1
	rc=$?
	t1=$(now)
	pass=$(grep -cE '^ *--- PASS' "$log")
	fail=$(grep -cE '^ *--- FAIL' "$log")
	skipped=$(grep -cE '^ *--- SKIP' "$log")
	say "test $name: exit $rc; tests and subtests: $pass passed, $fail failed, $skipped skipped; $(secs "$t0" "$t1") s${skip:+ (not run: $skip; $(test_skip_reason "$name"))}"
	if [ "$fail" -gt 0 ] || [ "$rc" -ne 0 ]; then
		grep -E '^ *--- FAIL' "$log" | head -n 30 | sed 's/^/    /' | tee -a "$REPORT"
		say "    messages:"
		grep -E '^ +[A-Za-z0-9_]+\.go:[0-9]+: |^panic:|^FAIL|timed out' "$log" | head -n 60 | sed 's/^/      /' | tee -a "$REPORT"
	fi
	if [ "$skipped" -gt 0 ]; then
		rec "    skip reasons:"
		grep -A1 -E '^ *--- SKIP' "$log" | grep -E '\.go:[0-9]+: ' | sed -E 's/^ +//' | sort | uniq -c | head -n 20 | sed 's/^/      /' >>"$REPORT"
	fi
}

cmd_tools() {
	require_kit
	section "tools"
	if have sha256sum; then
		capture "sha256sum -c --quiet SHA256SUMS (every file of the kit)" sh -c "cd '$KIT' && sha256sum -c --quiet SHA256SUMS"
	else
		say "no sha256sum: the kit's checksums are not checked"
	fi
	capture "syncr-worker version" "$BIN/syncr-worker" version
	capture "rclone version" sh -c "'$BIN/rclone' version | head -n 3"
	capture "restic version" "$BIN/restic" version
	capture "syncr-worker tools status (the worker's own version check)" \
		env SYNCR_RCLONE_PATH="$BIN/rclone" SYNCR_RESTIC_PATH="$BIN/restic" "$BIN/syncr-worker" tools status --dir "$WORK/tools-status"
	capture "rclone lsf over HTTPS (DNS + TLS from rclone)" sh -c \
		"'$BIN/rclone' lsf --config /dev/null --max-depth 1 --http-url https://downloads.rclone.org/ :http: 2>&1 | tail -n 3"
	if [ ! -f "$TESTS/index.txt" ]; then
		say "no tests/index.txt: the kit is incomplete"
		return
	fi
	say "worker test binaries (logs in $WORK/logs/; a phone may take 5-15 min):"
	local name dir
	while read -r name dir; do
		[ -n "$name" ] || continue
		run_test_binary "$name" "$dir"
	done <"$TESTS/index.txt"
}

# --- storage ------------------------------------------------------------

# make_data <dir> writes FILES files of mixed sizes, DATA_MB in total, in
# folders of 100 (skipped when it already holds this size).
make_data() {
	local dir=$1 total=$((DATA_MB * 1048576)) n_large n_medium n_small i size seed=12345 small_sum=0 medium_sum=0 large
	if [ "$(cat "$dir.params" 2>/dev/null)" = "$DATA_MB $FILES" ]; then
		return 0
	fi
	rm -rf "$dir" "$dir.params"
	mkdir -p "$dir"
	n_large=$((FILES / 100))
	[ $n_large -ge 1 ] || n_large=1
	n_medium=$((FILES / 10))
	n_small=$((FILES - n_large - n_medium))
	[ $n_small -ge 0 ] || n_small=0
	next() { seed=$(((seed * 1103515245 + 12345) % 2147483648)); }
	for ((i = 0; i < FILES; i++)); do
		mkdir -p "$dir/d$((i / 100))"
		next
		if [ $i -lt $n_small ]; then
			size=$((1024 + seed % 65536))
			small_sum=$((small_sum + size))
		elif [ $i -lt $((n_small + n_medium)) ]; then
			size=$((131072 + seed % 917504))
			medium_sum=$((medium_sum + size))
		else
			large=$(((total - small_sum - medium_sum) / n_large))
			[ $large -ge 65536 ] || large=65536
			size=$large
		fi
		head -c "$size" /dev/urandom >"$dir/d$((i / 100))/f$i.bin" || return 1
	done
	echo "$DATA_MB $FILES" >"$dir.params"
}

dir_bytes() { du -sk "$1" | awk '{ print $1 * 1024 }'; }

# json_field <json line> <key> prints a number or string field.
json_field() { sed -nE "s/.*\"$2\":\"?([^\",}]*)\"?[,}].*/\1/p" <<<"$1" | head -n 1; }

restic_env() {
	if [ ! -s "$WORK/restic-password" ]; then
		od -An -v -tx1 -N24 /dev/urandom | tr -d ' \n' >"$WORK/restic-password"
	fi
	export RESTIC_PASSWORD_FILE=$WORK/restic-password RESTIC_CACHE_DIR=$WORK/restic-cache
}

# backup <label> <repo> <dir> [flags...] runs one timed restic backup and
# records its summary; it sets LAST_UNMODIFIED and LAST_TOTAL.
backup() {
	local label=$1 repo=$2 dir=$3 log t0 t1 rc summary errors
	shift 3
	log=$WORK/logs/restic-$label.json
	t0=$(now)
	"$BIN/restic" -r "$repo" backup --json --host spike "$@" "$dir" >"$log" 2>"$log.err"
	rc=$?
	t1=$(now)
	summary=$(grep '"message_type":"summary"' "$log" | tail -n 1)
	errors=$(grep -c '"message_type":"error"' "$log")
	LAST_UNMODIFIED=$(json_field "$summary" files_unmodified)
	LAST_TOTAL=$(json_field "$summary" total_files_processed)
	say "  $label: exit $rc in $(secs "$t0" "$t1") s; new $(json_field "$summary" files_new), changed $(json_field "$summary" files_changed), unmodified ${LAST_UNMODIFIED:-?} of ${LAST_TOTAL:-?}; added $(json_field "$summary" data_added) bytes; $errors errors${*:+ (flags: $*)}"
	if [ $rc -ne 0 ] || [ "$errors" -gt 0 ]; then
		{ grep '"message_type":"error"' "$log" | head -n 5; head -n 5 "$log.err"; } | sed 's/^/      /' | tee -a "$REPORT"
	fi
}

stat_sample() {
	local dir=$1
	find "$dir" -type f -name 'f*.bin' 2>/dev/null | LC_ALL=C sort |
		awk '{ l[NR] = $0 } END { if (NR) { print l[1]; if (NR > 2) print l[int(NR / 2)]; if (NR > 1) print l[NR] } }' |
		while read -r f; do stat -c '%i ctime=%Z mtime=%Y %s %n' "$f"; done
}

storage_location() {
	local name=$1 dir=$2 repo bytes t0 t1 before after
	repo=$WORK/repos/$name
	say "-- $name: $dir"
	bytes=$(dir_bytes "$dir")
	t0=$(now)
	find "$dir" -type f -exec cat {} + >/dev/null
	t1=$(now)
	say "  read: $bytes bytes in $(secs "$t0" "$t1") s ($(rate "$bytes" "$t0" "$t1"); the page cache may hold part of it)"
	rm -rf "$repo"
	mkdir -p "$WORK/repos"
	t0=$(now)
	capture "restic init ($name)" "$BIN/restic" -r "$repo" init
	t1=$(now)
	say "  init took $(secs "$t0" "$t1") s"
	before=$(stat_sample "$dir")
	backup "$name-1" "$repo" "$dir"
	backup "$name-2" "$repo" "$dir"
	after=$(stat_sample "$dir")
	say "  inode ctime mtime size (before backup 1):"
	printf '%s\n' "$before" | sed 's/^/    /' | tee -a "$REPORT"
	say "  inode ctime mtime size (after backup 2):"
	printf '%s\n' "$after" | sed 's/^/    /' | tee -a "$REPORT"
	if [ "$before" = "$after" ]; then
		say "  inode and ctime: stable"
	else
		say "  inode and ctime: CHANGED between the two backups"
	fi
	if [ -n "${LAST_TOTAL:-}" ] && [ "${LAST_UNMODIFIED:-0}" = "$LAST_TOTAL" ]; then
		say "  change detection: backup 2 saw every file as unmodified"
		return
	fi
	say "  change detection: backup 2 re-read files; trying --ignore-inode"
	backup "$name-3" "$repo" "$dir" --ignore-inode
	if [ -n "${LAST_TOTAL:-}" ] && [ "${LAST_UNMODIFIED:-0}" = "$LAST_TOTAL" ]; then
		say "  change detection: needs --ignore-inode on $name"
		return
	fi
	backup "$name-4" "$repo" "$dir" --ignore-inode --ignore-ctime
	if [ -n "${LAST_TOTAL:-}" ] && [ "${LAST_UNMODIFIED:-0}" = "$LAST_TOTAL" ]; then
		say "  change detection: needs --ignore-inode --ignore-ctime on $name"
	else
		say "  change detection: restic re-reads files on $name even with --ignore-inode --ignore-ctime"
	fi
}

cmd_storage() {
	require_kit
	section "storage"
	require_shared
	restic_env
	local t0 t1 internal=$WORK/data-internal shared=$SPIKE_DIR/data
	say "synthetic data: $FILES files, about $DATA_MB MB (SPIKE_FILES, SPIKE_DATA_MB)"
	t0=$(now)
	make_data "$internal" || {
		say "could not create $internal"
		return 1
	}
	t1=$(now)
	say "internal storage data ready in $(secs "$t0" "$t1") s: $internal"
	if [ "$(cat "$shared.params" 2>/dev/null)" != "$DATA_MB $FILES" ]; then
		rm -rf "$shared" "$shared.params"
		mkdir -p "$SPIKE_DIR"
		t0=$(now)
		cp -R "$internal" "$shared" || {
			say "could not copy the data to $shared"
			return 1
		}
		t1=$(now)
		echo "$DATA_MB $FILES" >"$shared.params"
		say "copied to shared storage in $(secs "$t0" "$t1") s ($(rate "$(dir_bytes "$internal")" "$t0" "$t1") write)"
	fi
	storage_location internal "$internal"
	storage_location shared "$shared"
}

# --- long job -----------------------------------------------------------

# _long-job runs rclone copies to shared storage and restic backups (with
# --force, so every file is read) in a loop for LONG_MIN minutes. Every
# step appends to $LONG/state; "end" is written only when it finishes.
cmd_long_job() {
	local deadline i=0 t0 rc1 rc2
	restic_env
	deadline=$(($(date +%s) + LONG_MIN * 60))
	trap 'echo "signal TERM $(date +%s)" >>"$LONG/state"; exit 143' TERM
	trap 'echo "signal HUP $(date +%s)" >>"$LONG/state"' HUP
	echo "start $(date +%s) pid $$" >>"$LONG/state"
	while [ "$(date +%s)" -lt $deadline ]; do
		i=$((i + 1))
		t0=$(date +%s)
		rm -rf "$SPIKE_DIR/long"
		"$BIN/rclone" copy --config /dev/null "$WORK/data-internal" "$SPIKE_DIR/long" >>"$LONG/job.log" 2>&1
		rc1=$?
		"$BIN/restic" -r "$WORK/repos/long" backup --host spike --force --quiet "$SPIKE_DIR/long" >>"$LONG/job.log" 2>&1
		rc2=$?
		echo "iteration $i start $t0 end $(date +%s) rclone $rc1 restic $rc2" >>"$LONG/state"
	done
	echo "end $(date +%s) iterations $i" >>"$LONG/state"
}

cmd_long_start() {
	require_kit
	section "long-start"
	require_shared
	if [ -f "$LONG/pid" ] && kill -0 "$(cat "$LONG/pid")" 2>/dev/null; then
		say "the long job is still running (pid $(cat "$LONG/pid")); run: $0 long-check"
		return 1
	fi
	restic_env
	make_data "$WORK/data-internal" || return 1
	rm -rf "$LONG" "$WORK/repos/long"
	mkdir -p "$LONG" "$WORK/repos"
	"$BIN/restic" -r "$WORK/repos/long" init >"$LONG/init.log" 2>&1 || {
		say "restic init failed: $(tail -n 3 "$LONG/init.log")"
		return 1
	}
	if [ "${SPIKE_NO_WAKELOCK:-}" = 1 ]; then
		say "wake lock: not taken (SPIKE_NO_WAKELOCK=1)"
		echo no >"$LONG/wakelock"
	elif have termux-wake-lock; then
		termux-wake-lock && say "wake lock: taken (termux-wake-lock)"
		echo yes >"$LONG/wakelock"
	else
		say "wake lock: termux-wake-lock is not available"
		echo unavailable >"$LONG/wakelock"
	fi
	nohup bash "$KIT/termux-spike.sh" _long-job >>"$LONG/job.log" 2>&1 &
	echo $! >"$LONG/pid"
	date +%s >"$LONG/started"
	say "long job started (pid $!, $LONG_MIN min of rclone copies and restic backups)"
	say ""
	say "Now: leave Termux (home button), lock the screen, and leave the phone alone for at least 15 minutes,"
	say "unplugged if you can. Then open Termux again and run: $0 long-check"
}

cmd_long_check() {
	section "long-check"
	if [ ! -f "$LONG/state" ] && [ ! -f "$LONG/pid" ]; then
		say "no long job was started; run: $0 long-start"
		return 1
	fi
	local pid started start_ts end_line iterations failed gaps running=no
	pid=$(cat "$LONG/pid" 2>/dev/null)
	started=$(cat "$LONG/started" 2>/dev/null)
	if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
		running=yes
	fi
	start_ts=$(awk '$1 == "start" { print $2; exit }' "$LONG/state" 2>/dev/null)
	end_line=$(grep '^end ' "$LONG/state" 2>/dev/null)
	iterations=$(grep -c '^iteration ' "$LONG/state" 2>/dev/null)
	failed=$(awk '$1 == "iteration" && ($8 != 0 || $10 != 0)' "$LONG/state" 2>/dev/null | wc -l)
	say "wake lock: $(cat "$LONG/wakelock" 2>/dev/null); started $(( $(date +%s) - ${started:-$(date +%s)} )) s ago"
	say "iterations: $iterations, failed: $failed (exit 137 = killed by SIGKILL)"
	gaps=$(awk -v s="${start_ts:-0}" '$1 == "iteration" { gap = $4 - prev; if (prev > 0 && gap > 120) printf "gap of %d s before iteration %d; ", gap, $2; prev = $6 } END { }' "$LONG/state" 2>/dev/null)
	[ -z "$gaps" ] || say "pauses between iterations: $gaps"
	grep -E '^signal ' "$LONG/state" 2>/dev/null | sed 's/^/  /' | tee -a "$REPORT"
	awk '$1 == "iteration" && ($8 != 0 || $10 != 0)' "$LONG/state" 2>/dev/null | head -n 10 | sed 's/^/  failed: /' | tee -a "$REPORT"
	if [ "$running" = yes ]; then
		say "result: STILL RUNNING (pid $pid); check again after it ends ($LONG_MIN min after the start)"
		return 0
	fi
	if [ -n "$end_line" ]; then
		if [ "$failed" -eq 0 ]; then
			say "result: COMPLETED, every iteration succeeded ($end_line)"
		else
			say "result: COMPLETED with failed iterations ($end_line)"
		fi
	else
		say "result: KILLED: the job is gone and never wrote its end marker (last: $(tail -n 1 "$LONG/state" 2>/dev/null))"
	fi
	if have termux-wake-unlock; then
		termux-wake-unlock && say "wake lock released"
	fi
}

# --- worker -------------------------------------------------------------

worker_pid() {
	local pid
	pid=$(cat "$WDIR/pid" 2>/dev/null) || return 1
	kill -0 "$pid" 2>/dev/null && printf '%s\n' "$pid"
}

worker_name() {
	local model
	model=$(prop ro.product.model)
	case "$model" in "" | unset | "no getprop") model=$(uname -n) ;; esac
	model=$(printf '%s' "$model" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9' '-' | sed -E 's/-+/-/g; s/^-//; s/-$//')
	printf '%s-spike\n' "${model:-phone}"
}

cmd_worker() {
	require_kit
	section "worker"
	require_shared
	local pid
	if pid=$(worker_pid); then
		say "the worker is already running (pid $pid); use worker-check or worker-stop"
		return 1
	fi
	local url ca token name cfg=$WDIR/worker.yaml data=$WDIR/data force=() cafile=() out rc line
	name=$(worker_name)
	mkdir -p "$WDIR" "$SPIKE_DIR/data" "$SPIKE_DIR/restore"
	if [ -f "$cfg" ]; then
		printf 'A worker is already set up here (%s). Start it again without enrolling? [Y/n] ' "$cfg"
		read -r line
		case "$line" in
		n | N | no) force=(--force) ;;
		*)
			start_worker
			return
			;;
		esac
	fi
	printf 'Controller URL (https://...): '
	read -r url
	case "$url" in
	https://* | http://*) ;;
	*)
		say "not a URL: the controller's address starts with https://"
		return 1
		;;
	esac
	printf 'CA file of a self-signed controller (Enter for none): '
	read -r ca
	[ -z "$ca" ] || cafile=(--ca-file "$ca")
	printf 'Enrollment token (Workers > Add worker; it is not shown): '
	read -r -s token
	echo
	[ -n "$token" ] || {
		say "no token given"
		return 1
	}
	say "setup: name $name, data $data, config $cfg, local root $SPIKE_DIR"
	# The token travels only in the environment of setup, never on a command
	# line; any echo of it would be replaced before the output is recorded.
	out=$(SYNCR_ENROLLMENT_TOKEN=$token SYNCR_RCLONE_PATH=$BIN/rclone SYNCR_RESTIC_PATH=$BIN/restic \
		"$BIN/syncr-worker" setup --controller "$url" --name "$name" --user --no-service \
		--data-dir "$data" --config "$cfg" --local-root "$SPIKE_DIR" "${cafile[@]}" "${force[@]}" 2>&1)
	rc=$?
	while IFS= read -r line; do
		line=${line//"$token"/[token]}
		printf '    %s\n' "$line"
	done <<<"$out" | tee -a "$REPORT"
	token=
	say "setup exit $rc"
	[ $rc -eq 0 ] || return 1
	start_worker
}

start_worker() {
	local pid
	nohup "$BIN/syncr-worker" run --config "$WDIR/worker.yaml" --log-file "$WDIR/worker.log" >>"$WDIR/run.out" 2>&1 &
	pid=$!
	echo "$pid" >"$WDIR/pid"
	date +%s >"$WDIR/started"
	sleep 5
	if ! kill -0 "$pid" 2>/dev/null; then
		say "the worker stopped right away:"
		tail -n 20 "$WDIR/run.out" "$WDIR/worker.log" 2>/dev/null | redact | sed 's/^/    /' | tee -a "$REPORT"
		return 1
	fi
	say "worker running (pid $pid), log $WDIR/worker.log"
	cat <<EOF

In the Syncr web UI, with this phone as the worker ($(worker_name)):
  1. Back up the folder $SPIKE_DIR/data to a restic repository (any of yours).
     Note the host name the snapshot shows.
  2. Restore that snapshot into $SPIKE_DIR/restore.
  3. Run a transfer (copy) between this phone and one of your remotes, either way.
  4. Start one more backup or transfer, lock the screen at once, and wait 10 min.
  5. Browse the phone's folders in the file browser.
Then run: $0 worker-check   and later: $0 worker-stop
EOF
}

# redact drops the values of keys that could hold a secret from log lines.
redact() { sed -E 's/([A-Za-z_]*([Ss]ecret|[Tt]oken|[Pp]assword)[A-Za-z_]*)=("[^"]*"|[^ ]*)/\1=[redacted]/g'; }

cmd_worker_check() {
	section "worker-check"
	local log=$WDIR/worker.log pid
	[ -f "$log" ] || {
		say "no worker log; run: $0 worker"
		return 1
	}
	if pid=$(worker_pid); then
		say "worker: running (pid $pid, started $(($(date +%s) - $(cat "$WDIR/started"))) s ago)"
	else
		say "worker: not running"
	fi
	say "log lines: $(wc -l <"$log"); warnings: $(grep -c 'level=WARN' "$log"); errors: $(grep -c 'level=ERROR' "$log")"
	say "start-up and link:"
	grep -E 'msg="?(worker starting|worker ready|tool resolved|tool candidate rejected|worker link starting|active link started|active link stopped|worker stopped|the worker stopped with an error)' "$log" |
		tail -n 20 | redact | sed 's/^/  /' | tee -a "$REPORT"
	say "connection: $(grep -c 'msg="link call failed' "$log") failed and $(grep -c 'msg="link call rejected' "$log") rejected link calls; recovered $(grep -cE 'msg="(report|poll) succeeded again' "$log") times"
	grep -E 'msg="(link call failed|link call rejected|report succeeded again|poll succeeded again)' "$log" |
		tail -n 6 | redact | cut -c1-300 | sed 's/^/  /' | tee -a "$REPORT"
	say "jobs:"
	grep -E 'msg="?(job finished|stopping job)' "$log" | tail -n 30 | redact | sed 's/^/  /' | tee -a "$REPORT"
	say "warnings and errors (count, message):"
	grep -E 'level=(WARN|ERROR)' "$log" | sed -nE 's/.*level=(WARN|ERROR) msg=("[^"]*"|[^ ]*).*/\1 \2/p' |
		sort | uniq -c | sort -rn | head -n 20 | sed 's/^/  /' | tee -a "$REPORT"
	say "last warnings and errors:"
	grep -E 'level=(WARN|ERROR)' "$log" | tail -n 10 | redact | cut -c1-400 | sed 's/^/  /' | tee -a "$REPORT"
}

cmd_worker_stop() {
	section "worker-stop"
	local pid i
	if ! pid=$(worker_pid); then
		say "the worker is not running"
		return 0
	fi
	kill -TERM "$pid"
	for ((i = 0; i < 40; i++)); do
		kill -0 "$pid" 2>/dev/null || break
		sleep 1
	done
	if kill -0 "$pid" 2>/dev/null; then
		say "the worker did not stop within 40 s after SIGTERM (pid $pid)"
		return 1
	fi
	say "worker stopped after SIGTERM in about $i s"
	grep -E 'msg="?(shutdown|worker stopped)' "$WDIR/worker.log" 2>/dev/null | tail -n 8 | redact | sed 's/^/  /' | tee -a "$REPORT"
	rm -f "$WDIR/pid"
}

# --- all and cleanup ----------------------------------------------------

cmd_all() {
	if [ -s "$REPORT" ]; then
		mv "$REPORT" "$WORK/report-$(date +%Y%m%d-%H%M%S).txt"
	fi
	cmd_facts
	cmd_tools
	cmd_storage
	cat <<EOF

Done with facts, tools and storage. Next:
  1. $0 long-start    then lock the screen for at least 15 minutes
  2. $0 long-check
  3. optional, with your Syncr controller: $0 worker, the steps it prints,
     then $0 worker-check and $0 worker-stop
  4. send back $REPORT
     (also in the Download folder as syncr-spike-report.txt)
EOF
}

cmd_cleanup() {
	printf 'Remove the synthetic data, the repositories, the worker files and %s? Type yes: ' "$SPIKE_DIR"
	local answer
	read -r answer
	[ "$answer" = yes ] || {
		echo "nothing removed"
		return 0
	}
	worker_pid >/dev/null && cmd_worker_stop
	if [ -f "$LONG/pid" ] && kill -0 "$(cat "$LONG/pid")" 2>/dev/null; then
		kill "$(cat "$LONG/pid")" 2>/dev/null
	fi
	have termux-wake-unlock && termux-wake-unlock
	rm -rf "$SPIKE_DIR" "$WORK/data-internal" "$WORK/data-internal.params" "$WORK/repos" "$WORK/restic-cache" \
		"$WORK/restic-password" "$LONG" "$WDIR" "$WORK/tools-status" "$WORK/logs"
	rm -f "$SHARED/Download/syncr-spike-report.txt"
	echo "Removed. The report stays in $REPORT; delete $WORK to remove it too."
	echo "If you enrolled the worker, remove it in the Syncr UI (Workers)."
}

# usage prints the comment block at the top of this file.
usage() { awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"; }

case "${1:-}" in
all | facts | tools | storage | long-start | long-check | _long-job | worker | worker-check | worker-stop | cleanup) prepare ;;
esac
case "${1:-}" in
all) cmd_all ;;
facts) cmd_facts ;;
tools) cmd_tools ;;
storage) cmd_storage ;;
long-start) cmd_long_start ;;
long-check) cmd_long_check ;;
_long-job) cmd_long_job ;;
worker) cmd_worker ;;
worker-check) cmd_worker_check ;;
worker-stop) cmd_worker_stop ;;
cleanup) cmd_cleanup ;;
-h | --help | help | "")
	usage
	exit 0
	;;
*)
	echo "unknown command $1" >&2
	usage >&2
	exit 2
	;;
esac
status=$?
[ "${1:-}" = _long-job ] || [ "${1:-}" = cleanup ] || publish
exit $status
