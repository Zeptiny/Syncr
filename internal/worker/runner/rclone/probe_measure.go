package rclone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
	"github.com/Zeptiny/Syncr/internal/worker/runner/tally"
)

// scanGrace bounds how long a measuring rclone may take to stop once its
// context ends (the executor's soft deadline) before it is killed: what it
// listed must still reach the controller before the probe's own timeout.
// The listing only reads, so stopping it hard loses nothing.
const scanGrace = time.Second

// Measure measures op.location: the totals of everything below it and of
// each direct child folder, by name, at most maxChildren of them, largest
// first.
//
// It runs "lsjson -R --no-mimetype --no-modtime --use-json-log" and reads
// the listing line by line. rclone 1.75 lists recursively with the
// backend's own recursive listing when it has one (a bucket: one request
// per 1000 objects, the folders it synthesizes printed last), whether or
// not --fast-list is given, and streams the items either way; elsewhere
// it walks folder by folder. --no-modtime spares a request per object on
// backends that keep the time in metadata. The child of an item is the
// first segment of its Path; folders count as folders, everything else as
// files with its size (an unknown size, -1, as 0). rclone skips symbolic
// links on a local upstream (it does not list them without
// --links/--copy-links), so they are not counted.
//
// rclone keeps listing past a folder it cannot read and fails at the end;
// when every error it logged names such an entry, the measurement is kept
// and those entries are counted as unreadable (rclone counts the walk's
// last error once more in its final notice). Any other failure fails the
// probe as List's do.
//
// When ctx's deadline passes (the executor's soft deadline) rclone is
// stopped and Measure returns what it counted with complete=false and no
// error; when ctx is cancelled it fails.
func (p *Prober) Measure(ctx context.Context, probeID string, remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeMeasureRemote, maxChildren int) (*syncrv1.ProbeMeasurement, error) {
	run, err := p.prepare(probeID, remotes, op.GetLocation())
	if err != nil {
		return nil, err
	}
	defer run.done()
	t := tally.New(op.GetLocation().GetPath(), maxChildren)
	onLine := func(l proc.Line) {
		if it, ok := parseMeasureLine(l.Data); ok {
			t.Add(it.Path, it.IsDir, it.Size)
		}
	}
	var errs listErrors
	tr := newTracker(func(runner.Progress) {}, run.sc)
	tr.observe = errs.observe
	pres, err := p.r.cfg.Supervisor.Run(ctx, proc.Spec{
		Path: p.r.cfg.Binary,
		Args: []string{"lsjson", "-R", "--no-mimetype", "--no-modtime", "--use-json-log", "--", run.target},
		Dir:  run.dir, Env: run.env.Vars(), Stdout: onLine, Stderr: probeStderr(tr), GracePeriod: scanGrace,
	})
	switch {
	case err == nil:
		return t.Result(true), nil
	case runner.Canceled(err) && errors.Is(ctx.Err(), context.DeadlineExceeded):
		return t.Result(false), nil
	}
	if _, exited := errors.AsType[*proc.ExitError](err); exited && errs.onlyEntries() {
		for range errs.entries {
			t.Unreadable()
		}
		return t.Result(true), nil
	}
	if oerr := p.r.outcome(err, pres, tr, "lsjson"); oerr != nil {
		return nil, oerr
	}
	return t.Result(true), nil
}

// measureItem is the part of an "rclone lsjson -R" item that Measure reads.
type measureItem struct {
	Path  string `json:"Path"`
	Size  int64  `json:"Size"`
	IsDir bool   `json:"IsDir"`
}

// parseMeasureLine parses one line of a recursive lsjson listing ("[",
// one item per line with a trailing comma on all but the last, "]").
func parseMeasureLine(line []byte) (measureItem, bool) {
	data := bytes.TrimSuffix(bytes.TrimSpace(line), []byte(","))
	if len(data) == 0 || data[0] != '{' {
		return measureItem{}, false
	}
	var it measureItem
	if err := json.Unmarshal(data, &it); err != nil || it.Path == "" {
		return measureItem{}, false
	}
	return it, true
}

// listErrors tells a listing that skipped unreadable entries from a
// failed one, from rclone's JSON log lines: the error-level lines (an
// entry's names it as their object) and the error count of the final
// notice ("Failed to lsjson with 3 errors: ...", or "Failed to lsjson:
// ..." for one).
type listErrors struct {
	entries int // error lines naming an entry
	other   int // error lines naming none (the listed folder itself...)
	// reported is the final notice's count; noticed says there was one.
	reported int
	noticed  bool
}

var failedNotice = regexp.MustCompile(`^Failed to lsjson(?: with (\d+) errors)?: `)

func (e *listErrors) observe(ll logLine) {
	switch strings.ToLower(ll.Level) {
	case "error":
		if ll.Object != "" {
			e.entries++
		} else {
			e.other++
		}
	case "critical", "alert", "emergency", "fatal":
		e.other++
	case "notice":
		if m := failedNotice.FindStringSubmatch(ll.Msg); m != nil {
			e.noticed, e.reported = true, 1
			if m[1] != "" {
				e.reported, _ = strconv.Atoi(m[1])
			}
		}
	}
}

// onlyEntries reports that every error rclone counted was an entry it
// could not read: one error line per entry, and no other error. rclone
// counts the walk's last error a second time when ListJSON returns it, so
// the notice may count one more.
func (e *listErrors) onlyEntries() bool {
	return e.entries > 0 && e.other == 0 && e.noticed && (e.reported == e.entries || e.reported == e.entries+1)
}
