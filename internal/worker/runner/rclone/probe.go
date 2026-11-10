package rclone

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	pathpkg "path"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
	"github.com/Zeptiny/Syncr/internal/worker/runner/scrub"
)

// Bounds of the probe operations.
const (
	// defaultListEntries bounds a listing whose request names no bound.
	defaultListEntries = 2000
	// maxAboutBytes bounds the output kept of "rclone about".
	maxAboutBytes = 64 << 10
)

// Prober runs the probe operations list_remote and about with the runner's
// validation, environment, isolation and scrubbing: remotes reach rclone
// only as environment variables, in a private scratch directory. It is safe
// for concurrent use.
type Prober struct{ r *Runner }

// NewProber returns a Prober for cfg.
func NewProber(cfg Config) (*Prober, error) {
	r, err := New(cfg)
	if err != nil {
		return nil, err
	}
	return &Prober{r: r}, nil
}

// probeRun is one prepared rclone probe process.
type probeRun struct {
	dir    string
	env    *rcloneenv.Env
	target string
	sc     *scrub.Scrubber
	done   func()
}

// prepare validates the remotes and the location, which must name a remote.
func (p *Prober) prepare(probeID string, remotes []*syncrv1.RemoteDefinition, loc *syncrv1.Location) (*probeRun, error) {
	if loc.GetRemote() == "" {
		return nil, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "the location names no remote")
	}
	dir, err := p.r.scratchDir("probe-" + probeID)
	if err != nil {
		return nil, runner.Fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, err)
	}
	done := func() { p.r.removeScratch(dir) }
	env, err := p.r.cfg.Env.Build(dir, remotes)
	if err != nil {
		done()
		code := syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC
		if errors.Is(err, rcloneenv.ErrLocalPath) {
			code = syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED
		}
		return nil, runner.Fail(code, err)
	}
	target, err := p.r.location("location", loc, env)
	if err != nil {
		done()
		return nil, err
	}
	return &probeRun{dir: dir, env: env, target: target, sc: scrub.New(env.Secrets()), done: done}, nil
}

// stderr returns the handler of a probe's standard error: the tracker's,
// which keeps error lines, plus rclone's final "Failed to <command>: ..."
// notice, which names the reason when no error line does (a backend that
// cannot report its usage).
func probeStderr(t *tracker) func(proc.Line) {
	const marker = " NOTICE: Failed to "
	return func(l proc.Line) {
		t.line(l)
		if s := string(bytes.TrimSpace(l.Data)); !l.Truncated && !strings.HasPrefix(s, "{") {
			if i := strings.Index(s, marker); i >= 0 {
				t.note("failed to " + s[i+len(marker):])
			}
		}
	}
}

// List lists the direct children of op.location (lsjson --max-depth=1):
// directories first, then by name, at most op.max_entries. A folder that
// does not exist fails with NOT_FOUND where the backend has folders; in a
// bucket, a prefix without objects is an empty folder.
func (p *Prober) List(ctx context.Context, probeID string, remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeListRemote) (*syncrv1.ProbeListing, error) {
	run, err := p.prepare(probeID, remotes, op.GetLocation())
	if err != nil {
		return nil, err
	}
	defer run.done()
	limit := int(op.GetMaxEntries())
	if limit <= 0 {
		limit = defaultListEntries
	}
	args := []string{"lsjson", "--max-depth=1", "--no-mimetype"}
	if op.GetDirsOnly() {
		args = append(args, "--dirs-only")
	}
	args = append(args, "--", run.target)

	// Once the bound is passed the listing is cut short: rclone is stopped.
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	base := op.GetLocation().GetPath()
	out := &syncrv1.ProbeListing{Path: base}
	onLine := func(l proc.Line) {
		e, ok := parseLsjsonLine(l.Data, base)
		if !ok || out.GetTruncated() {
			return
		}
		if len(out.GetEntries()) == limit {
			out.Truncated = true
			stop()
			return
		}
		out.Entries = append(out.Entries, e)
	}
	t := newTracker(func(runner.Progress) {}, run.sc)
	pres, err := p.r.cfg.Supervisor.Run(ctx, proc.Spec{
		Path: p.r.cfg.Binary, Args: args, Dir: run.dir, Env: run.env.Vars(), Stdout: onLine, Stderr: probeStderr(t),
	})
	if err != nil && !out.GetTruncated() {
		if oerr := p.r.outcome(err, pres, t, "lsjson"); oerr != nil {
			return nil, oerr
		}
	}
	sortProbeEntries(out.Entries)
	return out, nil
}

