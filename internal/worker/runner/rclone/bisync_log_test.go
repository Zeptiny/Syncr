package rclone

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner/scrub"
)

// The testdata/bisync_*.jsonl files are the output of rclone v1.75.2
// bisync with the runner's fixed flags (--use-json-log --log-level=NOTICE
// --stats=1s --stats-log-level=NOTICE --color=NEVER --workdir=<state>/work
// --filters-file=<state>/filters.txt --recover --resilient), the pair's
// sides in /data/a and /data/b and its state in /data/st (local paths
// rewritten). Each was recorded in this situation:
//
//	bisync_resync.jsonl                 first run, --resync --resync-mode=newer --bwlimit=1M
//	bisync_conflicts.jsonl              new files on both sides, a deletion, two files edited on both sides
//	bisync_conflict_names.jsonl         conflicts of names with a quote, a tab, " - ", accents, a subfolder
//	bisync_conflict_resolve_newer.jsonl --conflict-resolve=newer: a winner, equal versions, no winner (equal times)
//	bisync_conflicts_dry_run.jsonl      the same changes with --dry-run (default resolve)
//	bisync_dry_run.jsonl                --dry-run: a new file, a deletion, a conflict
//	bisync_too_many_deletes.jsonl       5 of 8 files deleted on path1 (exit 1)
//	bisync_all_changed.jsonl            the only file edited on both sides (exit 1)
//	bisync_filters_changed.jsonl        the filters file changed since the resync (exit 7)
//	bisync_filters_md5_missing.jsonl    the filters file's hash removed (exit 7)
//	bisync_critical_must_resync.jsonl   an unreadable file on path1 on the first run after a resync (exit 7,
//	                                    listings renamed .lst-err, no previous listings yet)
//	bisync_no_prior_listings.jsonl      the run after that one (exit 7)
//	bisync_recovered.jsonl              a later run, once the file could be read again, after the same error had
//	                                    stopped the run before it: rclone went on from the previous listings (exit 0)
//	bisync_empty_prior.jsonl            a file added after a resync of two empty sides (exit 7)
//	bisync_empty_current.jsonl          path1 emptied (exit 7)
//	bisync_missing_side.jsonl           path1 removed (exit 7)
//	bisync_check_access.jsonl           --check-access without RCLONE_TEST files (exit 7)
//	bisync_check_access_one_side.jsonl  --check-access, RCLONE_TEST on path1 only (exit 7)
//	bisync_check_access_resync.jsonl    --resync --check-access, RCLONE_TEST on path1 only: a fresh pair's first run (exit 7)
//	bisync_prior_lock.jsonl             a lock file of another run in the work dir (exit 1)

// readBisyncLog feeds a fixture through a tracker with a bisyncLog, as Run
// does.
func readBisyncLog(t *testing.T, fixture string, sc *scrub.Scrubber) (*bisyncLog, *tracker) {
	t.Helper()
	l := &bisyncLog{sc: sc, stateDir: "/data/st"}
	tr := newTracker(nil, sc)
	tr.observe = l.line
	for _, line := range fixtureLines(t, fixture) {
		tr.line(proc.Line{Data: []byte(line)})
	}
	return l, tr
}

