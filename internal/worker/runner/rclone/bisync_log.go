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
const (
	// rclone logs this for every file new or changed on both sides, at
	// NOTICE: "- WARNING  New or changed in both paths  - <path>". It then
	// compares the two versions and resolves a conflict only if they
	// differ, with lines of its own (msgRenaming, msgNotRenaming,
	// msgSkippedRename).
	msgConflict      = "New or changed in both paths"
	msgRenaming      = "Renaming Path"     // "Renaming Path1 copy": a loser renamed
	msgNotRenaming   = "renaming Path"     // "Not renaming Path2 copy, as it was determined the winner"
	msgSkippedRename = "Skipped rename as" // the rename of a dry run
	// msgCritical starts the message of an error that ends the run.
	msgCritical = "Bisync critical error: "
	// The run's state needs a resync: in the critical error ("filters file
	// has changed (must run --resync)") or after it ("Bisync aborted. Must
	// run --resync to recover."); rclone omits the latter under --resilient
	// even where a plain retry cannot help.
	msgMustResync  = "must run --resync"
	msgNoListings  = "cannot find prior Path1 or Path2 listings"
	msgEmptyPrior  = "Empty prior Path"
	msgTooMany     = "too many deletes ("
	msgSafetyAbort = "Safety abort: "
	msgForceSuffix = ". Run with --force if desired."
	msgEmptySide   = "Empty current Path" // "Empty current Path1 listing. Cannot sync to an empty directory: ..."
	msgAccess      = "Access test failed: "
	msgRootMissing = "error reading source root directory: "
	msgDirNotFound = "directory not found"
	msgPriorLock   = "prior lock file found"
)

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
	// resync: rclone said only a resync recovers.
	resync bool
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
	switch {
	case strings.Contains(msg, msgConflict):
		// A pending file that got no resolution had equal versions.
		l.pending, l.hasPending = conflictPath(msg), true
	case strings.Contains(msg, msgRenaming), strings.Contains(msg, msgNotRenaming), strings.HasPrefix(msg, msgSkippedRename):
		if l.hasPending {
			l.addConflict(l.pending)
			l.hasPending = false
		}
	case strings.HasPrefix(msg, msgCritical):
		l.critical, _, _ = strings.Cut(strings.TrimPrefix(msg, msgCritical), "\n")
		l.critical = strings.TrimSpace(l.critical)
		l.dirNotFound = l.dirNotFound || l.critical == msgDirNotFound
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
	}
	if strings.Contains(strings.ToLower(msg), msgMustResync) || strings.Contains(msg, msgNoListings) || strings.HasPrefix(msg, msgEmptyPrior) {
		l.resync = true
	}
	if strings.Contains(msg, msgPriorLock) {
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

// conflictPath returns the path of a msgConflict line: the text after the
// " - " that follows the message (the path itself may contain " - "),
// unquoted when rclone quoted it (names with quotes or control characters).
// It returns "" if the line has no path.
func conflictPath(msg string) string {
	_, rest, _ := strings.Cut(msg, msgConflict)
	_, p, ok := strings.Cut(rest, " - ")
	if !ok {
		return ""
	}
	if strings.HasPrefix(p, `"`) {
		if u, err := strconv.Unquote(p); err == nil {
			p = u
		}
	}
	return p
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

// failure returns the job error that the messages of a failed run call
// for, scrubbed, and false when they call for none (the exit code decides
// then). Checked in this order:
//
//   - a lock file of another run: INTERNAL (Syncr removes stale ones);
//   - only a resync recovers: RESYNC_REQUIRED;
//   - a side is empty or the access test failed: SIDE_UNAVAILABLE, which
//     force does not override;
//   - a side does not exist: NOT_FOUND;
//   - the delete limit or "all files changed": SAFETY_ABORT, which force
//     overrides.
func (l *bisyncLog) failure() (syncrv1.JobErrorCode, string, bool) {
	var code syncrv1.JobErrorCode
	var msg string
	switch {
	case l.priorLock:
		code = syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL
		msg = "rclone found a lock file of another run of this pair (prior lock file found), although the worker " +
			"removes stale ones before each run; another rclone process may be using the pair's state"
	case l.resync:
		code = syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED
		msg = "a resync is required: " + l.resyncReason() +
			"; run it once with resync to rebuild the pair's state from both sides as they are now"
	case l.emptySide != "":
		code = syncrv1.JobErrorCode_JOB_ERROR_CODE_SIDE_UNAVAILABLE
		side := sideName(l.emptySide)
		if side == "" {
			side = "a side"
		}
		msg = side + " is empty; refusing to sync it with the other side (is the drive mounted and the path right?)"
	case l.access != "":
		code = syncrv1.JobErrorCode_JOB_ERROR_CODE_SIDE_UNAVAILABLE
		msg = "check_access failed: RCLONE_TEST files not found on both sides (" + l.access + "); is the drive mounted?"
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

// maxBisyncMessageBytes bounds a failure message built from rclone's
// messages (the agent clips error messages at 4 KiB).
const maxBisyncMessageBytes = 1000

// resyncReason is the critical error without its pointer to rclone's own
// flag and without a path in the pair's state directory, or a generic text.
func (l *bisyncLog) resyncReason() string {
	r := l.critical
	if i := strings.Index(strings.ToLower(r), " ("+msgMustResync+")"); i >= 0 {
		r = r[:i]
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
