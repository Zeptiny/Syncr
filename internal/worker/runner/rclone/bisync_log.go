package rclone

import (
	"strconv"
	"strings"
	"unicode/utf8"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
	"github.com/Zeptiny/Syncr/internal/worker/runner/scrub"
)

// Bounds of RcloneBisyncOutput.conflict_paths.
const (
	maxConflictPaths     = 20
	maxConflictPathBytes = 500
)

// Messages of rclone bisync (1.75.2) that the runner acts on. The tests
// check them against output recorded from that version
// (testdata/bisync_*.jsonl), so a tool upgrade that changes them fails the
// tests instead of mislabelling jobs.
//
// rclone puts file names into many of its messages, so each phrase is
// matched where rclone itself puts it (at the start of its message, or in
// the message part of a line about one file, see indented), never anywhere
// in a line: a file named "must run --resync" must not change a job's
// outcome.
const (
	// rclone logs this for every file new or changed on both sides, at
	// NOTICE: "- WARNING  New or changed in both paths  - <path>". It then
	// compares the two versions and resolves a conflict only if they
	// differ, with lines of its own (msgRenaming, msgNotRenaming,
	// msgSkippedRename).
	msgConflict      = "New or changed in both paths"
	msgRenaming      = "Renaming Path"     // "Renaming Path1 copy": a loser renamed
	msgNotRenaming   = "Not renaming Path" // "Not renaming Path2 copy, as it was determined the winner"
	msgSkippedRename = "Skipped rename as" // the rename of a dry run
	// msgCritical starts the message of an error that ends the run.
	msgCritical = "Bisync critical error: "
	// msgAbortedResync follows a critical error after which rclone renamed
	// the pair's current listings ("-err"). It says a resync is needed, but
	// with --recover the next normal run falls back to the previous
	// listings when they are there (bisyncstate.Pair.Resumable).
	msgAbortedResync = "Bisync aborted. Must run --resync to recover."
	// msgResyncCause ends the first part ("<cause> (must run --resync):
	// <path>") of a critical error that only a resync recovers from: the
	// filters changed, or their hash is missing. rclone does not rename the
	// listings then (--resilient), and every run fails the same way.
	msgResyncCause = " (must run --resync)"
	// The critical errors of a run that finds no listings to compare
	// against (msgNoListings) or an empty one (the critical error and the
	// line before it).
	msgNoListings    = "cannot find prior Path1 or Path2 listings"
	msgEmptyPrior    = "Empty prior Path"
	msgEmptyPriorErr = "empty prior Path"
	msgTooMany       = "too many deletes ("
	msgSafetyAbort   = "Safety abort: "
	msgForceSuffix   = ". Run with --force if desired."
	msgEmptySide     = "Empty current Path" // "Empty current Path1 listing. Cannot sync to an empty directory: ..."
	msgAccess        = "Access test failed: "
	msgRootMissing   = "error reading source root directory: "
	msgDirNotFound   = "directory not found"
	// msgPriorLock is rclone's final message when it found the lock file of
	// another run: "Failed to bisync: prior lock file found: <path> ...".
	msgPriorLock = "Failed to bisync: prior lock file found"
)

// indentTagWidth is the width of the tag in a line about one file.
const indentTagWidth = 18

// indented splits a line that rclone bisync logs about one file, written
// as "- %-18s%-43s - %s" (a tag such as "WARNING" or "Path1", the message,
// the file), into its message and file. ok is false for any other line.
// The file is "" when the line has none.
func indented(msg string) (text, file string, ok bool) {
	rest, ok := strings.CutPrefix(msg, "- ")
	if !ok || len(rest) < indentTagWidth || strings.Contains(strings.TrimRight(rest[:indentTagWidth], " "), " ") {
		return "", "", false
	}
	text, file, _ = strings.Cut(rest[indentTagWidth:], " - ")
	return strings.TrimSpace(text), file, true
}

// bisyncLog reads rclone bisync's log lines (through tracker.observe, on
// the Run goroutine): the conflicts of a run and the messages that say why
// it stopped.
type bisyncLog struct {
	sc *scrub.Scrubber
	// stateDir is the pair's state directory; the messages that name a
	// file there are cut before its path.
	stateDir string

	conflicts     int64
	conflictPaths []string
	// pending is the path of the last msgConflict line that no resolution
	// line followed yet, and hasPending whether there is one.
	pending    string
	hasPending bool

	// critical is the last critical error, first line only.
	critical string
	// stateUnusable: the critical error says only a resync recovers,
	// whatever the listings (msgResyncCause, an empty prior listing).
	stateUnusable bool
	// noListings: rclone found no listings to compare against
	// (msgNoListings).
	noListings bool
	// aborted: rclone renamed the listings after a critical error
	// (msgAbortedResync).
	aborted bool
	// safety are the delete-limit and "all files changed" messages.
	safety []string
	// emptySide is the side ("Path1", "Path2") with an empty listing.
	emptySide string
	// access is the access test's counts ("Path1 count 0, Path2 count 0").
	access string
	// rootMissing is the listing error of a side that does not exist, and
	// dirNotFound whether the critical error was "directory not found".
	rootMissing string
	dirNotFound bool
	// priorLock: rclone found a lock file of another run.
	priorLock bool
}