func TestBisyncLogFixtures(t *testing.T) {
	t.Parallel()
	const (
		failed   = syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED
		resync   = syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED
		safety   = syncrv1.JobErrorCode_JOB_ERROR_CODE_SAFETY_ABORT
		side     = syncrv1.JobErrorCode_JOB_ERROR_CODE_SIDE_UNAVAILABLE
		notFound = syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND
		internal = syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL
	)
	tests := []struct {
		fixture   string
		run       failedRun // what the pair's state allows after the run (default: only a resync helps)
		conflicts []string
		code      syncrv1.JobErrorCode // UNSPECIFIED: no failure message
		message   string               // exact, when code is set
	}{
		{fixture: "bisync_resync.jsonl"},
		{fixture: "bisync_conflicts.jsonl", conflicts: []string{"f2.txt", "new both.txt"}},
		{fixture: "bisync_conflict_names.jsonl", conflicts: []string{`q"uote.txt`, "sub/deep.txt", "tab␉here.txt", "x - y.txt", "ünï cødé.txt"}},
		{fixture: "bisync_conflict_resolve_newer.jsonl", conflicts: []string{"f1.txt", "f3.txt"}},
		{fixture: "bisync_conflicts_dry_run.jsonl", conflicts: []string{"f1.txt", "f3.txt"}},
		{fixture: "bisync_dry_run.jsonl", conflicts: []string{"f1.txt"}},
		{
			fixture: "bisync_too_many_deletes.jsonl", code: safety,
			message: `safety check stopped the two-way sync: too many deletes (>50%, 5 of 8) on Path1 "/data/a/"; if these changes are intended, run it once with force`,
		},
		{
			fixture: "bisync_all_changed.jsonl", code: safety,
			message: `safety check stopped the two-way sync: all files were changed on Path1 "/data/a/"; all files were changed on Path2 "/data/b/"; if these changes are intended, run it once with force`,
		},
		{
			fixture: "bisync_filters_changed.jsonl", code: resync,
			message: "a resync is required: filters file has changed; run it once with resync to rebuild the pair's state from both sides as they are now",
		},
		{
			fixture: "bisync_filters_md5_missing.jsonl", code: resync,
			message: "a resync is required: filters file md5 hash not found; run it once with resync to rebuild the pair's state from both sides as they are now",
		},
		{
			// The first run after a resync: no previous listings to go on
			// from.
			fixture: "bisync_critical_must_resync.jsonl", code: resync,
			message: "a resync is required: failed to open source object: open /data/a/locked.txt: permission denied, " +
				"and the pair has no earlier state to go on from; fix the cause first (a resync stops at the same error), " +
				"then run it once with resync to rebuild the pair's state from both sides as they are now",
		},
		{
			// A later run: --recover goes on from the previous listings.
			fixture: "bisync_critical_must_resync.jsonl", run: failedRun{next: nextContinues}, code: failed,
			message: "the two-way sync stopped with a critical error: failed to open source object: open /data/a/locked.txt: permission denied; " +
				"fix the cause and run it again: the next run goes on from the pair's last good state, without a resync",
		},
		{
			// A requested resync that stopped the same way, the earlier
			// listings kept.
			fixture: "bisync_critical_must_resync.jsonl", run: failedRun{next: nextContinues, resync: true}, code: failed,
			message: "the resync stopped with a critical error: failed to open source object: open /data/a/locked.txt: permission denied; " +
				"fix the cause and run it again: the next run goes on from the pair's last good state, without a resync",
		},
		{
			// A first run's resync: the pair still has no baseline.
			fixture: "bisync_critical_must_resync.jsonl", run: failedRun{next: nextResyncs, resync: true}, code: failed,
			message: "the resync stopped with a critical error: failed to open source object: open /data/a/locked.txt: permission denied; " +
				"fix the cause and run it again: the pair has no state on this worker yet, so its next run resyncs",
		},
		{
			fixture: "bisync_no_prior_listings.jsonl", code: resync,
			message: "a resync is required: cannot find prior Path1 or Path2 listings, likely due to critical error on prior run; " +
				"fix what made the earlier run fail first (a resync stops at the same error), then run it once with resync to rebuild the pair's state from both sides as they are now",
		},
		{
			// A dry run looks for dry-run listings, which a critical error
			// left out; a normal run goes on from the previous listings.
			fixture: "bisync_no_prior_listings.jsonl", run: failedRun{next: nextContinues, dryRun: true}, code: failed,
			message: "the dry run stopped: cannot find prior Path1 or Path2 listings, likely due to critical error on prior run; " +
				"a dry run cannot go on from the state rclone kept after an earlier critical error, but a normal run can: " +
				"fix what made the earlier run fail, then run it once without dry run",
		},
		// Listings that are there do not matter when the filters changed.
		{
			fixture: "bisync_filters_changed.jsonl", run: failedRun{next: nextContinues}, code: resync,
			message: "a resync is required: filters file has changed; run it once with resync to rebuild the pair's state from both sides as they are now",
		},
		// The run that went on from the previous listings once the file
		// could be read: the files new on both sides since then were equal,
		// so no conflict.
		{fixture: "bisync_recovered.jsonl"},
		{
			fixture: "bisync_empty_prior.jsonl", code: resync,
			message: "a resync is required: empty prior Path1 listing; run it once with resync to rebuild the pair's state from both sides as they are now",
		},
		{
			fixture: "bisync_empty_current.jsonl", code: side,
			message: "Folder A (path1) is empty; refusing to sync it with the other side (is the drive mounted and the path right?)",
		},
		{
			fixture: "bisync_check_access.jsonl", code: side,
			message: "check_access failed: RCLONE_TEST files not found on both sides (Path1 count 0, Path2 count 0); is the drive mounted?",
		},
		{
			fixture: "bisync_check_access_one_side.jsonl", code: side,
			message: "check_access failed: RCLONE_TEST files not found on both sides (Path1 count 1, Path2 count 0); is the drive mounted?",
		},
		{
			// A resync that fails the access test also says "Must run
			// --resync to recover", which would not help.
			fixture: "bisync_check_access_resync.jsonl", code: side,
			message: "check_access failed: RCLONE_TEST files not found on both sides (Path1 count 1, Path2 count 0); is the drive mounted?",
		},
		{
			fixture: "bisync_missing_side.jsonl", code: notFound,
			message: "a side of the two-way sync does not exist: Local file system at /data/a: error reading source root directory: directory not found",
		},
		{fixture: "bisync_prior_lock.jsonl", code: internal, message: "prior lock file found"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s %+v", tt.fixture, tt.run), func(t *testing.T) {
			t.Parallel()
			l, tr := readBisyncLog(t, tt.fixture, nil)
			if l.conflicts != int64(len(tt.conflicts)) || !slices.Equal(l.conflictPaths, tt.conflicts) {
				t.Errorf("conflicts = %d %q, want %q", l.conflicts, l.conflictPaths, tt.conflicts)
			}
			code, msg, ok := l.failure(tt.run)
			switch {
			case tt.code == 0 && ok:
				t.Errorf("failure() = %v %q, want none", code, msg)
			case tt.code != 0 && (!ok || code != tt.code):
				t.Errorf("failure() = %v %q (%v), want %v", code, msg, ok, tt.code)
			case tt.code == internal && !strings.Contains(msg, tt.message):
				t.Errorf("message %q lacks %q", msg, tt.message)
			case tt.code != 0 && tt.code != internal && msg != tt.message:
				t.Errorf("message =\n%s\nwant\n%s", msg, tt.message)
			}
			if tt.fixture == "bisync_resync.jsonl" && (tr.progress.Bytes != 3000028 || tr.progress.Files != 5) {
				t.Errorf("stats of a resync = %+v", tr.progress)
			}
		})
	}
}

