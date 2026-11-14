package rclonetool

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

// LogLine is the subset of an rclone --use-json-log line the runners use.
// Every other key is ignored, and so are type changes in keys the runners
// do not use: rclone adds fields over time, and v1 of Syncr broke by
// decoding rclone's output strictly.
type LogLine struct {
	Level  string     `json:"level"`
	Msg    string     `json:"msg"`
	Object string     `json:"object"`
	Stats  *StatsLine `json:"stats"`
}

// StatsLine holds rclone's cumulative counters (accounting.StatsInfo).
type StatsLine struct {
	Bytes          Number `json:"bytes"`
	TotalBytes     Number `json:"totalBytes"`
	Transfers      Number `json:"transfers"`
	TotalTransfers Number `json:"totalTransfers"`
	Checks         Number `json:"checks"`
	Deletes        Number `json:"deletes"`
	Renames        Number `json:"renames"`
	Errors         Number `json:"errors"`
	ElapsedTime    Number `json:"elapsedTime"` // seconds
}

// Number decodes any JSON number (integer or float, possibly in exponent
// form) and treats null, strings and other types as zero, so an unexpected
// value in one counter never discards a whole stats line.
type Number float64

// UnmarshalJSON implements json.Unmarshaler. It never fails.
func (n *Number) UnmarshalJSON(b []byte) error {
	f, err := strconv.ParseFloat(string(b), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		*n = 0
		return nil
	}
	*n = Number(f)
	return nil
}

// Int64 converts n to a non-negative int64, saturating.
func (n Number) Int64() int64 {
	switch {
	case n <= 0:
		return 0
	case n >= math.MaxInt64:
		return math.MaxInt64
	}
	return int64(n)
}

// Duration converts n, a number of seconds, to a duration; 0 when it is
// not positive or too large.
func (n Number) Duration() time.Duration {
	if n <= 0 || float64(n) >= math.MaxInt64/float64(time.Second) {
		return 0
	}
	return time.Duration(float64(n) * float64(time.Second))
}

// MaxDiagnostics is how many distinct error lines a Tracker keeps for the
// failure message, and MaxDiagnosticBytes how long each may be.
const (
	MaxDiagnostics     = 3
	MaxDiagnosticBytes = 300
)

// Tracker consumes rclone's output lines: it turns stats lines into
// cumulative progress and remembers the last error lines, scrubbed of
// secrets. Its methods are called from proc callbacks, which run
// sequentially on the Run goroutine.
type Tracker struct {
	emit     runner.EmitFunc
	sc       *scrub.Scrubber
	progress runner.Progress
	diag     []string
	// Observe, if set, sees every JSON log line after the tracker took its
	// stats and diagnostics (the two-way sync runner reads rclone bisync's
	// conflicts and abort messages there).
	Observe func(LogLine)
}

// NewTracker returns a Tracker that emits each stats sample to emit (nil
// discards them) and scrubs the error lines it keeps with sc (nil keeps
// them as they are, shortened).
func NewTracker(emit runner.EmitFunc, sc *scrub.Scrubber) *Tracker {
	if emit == nil {
		emit = func(runner.Progress) {}
	}
	return &Tracker{emit: emit, sc: sc}
}

// Line handles one output line from either stream.
func (t *Tracker) Line(l proc.Line) {
	data := bytes.TrimSpace(l.Data)
	if len(data) == 0 {
		return
	}
	var ll LogLine
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
		t.Note(msg)
	}
	if t.Observe != nil {
		t.Observe(ll)
	}
}

// stats merges a stats sample into the cumulative progress and emits it.
// Counters never decrease: rclone resets its error count between retries
// of the whole operation.
func (t *Tracker) stats(s *StatsLine) {
	p := &t.progress
	p.Bytes = max(p.Bytes, s.Bytes.Int64())
	p.Files = max(p.Files, s.Transfers.Int64())
	p.Checks = max(p.Checks, s.Checks.Int64())
	p.Deletes = max(p.Deletes, s.Deletes.Int64())
	p.Renames = max(p.Renames, s.Renames.Int64())
	p.Errors = max(p.Errors, s.Errors.Int64())
	p.Elapsed = max(p.Elapsed, s.ElapsedTime.Duration())
	p.TotalBytes = s.TotalBytes.Int64()
	p.TotalFiles = s.TotalTransfers.Int64()
	t.emit(t.progress)
}

// plain handles a line that is not a JSON log line. rclone writes a few
// messages before its logger is set up (bad flags, unusable config) and Go
// runtime failures this way; other plain lines (usage text) are dropped.
func (t *Tracker) plain(s string) {
	switch {
	case strings.HasPrefix(s, "Error:"), strings.HasPrefix(s, "panic:"), strings.HasPrefix(s, "fatal error:"):
		t.Note(s)
	case strings.Contains(s, " CRITICAL: "), strings.Contains(s, " ERROR : "):
		// "2026/11/04 04:06:14 CRITICAL: msg": drop the timestamp.
		if i := strings.Index(s, "CRITICAL: "); i >= 0 {
			s = s[i+len("CRITICAL: "):]
		} else if i := strings.Index(s, "ERROR : "); i >= 0 {
			s = s[i+len("ERROR : "):]
		}
		t.Note(s)
	}
}

// Note remembers an error message, keeping the last MaxDiagnostics distinct
// ones (retries repeat the same errors). The message is scrubbed whole
// before it is collapsed to one line and shortened, so neither step can
// change or split a secret so that it escapes scrubbing.
func (t *Tracker) Note(msg string) {
	msg = t.sc.Line(msg, MaxDiagnosticBytes)
	for i, d := range t.diag {
		if d == msg {
			t.diag = append(t.diag[:i], t.diag[i+1:]...)
			break
		}
	}
	t.diag = append(t.diag, msg)
	if len(t.diag) > MaxDiagnostics {
		t.diag = t.diag[len(t.diag)-MaxDiagnostics:]
	}
}

// Diagnostics returns the remembered error messages, oldest first.
func (t *Tracker) Diagnostics() []string { return t.diag }

// Progress returns the cumulative progress so far.
func (t *Tracker) Progress() runner.Progress { return t.progress }