// line handles one JSON log line of rclone.
func (l *bisyncLog) line(ll logLine) {
	msg := ll.Msg
	text, file, isFileLine := indented(msg)
	switch {
	case isFileLine && text == msgConflict:
		// A pending file that got no resolution had equal versions.
		l.pending, l.hasPending = conflictPath(file), true
	case isFileLine && (strings.HasPrefix(text, msgRenaming) || strings.HasPrefix(text, msgNotRenaming)),
		strings.HasPrefix(msg, msgSkippedRename):
		if l.hasPending {
			l.addConflict(l.pending)
			l.hasPending = false
		}
	case strings.HasPrefix(msg, msgCritical):
		l.critical, _, _ = strings.Cut(strings.TrimPrefix(msg, msgCritical), "\n")
		l.critical = strings.TrimSpace(l.critical)
		l.dirNotFound = l.dirNotFound || l.critical == msgDirNotFound
		cause, _, _ := strings.Cut(l.critical, ": ")
		switch {
		case strings.HasSuffix(cause, msgResyncCause), strings.HasPrefix(l.critical, msgEmptyPriorErr):
			l.stateUnusable = true
		case strings.HasPrefix(l.critical, msgNoListings):
			l.noListings = true
		}
	case msg == msgAbortedResync:
		l.aborted = true
	case strings.HasPrefix(msg, msgEmptyPrior):
		l.stateUnusable = true
	case strings.HasPrefix(msg, msgTooMany), strings.HasPrefix(msg, msgSafetyAbort):
		l.safety = append(l.safety, strings.TrimSuffix(strings.TrimPrefix(msg, msgSafetyAbort), msgForceSuffix))
	case strings.HasPrefix(msg, msgEmptySide):
		if l.emptySide == "" {
			l.emptySide, _, _ = strings.Cut(strings.TrimPrefix(msg, "Empty current "), " ")
		}
	case strings.HasPrefix(msg, msgAccess):
		if l.access == "" {
			l.access = strings.TrimSuffix(strings.TrimPrefix(msg, msgAccess), " - RCLONE_TEST")
		}
	case strings.HasPrefix(msg, msgRootMissing) && strings.HasSuffix(msg, msgDirNotFound):
		l.rootMissing = msg
		if ll.Object != "" {
			l.rootMissing = ll.Object + ": " + msg
		}
	case strings.HasPrefix(msg, msgPriorLock):
		l.priorLock = true
	}
}

// addConflict counts a conflict and keeps its path, scrubbed, as valid
// UTF-8 of at most maxConflictPathBytes, while there is room.
func (l *bisyncLog) addConflict(path string) {
	l.conflicts++
	if path != "" && len(l.conflictPaths) < maxConflictPaths {
		l.conflictPaths = append(l.conflictPaths, clipText(runner.ValidText(l.sc.String(path)), maxConflictPathBytes))
	}
}

// conflictPath returns the path of a msgConflict line from the file part
// of the line (indented), unquoted when rclone quoted it (names with quotes
// or control characters).
func conflictPath(file string) string {
	if strings.HasPrefix(file, `"`) {
		if u, err := strconv.Unquote(file); err == nil {
			return u
		}
	}
	return file
}

// clipText shortens valid UTF-8 text to at most n bytes on a rune
// boundary, ending in "..." when it was cut.
func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len(scrub.Ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + scrub.Ellipsis
}

// sideName names rclone's side ("Path1", "Path2") for people, as the UI
// does (Folder A and Folder B), or returns "" for anything else.
func sideName(side string) string {
	switch side {
	case "Path1":
		return "Folder A (path1)"
	case "Path2":
		return "Folder B (path2)"
	}
	return ""
}

// pairNext is what a pair's state on the worker lets its next run do, read
// after a run that rclone stopped with a critical error.
type pairNext int

const (
	// nextNeedsResync: no listings to go on from; only a resync helps.
	nextNeedsResync pairNext = iota
	// nextResyncs: no baseline (bisyncstate.Pair.Fresh), so the next run
	// resyncs on its own.
	nextResyncs
	// nextContinues: listings a normal run goes on from
	// (bisyncstate.Pair.Resumable), the previous ones after a critical
	// error.
	nextContinues
)

// failedRun is what failure needs to know of a failed run besides rclone's
// messages.
type failedRun struct {
	// next is what the pair's state lets the next run do.
	next pairNext
	// resync and dryRun say whether the run was a resync and a dry run.
	resync, dryRun bool
}