// TestBisyncLogPhrasesInFileNames: rclone puts file names into its
// messages; a name that contains one of the phrases the runner acts on
// changes nothing, wherever rclone prints it.
func TestBisyncLogPhrasesInFileNames(t *testing.T) {
	t.Parallel()
	names := []string{
		"must run --resync.txt", msgAbortedResync, "x (must run --resync): y.txt", "prior lock file found",
		"Failed to bisync: prior lock file found", msgNoListings, "Empty prior Path1 listing", "empty prior Path2",
		msgConflict, "Renaming Path1 copy", "Not renaming Path2 copy", msgCritical + "z", "Access test failed: ",
	}
	lines := func(name string) []string {
		return []string{
			"Failed to copy: failed to open source object: open /data/a/" + name + ": permission denied",
			"- Path1             File was deleted                            - " + name,
			"- Path2             Queue delete                                - /data/b/" + name,
			"- Path1             Queue copy to Path2                         - /data/b/" + name,
			"Skipped delete as --dry-run is set (size 7): " + name,
		}
	}

	// A run that the delete limit stopped stays SAFETY_ABORT, with no
	// conflict.
	l := &bisyncLog{stateDir: "/data/st"}
	tr := newTracker(nil, nil)
	tr.observe = l.line
	for _, n := range names {
		for _, m := range lines(n) {
			l.line(logLine{Level: "error", Msg: m})
		}
	}
	for _, line := range fixtureLines(t, "bisync_too_many_deletes.jsonl") {
		tr.line(proc.Line{Data: []byte(line)})
	}
	for _, next := range []pairNext{nextNeedsResync, nextResyncs, nextContinues} {
		code, msg, ok := l.failure(failedRun{next: next})
		if !ok || code != syncrv1.JobErrorCode_JOB_ERROR_CODE_SAFETY_ABORT || !strings.Contains(msg, "too many deletes (>50%, 5 of 8)") {
			t.Errorf("next %v: failure() = %v %q %v, want SAFETY_ABORT", next, code, msg, ok)
		}
	}
	if l.conflicts != 0 || l.hasPending {
		t.Errorf("conflicts = %d (pending %v)", l.conflicts, l.hasPending)
	}

	// A critical error about such a file is that error, not changed filters.
	for _, n := range names {
		l := &bisyncLog{stateDir: "/data/st"}
		l.line(logLine{Level: "error", Msg: msgCritical + "failed to open source object: open /data/a/" + n + ": permission denied"})
		l.line(logLine{Level: "error", Msg: msgAbortedResync})
		code, msg, ok := l.failure(failedRun{next: nextContinues})
		if !ok || code != syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED || !strings.Contains(msg, "open /data/a/"+n+": permission denied; fix the cause") {
			t.Errorf("%q: failure() = %v %q %v, want EXECUTION_FAILED naming the file", n, code, msg, ok)
		}
	}

	// The phrases where rclone puts them still count.
	for _, tt := range []struct {
		msg  string
		code syncrv1.JobErrorCode
	}{
		{msgCritical + "filters file has changed (must run --resync): /data/st/filters.txt", syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED},
		{msgCritical + "empty prior Path2 listing: /data/st/work/s.path2.lst", syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED},
		{"Empty prior Path1 listing. Cannot sync to an empty directory: /data/st/work/s.path1.lst", syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED},
		{msgCritical + msgNoListings + ", likely due to critical error on prior run ", syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED},
		{"Failed to bisync: prior lock file found: /data/st/work/s.lck", syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL},
	} {
		l := &bisyncLog{}
		l.line(logLine{Level: "error", Msg: tt.msg})
		if code, msg, ok := l.failure(failedRun{}); !ok || code != tt.code {
			t.Errorf("%q: failure() = %v %q %v, want %v", tt.msg, code, msg, ok, tt.code)
		}
	}
}

