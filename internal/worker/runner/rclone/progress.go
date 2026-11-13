package rclone

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
	"github.com/Zeptiny/Syncr/internal/worker/runner/scrub"
)

// logLine is the subset of an rclone --use-json-log line the runner uses.
// Every other key is ignored, and so are type changes in keys the runner
// does not use: rclone adds fields over time, and v1 of Syncr broke by
// decoding rclone's output strictly.
type logLine struct {
	Level  string     `json:"level"`
	Msg    string     `json:"msg"`
	Object string     `json:"object"`
	Stats  *statsLine `json:"stats"`
}

// statsLine holds rclone's cumulative counters (accounting.StatsInfo).
type statsLine struct {
	Bytes          number `json:"bytes"`
	TotalBytes     number `json:"totalBytes"`
	Transfers      number `json:"transfers"`
	TotalTransfers number `json:"totalTransfers"`
	Checks         number `json:"checks"`
	Deletes        number `json:"deletes"`
	Renames        number `json:"renames"`
	Errors         number `json:"errors"`
	ElapsedTime    number `json:"elapsedTime"` // seconds
}

// number decodes any JSON number (integer or float, possibly in exponent
// form) and treats null, strings and other types as zero, so an unexpected
// value in one counter never discards a whole stats line.
type number float64

// UnmarshalJSON implements json.Unmarshaler. It never fails.
func (n *number) UnmarshalJSON(b []byte) error {
	f, err := strconv.ParseFloat(string(b), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		*n = 0
		return nil
	}
	*n = number(f)
	return nil
}

// int64 converts n to a non-negative int64, saturating.
func (n number) int64() int64 {
	switch {
	case n <= 0:
		return 0
	case n >= math.MaxInt64:
		return math.MaxInt64
	}
	return int64(n)
}

func (n number) duration() time.Duration {
	if n <= 0 || float64(n) >= math.MaxInt64/float64(time.Second) {
		return 0
	}
	return time.Duration(float64(n) * float64(time.Second))
}

// maxDiagnostics is how many distinct error lines are kept for the failure
// message, and maxDiagnosticBytes how long each may be.
const (
	maxDiagnostics     = 3
	maxDiagnosticBytes = 300
)

// tracker consumes rclone's output lines: it turns stats lines into
// cumulative progress and remembers the last error lines, scrubbed of
// secrets. Its methods are called from proc callbacks, which run
// sequentially on the Run goroutine.
type tracker struct {
	emit     runner.EmitFunc
	sc       *scrub.Scrubber
	progress runner.Progress
	diag     []string
	// observe, if set, sees every JSON log line after the tracker took its
	// stats and diagnostics (the two-way sync runner reads rclone bisync's
	// conflicts and abort messages there).
	observe func(logLine)
}

func newTracker(emit runner.EmitFunc, sc *scrub.Scrubber) *tracker {
	if emit == nil {
		emit = func(runner.Progress) {}
	}
	return &tracker{emit: emit, sc: sc}
}

// line handles one output line from either stream.
func (t *tracker) line(l proc.Line) {
	data := bytes.TrimSpace(l.Data)
	if len(data) == 0 {
		return
	}
	var ll logLine
	if data[0] != '{' || l.Truncated || json.Unmarshal(data, &ll) != nil {
		t.plain(string(data))
		return
	}
	if ll.Stats != nil {
		t.stats(ll.Stats)
	}
	switch strings.ToLower(ll.Level) {
	case "error", "critical", "alert", "emergency", "fatal":
		msg := ll.Msg
		if ll.Object != "" {
			msg = ll.Object + ": " + msg
		}
		t.note(msg)
	}
	if t.observe != nil {
		t.observe(ll)
	}
}

// stats merges a stats sample into the cumulative progress and emits it.
// Counters never decrease: rclone resets its error count between retries
// of the whole operation.
func (t *tracker) stats(s *statsLine) {
	p := &t.progress
	p.Bytes = max(p.Bytes, s.Bytes.int64())
	p.Files = max(p.Files, s.Transfers.int64())
	p.Checks = max(p.Checks, s.Checks.int64())
	p.Deletes = max(p.Deletes, s.Deletes.int64())
	p.Renames = max(p.Renames, s.Renames.int64())
	p.Errors = max(p.Errors, s.Errors.int64())
	p.Elapsed = max(p.Elapsed, s.ElapsedTime.duration())
	p.TotalBytes = s.TotalBytes.int64()
	p.TotalFiles = s.TotalTransfers.int64()
	t.emit(t.progress)
}

// plain handles a line that is not a JSON log line. rclone writes a few
// messages before its logger is set up (bad flags, unusable config) and Go
// runtime failures this way; other plain lines (usage text) are dropped.
func (t *tracker) plain(s string) {
	switch {
	case strings.HasPrefix(s, "Error:"), strings.HasPrefix(s, "panic:"), strings.HasPrefix(s, "fatal error:"):
		t.note(s)
	case strings.Contains(s, " CRITICAL: "), strings.Contains(s, " ERROR : "):
		// "2026/11/04 04:06:14 CRITICAL: msg": drop the timestamp.
		if i := strings.Index(s, "CRITICAL: "); i >= 0 {
			s = s[i+len("CRITICAL: "):]
		} else if i := strings.Index(s, "ERROR : "); i >= 0 {
			s = s[i+len("ERROR : "):]
		}
		t.note(s)
	}
}

// note remembers an error message, keeping the last maxDiagnostics distinct
// ones (retries repeat the same errors). The message is scrubbed whole
// before it is collapsed to one line and shortened, so neither step can
// change or split a secret so that it escapes scrubbing.
func (t *tracker) note(msg string) {
	msg = t.sc.Line(msg, maxDiagnosticBytes)
	for i, d := range t.diag {
		if d == msg {
			t.diag = append(t.diag[:i], t.diag[i+1:]...)
			break
		}
	}
	t.diag = append(t.diag, msg)
	if len(t.diag) > maxDiagnostics {
		t.diag = t.diag[len(t.diag)-maxDiagnostics:]
	}
}

// diagnostics returns the remembered error messages, oldest first.
func (t *tracker) diagnostics() []string { return t.diag }
