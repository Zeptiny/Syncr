package rclone

import (
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
	"github.com/Zeptiny/Syncr/internal/worker/runner/rclonetool"
	"github.com/Zeptiny/Syncr/internal/worker/runner/scrub"
)

// logLine is the subset of an rclone --use-json-log line the runner uses
// (rclonetool.LogLine).
type logLine = rclonetool.LogLine

// maxDiagnostics is how many distinct error lines are kept for the failure
// message, and maxDiagnosticBytes how long each may be.
const (
	maxDiagnostics     = rclonetool.MaxDiagnostics
	maxDiagnosticBytes = rclonetool.MaxDiagnosticBytes
)

// tracker consumes rclone's output lines through the shared reader
// (rclonetool.Tracker): it turns stats lines into cumulative progress and
// remembers the last error lines, scrubbed of secrets. Its methods are
// called from proc callbacks, which run sequentially on the Run goroutine.
type tracker struct {
	core *rclonetool.Tracker
	// progress is the cumulative progress, as last emitted.
	progress runner.Progress
	// observe, if set, sees every JSON log line after the tracker took its
	// stats and diagnostics (the two-way sync runner reads rclone bisync's
	// conflicts and abort messages there).
	observe func(logLine)
}

func newTracker(emit runner.EmitFunc, sc *scrub.Scrubber) *tracker {
	t := &tracker{}
	t.core = rclonetool.NewTracker(func(p runner.Progress) {
		t.progress = p
		if emit != nil {
			emit(p)
		}
	}, sc)
	t.core.Observe = func(l logLine) {
		if t.observe != nil {
			t.observe(l)
		}
	}
	return t
}

// line handles one output line from either stream.
func (t *tracker) line(l proc.Line) { t.core.Line(l) }

// diagnostics returns the remembered error messages, oldest first.
func (t *tracker) diagnostics() []string { return t.core.Diagnostics() }

// note remembers an error message (rclonetool.Tracker.Note).
func (t *tracker) note(msg string) { t.core.Note(msg) }