func TestIndented(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		msg, text, file string
		ok              bool
	}{
		{"- WARNING           New or changed in both paths                - x - y.txt", msgConflict, "x - y.txt", true},
		{"- Path2             Not renaming Path2 copy, as it was determined the winner - /data/b/f1.txt",
			"Not renaming Path2 copy, as it was determined the winner", "/data/b/f1.txt", true},
		{"-                   Access test failed: Path1 file not found in Path2 - RCLONE_TEST",
			"Access test failed: Path1 file not found in Path2", "RCLONE_TEST", true},
		{"- WARNING           New or changed in both paths", msgConflict, "", true},
		{"- WARNING New or changed in both paths - f.txt", "", "", false}, // not rclone's layout
		{"Failed to copy: New or changed in both paths - f.txt", "", "", false},
		{"- short", "", "", false},
	} {
		text, file, ok := indented(tt.msg)
		if text != tt.text || file != tt.file || ok != tt.ok {
			t.Errorf("indented(%q) = %q, %q, %v; want %q, %q, %v", tt.msg, text, file, ok, tt.text, tt.file, tt.ok)
		}
	}
}

func TestBisyncLogConflictLines(t *testing.T) {
	t.Parallel()
	warning := func(path string) logLine {
		return logLine{Level: "notice", Msg: "- WARNING           New or changed in both paths                - " + path}
	}
	renamed := logLine{Level: "notice", Msg: "- Path1             Renaming Path1 copy                         - /data/a/x.conflict1"}

	// Recorded from rclone v1.75.2: a name that is not valid UTF-8, quoted.
	l := &bisyncLog{}
	l.line(warning(`"bad\xffname.txt"`))
	l.line(renamed)
	if want := []string{"bad�name.txt"}; !slices.Equal(l.conflictPaths, want) {
		t.Errorf("paths = %q, want %q", l.conflictPaths, want)
	}

	// A path rclone did not quote is kept as is, even if it starts like a
	// quoted one; a line without a path counts without one.
	l = &bisyncLog{}
	for _, p := range []string{`"half.txt`, ""} {
		if p == "" {
			l.line(logLine{Msg: "- WARNING           New or changed in both paths"})
		} else {
			l.line(warning(p))
		}
		l.line(renamed)
	}
	if l.conflicts != 2 || !slices.Equal(l.conflictPaths, []string{`"half.txt`}) {
		t.Errorf("conflicts = %d %q", l.conflicts, l.conflictPaths)
	}

	// Equal versions get no resolution line: the warning alone is no
	// conflict, nor are resolution lines without a warning.
	l = &bisyncLog{}
	l.line(renamed)
	l.line(warning("same.txt"))
	l.line(warning("differs.txt"))
	l.line(renamed)
	l.line(logLine{Msg: "- Path2             Renaming Path2 copy                         - /data/b/differs.txt.conflict2"})
	l.line(warning("same-too.txt"))
	if l.conflicts != 1 || !slices.Equal(l.conflictPaths, []string{"differs.txt"}) {
		t.Errorf("conflicts = %d %q, want only differs.txt", l.conflicts, l.conflictPaths)
	}
}