// lsjsonItem is the part of an "rclone lsjson" item that is read.
type lsjsonItem struct {
	Name    string `json:"Name"`
	Size    int64  `json:"Size"`
	ModTime string `json:"ModTime"`
	IsDir   bool   `json:"IsDir"`
}

// parseLsjsonLine parses one line of lsjson's output, which prints "[", one
// item per line with a trailing comma on all but the last, then "]". base is
// the listed path on the remote.
func parseLsjsonLine(line []byte, base string) (*syncrv1.ProbeEntry, bool) {
	data := bytes.TrimSuffix(bytes.TrimSpace(line), []byte(","))
	if len(data) == 0 || data[0] != '{' {
		return nil, false
	}
	var it lsjsonItem
	if err := json.Unmarshal(data, &it); err != nil || it.Name == "" {
		return nil, false
	}
	e := &syncrv1.ProbeEntry{
		Name: runner.ValidText(it.Name),
		Path: runner.ValidText(pathpkg.Join(base, it.Name)),
		Type: syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_FILE,
	}
	if it.IsDir {
		e.Type = syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR
	} else if it.Size > 0 {
		e.Size = it.Size
	}
	if ts, err := time.Parse(time.RFC3339Nano, it.ModTime); err == nil && !ts.IsZero() {
		e.ModTime = timestamppb.New(ts)
	}
	return e, true
}

// sortProbeEntries orders directories first, then by name.
func sortProbeEntries(entries []*syncrv1.ProbeEntry) {
	slices.SortStableFunc(entries, func(a, b *syncrv1.ProbeEntry) int {
		ad, bd := a.GetType() == syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR, b.GetType() == syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR
		if ad != bd {
			if ad {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.GetName(), b.GetName())
	})
}

// About asks the remote for its usage (about --json). A backend that cannot
// report usage yields ProbeUsage.unsupported instead of an error.
func (p *Prober) About(ctx context.Context, probeID string, remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeAbout) (*syncrv1.ProbeUsage, error) {
	run, err := p.prepare(probeID, remotes, op.GetLocation())
	if err != nil {
		return nil, err
	}
	defer run.done()
	var raw []byte
	onLine := func(l proc.Line) {
		if len(raw)+len(l.Data) < maxAboutBytes {
			raw = append(append(raw, l.Data...), '\n')
		}
	}
	t := newTracker(func(runner.Progress) {}, run.sc)
	pres, err := p.r.cfg.Supervisor.Run(ctx, proc.Spec{
		Path: p.r.cfg.Binary, Args: []string{"about", "--json", "--", run.target},
		Dir: run.dir, Env: run.env.Vars(), Stdout: onLine, Stderr: probeStderr(t),
	})
	if err != nil {
		if _, exited := errors.AsType[*proc.ExitError](err); exited && aboutUnsupported(t.diagnostics()) {
			return &syncrv1.ProbeUsage{Unsupported: true}, nil
		}
		if oerr := p.r.outcome(err, pres, t, "about"); oerr != nil {
			return nil, oerr
		}
	}
	var u struct {
		Total, Used, Free, Trashed, Other, Objects *int64
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &u); err != nil {
		return nil, runner.Fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, fmt.Errorf("parse rclone about result: %w", err))
	}
	return &syncrv1.ProbeUsage{Total: u.Total, Used: u.Used, Free: u.Free, Trashed: u.Trashed, Other: u.Other, Objects: u.Objects}, nil
}

// aboutUnsupported reports rclone's message for a backend, or a bucket-based
// remote's root, without usage information.
func aboutUnsupported(diag []string) bool {
	for _, d := range diag {
		if strings.Contains(d, "doesn't support about") || strings.Contains(d, "does not support about") {
			return true
		}
	}
	return false
}