// failure returns the job error that the messages of a failed run call
// for, scrubbed, and false when they call for none (the exit code decides
// then). Checked in this order:
//
//   - a lock file of another run: INTERNAL (Syncr removes stale ones);
//   - the access test failed: SIDE_UNAVAILABLE, which force does not
//     override. It comes before the resync cases because a resync that
//     fails it also says "Must run --resync", which cannot help: the
//     RCLONE_TEST files are missing (a fresh pair stays fresh, so its next
//     run resyncs again once they are there);
//   - the filters changed or a prior listing is empty: RESYNC_REQUIRED;
//   - rclone found no listings, or renamed them after a critical error
//     ("Must run --resync to recover"): what the pair's state allows
//     (run.next). If the next run can go on from listings rclone kept, or
//     resyncs on its own, the failure is EXECUTION_FAILED with what to
//     fix; only without either is it RESYNC_REQUIRED;
//   - a side is empty: SIDE_UNAVAILABLE;
//   - a side does not exist: NOT_FOUND;
//   - the delete limit or "all files changed": SAFETY_ABORT, which force
//     overrides.
func (l *bisyncLog) failure(run failedRun) (syncrv1.JobErrorCode, string, bool) {
	var code syncrv1.JobErrorCode
	var msg string
	switch {
	case l.priorLock:
		code = syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL
		msg = "rclone found a lock file of another run of this pair (prior lock file found), although the worker " +
			"removes stale ones before each run; another rclone process may be using the pair's state"
	case l.access != "":
		code = syncrv1.JobErrorCode_JOB_ERROR_CODE_SIDE_UNAVAILABLE
		msg = "check_access failed: RCLONE_TEST files not found on both sides (" + l.access + "); is the drive mounted?"
	case l.stateUnusable:
		code = syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED
		msg = "a resync is required: " + l.resyncReason() + "; " + resyncTail
	case l.noListings || l.aborted:
		code, msg = l.criticalFailure(run)
	case l.emptySide != "":
		code = syncrv1.JobErrorCode_JOB_ERROR_CODE_SIDE_UNAVAILABLE
		side := sideName(l.emptySide)
		if side == "" {
			side = "a side"
		}
		msg = side + " is empty; refusing to sync it with the other side (is the drive mounted and the path right?)"
	case l.rootMissing != "" || l.dirNotFound:
		code = syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND
		msg = "a side of the two-way sync does not exist"
		if l.rootMissing != "" {
			msg += ": " + l.rootMissing
		}
	case len(l.safety) > 0:
		code = syncrv1.JobErrorCode_JOB_ERROR_CODE_SAFETY_ABORT
		msg = "safety check stopped the two-way sync: " + strings.Join(l.safety, "; ") +
			"; if these changes are intended, run it once with force"
	default:
		return 0, "", false
	}
	return code, l.sc.Line(msg, maxBisyncMessageBytes), true
}

// resyncTail ends the message of RESYNC_REQUIRED.
const resyncTail = "run it once with resync to rebuild the pair's state from both sides as they are now"

// criticalFailure is the failure of a run after which rclone asks for a
// resync because it found no listings or renamed them (critical error):
// with --recover, a normal run goes on from the previous listings when
// they are there, and a pair without a baseline resyncs on its own, so a
// resync is required only without either. Whatever stopped the run (an
// unreadable file, say) stops a resync too, so every message says to fix
// it first.
func (l *bisyncLog) criticalFailure(run failedRun) (syncrv1.JobErrorCode, string) {
	reason := l.resyncReason()
	what := "the two-way sync"
	if run.resync {
		what = "the resync"
	}
	failed := syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED
	switch {
	case run.next == nextContinues && l.noListings && run.dryRun:
		// A dry run looks for listings of its own (".lst-dry"), which a
		// critical error leaves behind only when an earlier dry run
		// made them; a normal run falls back to the previous listings.
		return failed, "the dry run stopped: " + reason +
			"; a dry run cannot go on from the state rclone kept after an earlier critical error, but a normal run can: " +
			"fix what made the earlier run fail, then run it once without dry run"
	case run.next == nextContinues:
		return failed, what + " stopped with a critical error: " + reason +
			"; fix the cause and run it again: the next run goes on from the pair's last good state, without a resync"
	case run.next == nextResyncs:
		return failed, what + " stopped with a critical error: " + reason +
			"; fix the cause and run it again: the pair has no state on this worker yet, so its next run resyncs"
	case l.noListings:
		return syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED, "a resync is required: " + reason +
			"; fix what made the earlier run fail first (a resync stops at the same error), then " + resyncTail
	default:
		return syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED, "a resync is required: " + reason +
			", and the pair has no earlier state to go on from; fix the cause first (a resync stops at the same error), then " + resyncTail
	}
}

// maxBisyncMessageBytes bounds a failure message built from rclone's
// messages (the agent clips error messages at 4 KiB).
const maxBisyncMessageBytes = 1000

// resyncReason is the critical error without its pointer to rclone's own
// flag and without a path in the pair's state directory, or a generic text.
func (l *bisyncLog) resyncReason() string {
	r := l.critical
	if cause, _, _ := strings.Cut(r, ": "); strings.HasSuffix(cause, msgResyncCause) {
		// What follows is the filters file, in the pair's state directory.
		r = strings.TrimSuffix(cause, msgResyncCause)
	}
	if l.stateDir != "" {
		if i := strings.Index(r, ": "+l.stateDir); i >= 0 {
			r = r[:i]
		}
	}
	if r = strings.TrimSpace(r); r == "" {
		return "rclone stopped with a critical error"
	}
	return r
}