func TestBisyncLogConflictBounds(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-Pa55"
	l := &bisyncLog{sc: scrub.New([]string{secret})}
	resolve := func(path string) {
		l.line(logLine{Msg: "- WARNING           New or changed in both paths                - " + path})
		l.line(logLine{Msg: "- Path1             Renaming Path1 copy                         - x"})
	}
	resolve(strings.Repeat("é", 400)) // 800 bytes
	resolve("notes-" + secret + ".txt")
	for i := range 30 {
		resolve(strings.Repeat("d/", i) + "f.txt")
	}
	if l.conflicts != 32 || len(l.conflictPaths) != maxConflictPaths {
		t.Fatalf("conflicts = %d with %d paths, want 32 with %d", l.conflicts, len(l.conflictPaths), maxConflictPaths)
	}
	first := l.conflictPaths[0]
	if len(first) > maxConflictPathBytes || !utf8.ValidString(first) || !strings.HasSuffix(first, scrub.Ellipsis) {
		t.Errorf("long path: %d bytes, valid %v: %q", len(first), utf8.ValidString(first), first)
	}
	if l.conflictPaths[1] != "notes-[redacted].txt" {
		t.Errorf("path with a secret = %q", l.conflictPaths[1])
	}
}

func TestBisyncLogMessagesAreScrubbed(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-Pa55"
	l := &bisyncLog{sc: scrub.New([]string{secret})}
	l.line(logLine{Level: "error", Msg: "Bisync critical error: login as " + secret + " failed"})
	l.line(logLine{Level: "error", Msg: "Bisync aborted. Must run --resync to recover."})
	code, msg, ok := l.failure(failedRun{})
	if !ok || code != syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED || strings.Contains(msg, secret) || !strings.Contains(msg, "login as [redacted] failed") {
		t.Errorf("failure() = %v %q %v", code, msg, ok)
	}
}

func TestBisyncLogWithoutKnownMessages(t *testing.T) {
	t.Parallel()
	l := &bisyncLog{}
	for _, m := range []string{
		"Bisync critical error: something new",
		"Bisync aborted. Error is retryable without --resync due to --resilient mode.",
		"Failed to bisync: bisync aborted",
	} {
		l.line(logLine{Level: "error", Msg: m})
	}
	if code, msg, ok := l.failure(failedRun{}); ok {
		t.Errorf("failure() = %v %q, want none (the exit code decides)", code, msg)
	}
	if l.resyncReason() != "something new" {
		t.Errorf("resyncReason() = %q", l.resyncReason())
	}
	if (&bisyncLog{}).resyncReason() != "rclone stopped with a critical error" {
		t.Error("resyncReason() without a critical error")
	}
}
