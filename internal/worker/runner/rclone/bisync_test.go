package rclone

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/bisyncstate"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

const testStateKey = "5f0c8d1e2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5"

// hookSupervisor is a fakeSupervisor that also calls during(spec) while the
// child "runs", to look at the pair's state then.
type hookSupervisor struct {
	*fakeSupervisor
	during func(proc.Spec)
}

func (h *hookSupervisor) Run(ctx context.Context, spec proc.Spec) (proc.Result, error) {
	if h.during != nil {
		h.during(spec)
	}
	return h.fakeSupervisor.Run(ctx, spec)
}

type bisyncFixture struct {
	scratch string
	data    string // the only local root
	states  *bisyncstate.Store
	sup     *fakeSupervisor
	hook    *hookSupervisor
	runner  *BisyncRunner
}

func newBisyncFixture(t *testing.T, sup *fakeSupervisor) *bisyncFixture {
	t.Helper()
	base := shortBase(t)
	f := &bisyncFixture{scratch: filepath.Join(base, "scratch"), data: filepath.Join(base, "data"), sup: sup}
	f.hook = &hookSupervisor{fakeSupervisor: sup}
	for _, d := range []string{filepath.Join(f.data, "a"), filepath.Join(f.data, "b")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	policy, err := localfs.NewPolicy([]string{f.data})
	if err != nil {
		t.Fatal(err)
	}
	f.states, err = bisyncstate.New(bisyncstate.Config{Root: filepath.Join(base, "bisync")})
	if err != nil {
		t.Fatal(err)
	}
	f.runner, err = NewBisync(Config{
		Binary:     "/opt/syncr/rclone",
		Supervisor: f.hook,
		Env:        rcloneenv.NewBuilder(policy),
		Paths:      policy,
		ScratchDir: f.scratch,
	}, f.states)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *bisyncFixture) pairDir() string { return filepath.Join(f.states.Root(), testStateKey) }

// job returns a two-way sync of the local folders a and b.
func (f *bisyncFixture) job(mutate ...func(*syncrv1.RcloneBisync)) runner.Job {
	op := &syncrv1.RcloneBisync{
		Path1:    &syncrv1.Location{Path: filepath.Join(f.data, "a")},
		Path2:    &syncrv1.Location{Path: filepath.Join(f.data, "b")},
		StateKey: testStateKey,
	}
	for _, m := range mutate {
		m(op)
	}
	return runner.Job{ID: "0b8a5a8e-6c1f-4a8e-9a43-1d2f3e4c5b6a", Spec: &syncrv1.JobSpec{
		Operation: &syncrv1.JobSpec_RcloneBisync{RcloneBisync: op},
	}}
}

// withBaseline gives the pair listings of an earlier run.
func (f *bisyncFixture) withBaseline(t *testing.T) {
	t.Helper()
	p, err := f.states.Open(testStateKey)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	for _, side := range []string{"path1", "path2"} {
		writeTestFile(t, filepath.Join(p.WorkDir(), "s."+side+".lst"), "# bisync listing v1\n- 7 - - 2026-11-12T20:52:35+0000 \"f1.txt\"\n")
	}
}

// leaveListings replaces the pair's listings with those of both sides with
// each of suffixes, as rclone leaves them (in the hook, while the pair is
// open).
func (f *bisyncFixture) leaveListings(t *testing.T, suffixes ...string) {
	t.Helper()
	work := filepath.Join(f.pairDir(), "work")
	old, err := filepath.Glob(filepath.Join(work, "*.lst*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range old {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	for _, suffix := range suffixes {
		for _, side := range []string{"path1", "path2"} {
			writeTestFile(t, filepath.Join(work, "s."+side+".lst"+suffix), "# bisync listing v1\n- 7 - - 2026-11-12T20:52:35+0000 \"f1.txt\"\n")
		}
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func bisyncOutput(t *testing.T, res runner.Result) *syncrv1.RcloneBisyncOutput {
	t.Helper()
	out := res.Output.GetRcloneBisync()
	if out == nil {
		t.Fatalf("output = %v, want a two-way sync output", res.Output)
	}
	return out
}

func TestBisyncKind(t *testing.T) {
	t.Parallel()
	f := newBisyncFixture(t, &fakeSupervisor{})
	if f.runner.Kind() != "rclone_bisync" || !link.IsKind(f.runner.Kind()) {
		t.Errorf("Kind() = %q", f.runner.Kind())
	}
	if _, err := NewBisync(Config{}, f.states); err == nil {
		t.Error("NewBisync without dependencies succeeded")
	}
	cfg := f.runner.r.cfg
	if _, err := NewBisync(cfg, nil); err == nil || !strings.Contains(err.Error(), "state store") {
		t.Errorf("NewBisync without a state store = %v", err)
	}
}

func TestBisyncFirstRunResyncs(t *testing.T) {
	t.Parallel()
	sup := &fakeSupervisor{lines: fixtureLines(t, "bisync_resync.jsonl"), result: proc.Result{Duration: 3 * time.Second}}
	f := newBisyncFixture(t, sup)
	var filters string
	f.hook.during = func(proc.Spec) {
		data, err := os.ReadFile(filepath.Join(f.pairDir(), "filters.txt"))
		if err != nil {
			t.Error(err)
		}
		filters = string(data)
	}
	job := f.job(func(op *syncrv1.RcloneBisync) {
		op.Options = &syncrv1.RcloneBisyncOptions{Filters: []*syncrv1.FilterRule{exclude("*.tmp"), include("/docs/**")}}
	})
	var emitted []runner.Progress
	res, err := f.runner.Run(t.Context(), job, func(p runner.Progress) { emitted = append(emitted, p) })
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}

	st := bisyncState{workDir: filepath.Join(f.pairDir(), "work"), filtersFile: filepath.Join(f.pairDir(), "filters.txt"), resync: true}
	want := append(bisyncFixed(st), "--resync", "--resync-mode=newer", "--conflict-resolve=none", "--conflict-loser=num", "--max-delete=50",
		"--", filepath.Join(f.data, "a"), filepath.Join(f.data, "b"))
	if !slices.Equal(sup.spec.Args, want) {
		t.Errorf("args =\n%q\nwant\n%q", sup.spec.Args, want)
	}
	if filters != "- *.tmp\n+ /docs/**\n" {
		t.Errorf("filters file while rclone ran = %q", filters)
	}
	// rclone's config and cache are the transfer runner's: in the job's
	// scratch directory, gone afterwards.
	if filepath.Dir(sup.spec.Dir) != f.scratch || filepath.Dir(sup.spec.Env[rcloneenv.EnvConfig]) != sup.spec.Dir {
		t.Errorf("isolation: dir %q, config %q", sup.spec.Dir, sup.spec.Env[rcloneenv.EnvConfig])
	}
	if left, _ := os.ReadDir(f.scratch); len(left) != 0 {
		t.Errorf("scratch dir not cleaned up: %v", left)
	}

	out := bisyncOutput(t, res)
	if out.GetResyncReason() != syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_FIRST_RUN || out.GetConflicts() != 0 || out.GetDryRun() {
		t.Errorf("output = %v", out)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %q", res.Warnings)
	}
	if len(emitted) != 3 || res.Stats.Bytes != 3000028 || res.Stats.Elapsed != 3*time.Second {
		t.Errorf("progress %+v, stats %+v", emitted, res.Stats)
	}
	// The pair's lock is free again, and the run was recorded.
	p, err := f.states.Open(testStateKey)
	if err != nil {
		t.Fatalf("pair still locked: %v", err)
	}
	_ = p.Close()
	if _, err := os.Stat(filepath.Join(f.pairDir(), "last-run")); err != nil {
		t.Error(err)
	}
}

func TestBisyncResyncDecision(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		baseline bool
		resync   bool
		force    bool
		mode     syncrv1.BisyncResyncMode
		reason   syncrv1.BisyncResyncReason
		flags    []string
	}{
		{"normal run", true, false, false, 0, syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_UNSPECIFIED, nil},
		{"requested resync", true, true, false, syncrv1.BisyncResyncMode_BISYNC_RESYNC_MODE_PATH1,
			syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_REQUESTED, []string{"--resync", "--resync-mode=path1"}},
		{"requested resync of a fresh pair", false, true, false, 0,
			syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_REQUESTED, []string{"--resync", "--resync-mode=newer"}},
		{"force", true, false, true, 0, syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_UNSPECIFIED, []string{"--force"}},
		{"first run with force", false, false, true, syncrv1.BisyncResyncMode_BISYNC_RESYNC_MODE_OLDER,
			syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_FIRST_RUN, []string{"--resync", "--force", "--resync-mode=older"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newBisyncFixture(t, &fakeSupervisor{})
			if tt.baseline {
				f.withBaseline(t)
			}
			res, err := f.runner.Run(t.Context(), f.job(func(op *syncrv1.RcloneBisync) {
				op.Resync, op.Force = tt.resync, tt.force
				op.Options = &syncrv1.RcloneBisyncOptions{ResyncMode: tt.mode}
			}), nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := bisyncOutput(t, res).GetResyncReason(); got != tt.reason {
				t.Errorf("reason = %v, want %v", got, tt.reason)
			}
			args := f.sup.spec.Args
			start := slices.Index(args, "--resilient") + 1
			end := slices.Index(args, "--conflict-resolve=none")
			if got := args[start:end]; !slices.Equal(got, tt.flags) && (len(got) != 0 || len(tt.flags) != 0) {
				t.Errorf("flags = %q, want %q", got, tt.flags)
			}
		})
	}
}

// TestBisyncResyncSides: before a resync the runner checks the local
// sides, which rclone does not do during --resync: a missing side is
// NOT_FOUND, and an automatic first run never fills an empty local side
// (RESYNC_REQUIRED, rclone not started). A requested resync fills it, and
// a normal run leaves both checks to rclone.
func TestBisyncResyncSides(t *testing.T) {
	t.Parallel()
	const (
		resync   = syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED
		notFound = syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND
	)
	tests := []struct {
		name     string
		baseline bool
		resync   bool
		dryRun   bool
		remote   bool              // Folder B on a remote
		a, b     map[string]string // the files of the sides; nil: empty
		missing  string            // a side that does not exist ("a" or "b")
		code     syncrv1.JobErrorCode
		message  string
	}{
		{name: "first run, both sides with files", a: map[string]string{"f": "1"}, b: map[string]string{"g": "2"}},
		{name: "first run, a hidden file is content", a: map[string]string{".keep": ""}, b: map[string]string{"g": "2"}},
		{name: "first run, both local sides empty (nothing to fill)"},
		{name: "first run, Folder A empty", b: map[string]string{"g": "2"}, code: resync,
			message: "Folder A (path1) is empty; a first run does not fill an empty folder on its own (a drive that is not mounted looks the same). " +
				"If this is a new machine, run it once with resync to fill it from Folder B (path2); if the folder is on a drive, mount it first"},
		{name: "first run, Folder B empty", a: map[string]string{"f": "1"}, code: resync,
			message: "Folder B (path2) is empty; a first run does not fill an empty folder on its own (a drive that is not mounted looks the same). " +
				"If this is a new machine, run it once with resync to fill it from Folder A (path1)"},
		{name: "first run, Folder A empty, Folder B on a remote", remote: true, code: resync, message: "Folder A (path1) is empty"},
		{name: "first run, Folder A with files, Folder B on a remote", remote: true, a: map[string]string{"f": "1"}},
		{name: "first dry run, Folder A empty", dryRun: true, b: map[string]string{"g": "2"}, code: resync, message: "Folder A (path1) is empty"},
		{name: "first run, Folder A missing", missing: "a", b: map[string]string{"g": "2"}, code: notFound,
			message: "Folder A (path1) does not exist: "},
		{name: "first run, both missing", missing: "ab", code: notFound, message: "Folder A (path1) does not exist"},
		{name: "requested resync fills an empty folder", resync: true, b: map[string]string{"g": "2"}},
		{name: "requested resync of a pair with state", baseline: true, resync: true, b: map[string]string{"g": "2"}},
		{name: "requested resync, Folder B missing", resync: true, missing: "b", a: map[string]string{"f": "1"}, code: notFound,
			message: "Folder B (path2) does not exist: "},
		{name: "normal run, Folder A empty (rclone checks)", baseline: true, b: map[string]string{"g": "2"}},
		{name: "normal run, Folder A missing (rclone checks)", baseline: true, missing: "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newBisyncFixture(t, &fakeSupervisor{})
			if tt.baseline {
				f.withBaseline(t)
			}
			a, b := filepath.Join(f.data, "a"), filepath.Join(f.data, "b")
			for dir, files := range map[string]map[string]string{a: tt.a, b: tt.b} {
				for name, content := range files {
					writeTestFile(t, filepath.Join(dir, name), content)
				}
			}
			for _, side := range tt.missing {
				if err := os.Remove(filepath.Join(f.data, string(side))); err != nil {
					t.Fatal(err)
				}
			}
			job := f.job(func(op *syncrv1.RcloneBisync) {
				op.Resync = tt.resync
				op.Options = &syncrv1.RcloneBisyncOptions{DryRun: tt.dryRun}
				if tt.remote {
					op.Path2 = &syncrv1.Location{Remote: "box", Path: "shared"}
				}
			})
			if tt.remote {
				job.Spec.Remotes = []*syncrv1.RemoteDefinition{sftpRemote("box", "secret-pass")}
			}
			res, err := f.runner.Run(t.Context(), job, nil)
			if tt.code == 0 {
				if err != nil || !f.sup.called {
					t.Fatalf("Run() = %v, started %v; want a run", err, f.sup.called)
				}
				if tt.baseline || slices.Contains(f.sup.spec.Args, "--resync") {
					return
				}
				t.Errorf("args %q lack --resync", f.sup.spec.Args)
				return
			}
			if runner.CodeOf(err) != tt.code || !strings.Contains(err.Error(), tt.message) {
				t.Fatalf("Run() = %v (code %v), want code %v and %q", err, runner.CodeOf(err), tt.code, tt.message)
			}
			if f.sup.called || res.ExitCode != -1 || res.Output != nil {
				t.Errorf("rclone was started (exit code %d, output %v)", res.ExitCode, res.Output)
			}
			// Nothing was created where a side is missing, and the pair
			// stays without a baseline, so the run after the fix is a
			// first run again.
			for _, side := range tt.missing {
				if _, err := os.Stat(filepath.Join(f.data, string(side))); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("side %c: %v", side, err)
				}
			}
			p, err := f.states.Open(testStateKey)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = p.Close() }()
			if fresh, err := p.Fresh(); err != nil || !fresh {
				t.Errorf("Fresh() after the refusal = %v, %v", fresh, err)
			}
		})
	}
}

// TestBisyncRefusesTheStateFolder: a local side may not be the folder of
// the pairs' state, be inside it or hold it, even when local_roots covers
// it, also through a link.
func TestBisyncRefusesTheStateFolder(t *testing.T) {
	t.Parallel()
	f := newBisyncFixture(t, &fakeSupervisor{})
	base := filepath.Dir(f.data)
	// local_roots covers the whole base, the state root included.
	policy, err := localfs.NewPolicy([]string{base})
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.runner.r.cfg
	cfg.Paths, cfg.Env = policy, rcloneenv.NewBuilder(policy)
	r, err := NewBisync(cfg, f.states)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(testStateKey[:8]+"0", "work")
	link := filepath.Join(f.data, "into-state")
	if err := os.MkdirAll(filepath.Join(f.states.Root(), other), 0o700); err != nil {
		t.Fatal(err)
	}
	linked := os.Symlink(filepath.Join(f.states.Root(), other), link) == nil

	for _, tt := range []struct {
		name, path1, path2 string
	}{
		{"another pair's work directory", filepath.Join(f.states.Root(), other), filepath.Join(f.data, "b")},
		{"the state folder", filepath.Join(f.data, "a"), f.states.Root()},
		{"a folder that holds it", base, ""}, // Folder B on a remote
		{"a link into it", link, filepath.Join(f.data, "b")},
	} {
		if tt.path1 == link && !linked {
			continue // no symlinks without privileges on Windows
		}
		job := f.job(func(op *syncrv1.RcloneBisync) {
			op.Path1, op.Path2 = &syncrv1.Location{Path: tt.path1}, &syncrv1.Location{Path: tt.path2}
			if tt.path2 == "" {
				op.Path2 = &syncrv1.Location{Remote: "box", Path: "x"}
			}
		})
		job.Spec.Remotes = []*syncrv1.RemoteDefinition{sftpRemote("box", "secret-pass")}
		_, err := r.Run(t.Context(), job, nil)
		if runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED ||
			!strings.Contains(err.Error(), "the folder where the worker keeps the state of two-way syncs") {
			t.Errorf("%s: Run() = %v (code %v), want PATH_NOT_ALLOWED", tt.name, err, runner.CodeOf(err))
		}
		if f.sup.called {
			t.Fatalf("%s: rclone was started", tt.name)
		}
	}
	if entries, _ := os.ReadDir(f.states.Root()); len(entries) != 1 {
		t.Errorf("state root holds %v, want only the directory the test made", entries)
	}
	// Folders beside it are fine.
	if _, err := r.Run(t.Context(), f.job(), nil); err != nil || !f.sup.called {
		t.Errorf("Run() beside the state folder = %v", err)
	}
}

func TestBisyncClearsStaleLocksFirst(t *testing.T) {
	t.Parallel()
	f := newBisyncFixture(t, &fakeSupervisor{})
	f.withBaseline(t)
	lck := filepath.Join(f.pairDir(), "work", "s.lck")
	writeTestFile(t, lck, `{"PID":"1"}`)
	var seen error
	f.hook.during = func(proc.Spec) { _, seen = os.Stat(lck) }
	res, err := f.runner.Run(t.Context(), f.job(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(seen, os.ErrNotExist) {
		t.Errorf("the stale lock was there while rclone ran: %v", seen)
	}
	// A pair with listings and a killed run's lock is not fresh: no resync.
	if r := bisyncOutput(t, res).GetResyncReason(); r != syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_UNSPECIFIED {
		t.Errorf("reason = %v", r)
	}
}

func TestBisyncPairInUse(t *testing.T) {
	t.Parallel()
	f := newBisyncFixture(t, &fakeSupervisor{})
	p, err := f.states.Open(testStateKey)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	res, err := f.runner.Run(t.Context(), f.job(), nil)
	if runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_TEMPORARY || !strings.Contains(err.Error(), "in progress on this worker") {
		t.Errorf("Run() = %v (code %v), want TEMPORARY", err, runner.CodeOf(err))
	}
	if f.sup.called || res.ExitCode != -1 {
		t.Errorf("rclone started (exit code %d)", res.ExitCode)
	}
}

func TestBisyncRejectsBeforeStarting(t *testing.T) {
	t.Parallel()
	const secret = "TOP-SECRET-PASS"
	long := strings.Repeat("x", 190)
	tests := []struct {
		name    string
		mutate  func(f *bisyncFixture, op *syncrv1.RcloneBisync, spec *syncrv1.JobSpec)
		code    syncrv1.JobErrorCode
		message string
	}{
		{"no state key", func(_ *bisyncFixture, op *syncrv1.RcloneBisync, _ *syncrv1.JobSpec) { op.StateKey = "" },
			syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "state key"},
		{"state key with a path", func(_ *bisyncFixture, op *syncrv1.RcloneBisync, _ *syncrv1.JobSpec) {
			op.StateKey = "../../../../tmp/" + testStateKey[16:]
		}, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "state key"},
		{"upper-case state key", func(_ *bisyncFixture, op *syncrv1.RcloneBisync, _ *syncrv1.JobSpec) {
			op.StateKey = strings.ToUpper(testStateKey)
		}, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "state key"},
		{"invalid options", func(_ *bisyncFixture, op *syncrv1.RcloneBisync, _ *syncrv1.JobSpec) {
			op.Options = &syncrv1.RcloneBisyncOptions{MaxDeletePercent: ptr[int32](0)}
		}, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "max-delete"},
		{"invalid filter", func(_ *bisyncFixture, op *syncrv1.RcloneBisync, _ *syncrv1.JobSpec) {
			op.Options = &syncrv1.RcloneBisyncOptions{Filters: []*syncrv1.FilterRule{exclude("a\nb")}}
		}, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "line break"},
		{"missing path1", func(_ *bisyncFixture, op *syncrv1.RcloneBisync, _ *syncrv1.JobSpec) { op.Path1 = nil },
			syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "path1 is missing"},
		{"path2 outside the roots", func(f *bisyncFixture, op *syncrv1.RcloneBisync, _ *syncrv1.JobSpec) {
			op.Path2 = &syncrv1.Location{Path: filepath.Dir(f.data)}
		}, syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED, "path2"},
		{"undefined remote", func(_ *bisyncFixture, op *syncrv1.RcloneBisync, _ *syncrv1.JobSpec) {
			op.Path2 = &syncrv1.Location{Remote: "nope", Path: "x"}
		}, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "not defined"},
		{"invalid remote", func(_ *bisyncFixture, op *syncrv1.RcloneBisync, spec *syncrv1.JobSpec) {
			spec.Remotes = []*syncrv1.RemoteDefinition{sftpRemote("box", secret), {Alias: "evil", BackendType: "local"}}
			op.Path2 = &syncrv1.Location{Remote: "box", Path: "x"}
		}, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "evil"},
		{"same folder", func(_ *bisyncFixture, op *syncrv1.RcloneBisync, _ *syncrv1.JobSpec) { op.Path2 = op.Path1 },
			syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "inside the other"},
		{"nested folders", func(_ *bisyncFixture, op *syncrv1.RcloneBisync, _ *syncrv1.JobSpec) {
			op.Path2 = &syncrv1.Location{Path: filepath.Join(op.GetPath1().GetPath(), "sub")}
		}, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "inside the other"},
		{"paths too long", func(_ *bisyncFixture, op *syncrv1.RcloneBisync, spec *syncrv1.JobSpec) {
			spec.Remotes = []*syncrv1.RemoteDefinition{sftpRemote("box", secret)}
			op.Path2 = &syncrv1.Location{Remote: "box", Path: long}
		}, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "too long for a two-way sync"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newBisyncFixture(t, &fakeSupervisor{})
			job := f.job()
			tt.mutate(f, job.Spec.GetRcloneBisync(), job.Spec)
			res, err := f.runner.Run(t.Context(), job, nil)
			if err == nil || runner.CodeOf(err) != tt.code || !strings.Contains(err.Error(), tt.message) {
				t.Fatalf("Run() = %v (code %v), want code %v and %q", err, runner.CodeOf(err), tt.code, tt.message)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error leaks the secret: %v", err)
			}
			if f.sup.called || res.ExitCode != -1 {
				t.Errorf("rclone was started (exit code %d)", res.ExitCode)
			}
			// No pair state is created for a spec that is refused.
			if _, err := os.Stat(f.states.Root()); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("state root created: %v", err)
			}
			if left, _ := os.ReadDir(f.scratch); len(left) != 0 {
				t.Errorf("scratch dir not cleaned up: %v", left)
			}
		})
	}
}

func TestBisyncWrongOperation(t *testing.T) {
	t.Parallel()
	f := newBisyncFixture(t, &fakeSupervisor{})
	for _, spec := range []*syncrv1.JobSpec{nil, {}, transferJob(&syncrv1.Location{Path: f.data}, &syncrv1.Location{Path: f.data}).Spec} {
		if _, err := f.runner.Run(t.Context(), runner.Job{ID: "j", Spec: spec}, nil); runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC {
			t.Errorf("spec %v: err = %v", spec, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.runner.Run(ctx, f.job(), nil); !runner.Canceled(err) || f.sup.called {
		t.Errorf("Run() with a cancelled context = %v, started %v", err, f.sup.called)
	}
}

// TestBisyncOutcomes runs the recorded rclone outputs through the runner
// with their exit codes. Where it matters, the fake rclone leaves the work
// directory as the real one did (after: the suffixes of the listings of
// both sides when it exits).
func TestBisyncOutcomes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		fixture string
		exit    int
		code    syncrv1.JobErrorCode
		message string
		fresh   bool     // no baseline before the run (a first run's resync)
		after   []string // nil: the baseline stays
	}{
		{"bisync_too_many_deletes.jsonl", 1, syncrv1.JobErrorCode_JOB_ERROR_CODE_SAFETY_ABORT, "too many deletes (>50%, 5 of 8)", false, nil},
		{"bisync_all_changed.jsonl", 1, syncrv1.JobErrorCode_JOB_ERROR_CODE_SAFETY_ABORT, "all files were changed on Path1", false, nil},
		{"bisync_filters_changed.jsonl", 7, syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED, "a resync is required: filters file has changed;", false, nil},
		{"bisync_filters_md5_missing.jsonl", 7, syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED, "filters file md5 hash not found", false, nil},
		// An unreadable file: on the first run after a resync, with nothing
		// to go on from; on a later run, where --recover goes on from the
		// previous listings; on a first run's resync, which leaves the pair
		// without a baseline.
		{"bisync_critical_must_resync.jsonl", 7, syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED,
			"permission denied, and the pair has no earlier state to go on from", false, []string{"-err", "-new"}},
		{"bisync_critical_must_resync.jsonl", 7, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED,
			"permission denied; fix the cause and run it again: the next run goes on from the pair's last good state", false, []string{"-err", "-new", "-old"}},
		{"bisync_critical_must_resync.jsonl", 7, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED,
			"the resync stopped with a critical error: failed to open source object: open /data/a/locked.txt: permission denied; fix the cause and run it again: the pair has no state on this worker yet",
			true, []string{"-new"}},
		{"bisync_no_prior_listings.jsonl", 7, syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED, "cannot find prior Path1 or Path2 listings", false, []string{"-err", "-new"}},
		{"bisync_empty_prior.jsonl", 7, syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED, "empty prior Path1 listing", false, nil},
		{"bisync_empty_current.jsonl", 7, syncrv1.JobErrorCode_JOB_ERROR_CODE_SIDE_UNAVAILABLE, "Folder A (path1) is empty", false, nil},
		{"bisync_check_access.jsonl", 7, syncrv1.JobErrorCode_JOB_ERROR_CODE_SIDE_UNAVAILABLE, "RCLONE_TEST files not found", false, nil},
		{"bisync_missing_side.jsonl", 7, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "does not exist: Local file system at /data/a", false, nil},
		{"bisync_prior_lock.jsonl", 1, syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "prior lock file found", false, nil},
		// A transfer's listing error says the same as a two-way sync's.
		{"missing_source.jsonl", 3, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "does not exist: Local file system at /data/missing", false, nil},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s %v", tt.fixture, tt.after), func(t *testing.T) {
			t.Parallel()
			f := newBisyncFixture(t, &fakeSupervisor{
				lines:  fixtureLines(t, tt.fixture),
				result: proc.Result{ExitCode: tt.exit},
				err:    &proc.ExitError{Path: "rclone", ExitCode: tt.exit, Err: errors.New("exit status")},
			})
			if !tt.fresh {
				f.withBaseline(t)
			}
			if tt.after != nil {
				f.hook.during = func(proc.Spec) { f.leaveListings(t, tt.after...) }
			}
			res, err := f.runner.Run(t.Context(), f.job(), nil)
			if runner.CodeOf(err) != tt.code || !strings.Contains(err.Error(), tt.message) {
				t.Errorf("Run() = %v (code %v), want code %v and %q", err, runner.CodeOf(err), tt.code, tt.message)
			}
			if _, ok := errors.AsType[*proc.ExitError](err); !ok {
				t.Errorf("error does not unwrap to *proc.ExitError")
			}
			if res.ExitCode != tt.exit || res.Output != nil {
				t.Errorf("result = %+v", res)
			}
		})
	}
}

// TestBisyncFallback: output the runner does not know is classified by the
// exit code, as for transfers, with the last error lines appended; the
// message says whether the run was a resync.
func TestBisyncFallback(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		baseline bool
		want     string
	}{
		{false, "rclone bisync --resync (first run of the pair on this worker) failed: fatal error, retrying will not help (exit code 7): Bisync critical error: something new"},
		{true, "rclone bisync failed: fatal error, retrying will not help (exit code 7): Bisync critical error: something new"},
	} {
		f := newBisyncFixture(t, &fakeSupervisor{
			lines:  []string{`{"level":"error","msg":"Bisync critical error: something new"}`},
			result: proc.Result{ExitCode: 7},
			err:    &proc.ExitError{Path: "rclone", ExitCode: 7, Err: errors.New("exit status 7")},
		})
		if tt.baseline {
			f.withBaseline(t)
		}
		_, err := f.runner.Run(t.Context(), f.job(), nil)
		if runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED || err.Error() != tt.want {
			t.Errorf("Run() = %q (code %v), want %q", err, runner.CodeOf(err), tt.want)
		}
	}
	f := newBisyncFixture(t, &fakeSupervisor{
		result: proc.Result{ExitCode: 1},
		err:    &proc.ExitError{Path: "rclone", ExitCode: 1, Err: errors.New("exit status 1")},
	})
	_, err := f.runner.Run(t.Context(), f.job(func(op *syncrv1.RcloneBisync) { op.Resync = true }), nil)
	if want := "rclone bisync --resync failed: error (exit code 1)"; err == nil || err.Error() != want {
		t.Errorf("Run() = %v, want %q", err, want)
	}
}

func TestBisyncConflicts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		fixture string
		opts    *syncrv1.RcloneBisyncOptions
		paths   []string
		warning string
	}{
		{"bisync_conflicts.jsonl", nil, []string{"f2.txt", "new both.txt"},
			`2 files changed on both sides; both versions were kept, renamed with the suffix "conflict"`},
		{"bisync_conflict_resolve_newer.jsonl", &syncrv1.RcloneBisyncOptions{ConflictResolve: syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_NEWER},
			[]string{"f1.txt", "f3.txt"},
			`2 files changed on both sides; the newer version kept the name where that could be decided, and the other version was kept, renamed with the suffix "conflict"`},
		{"bisync_dry_run.jsonl", &syncrv1.RcloneBisyncOptions{DryRun: true}, []string{"f1.txt"},
			"1 file changed on both sides (dry run: nothing was changed)"},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			t.Parallel()
			f := newBisyncFixture(t, &fakeSupervisor{lines: fixtureLines(t, tt.fixture)})
			f.withBaseline(t)
			res, err := f.runner.Run(t.Context(), f.job(func(op *syncrv1.RcloneBisync) { op.Options = tt.opts }), nil)
			if err != nil {
				t.Fatal(err)
			}
			out := bisyncOutput(t, res)
			if out.GetConflicts() != int64(len(tt.paths)) || !slices.Equal(out.GetConflictPaths(), tt.paths) || out.GetDryRun() != tt.opts.GetDryRun() {
				t.Errorf("output = %v", out)
			}
			if !slices.Equal(res.Warnings, []string{tt.warning}) {
				t.Errorf("warnings = %q, want %q", res.Warnings, tt.warning)
			}
		})
	}
}

func TestConflictWarningWinners(t *testing.T) {
	t.Parallel()
	for resolve, want := range map[syncrv1.BisyncConflictResolve]string{
		syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_PATH1:   "the Folder A (path1)'s version",
		syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_PATH2:   "the Folder B (path2)'s version",
		syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_SMALLER: "the smaller version",
	} {
		w := conflictWarning(3, &syncrv1.RcloneBisyncOptions{ConflictResolve: resolve, ConflictSuffix: "mine,theirs"})
		if !strings.HasPrefix(w, "3 files changed on both sides; "+want) || !strings.Contains(w, `"mine,theirs"`) {
			t.Errorf("%v: %q", resolve, w)
		}
	}
}

func TestBisyncSupervisorErrors(t *testing.T) {
	t.Parallel()
	canceled := &proc.CanceledError{Path: "rclone", ExitCode: 130, Cause: context.Canceled}
	tests := []struct {
		name  string
		err   error
		exit  int
		check func(error) bool
	}{
		{"canceled", canceled, 130, func(err error) bool { return errors.Is(err, canceled) && runner.Canceled(err) }},
		{"start", &proc.StartError{Path: "rclone", Err: exec.ErrNotFound}, -1, func(err error) bool {
			return runner.CodeOf(err) == syncrv1.JobErrorCode_JOB_ERROR_CODE_TOOL_UNAVAILABLE
		}},
		{"wait delay after success", exec.ErrWaitDelay, 0, func(err error) bool { return err == nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// A cancelled run that printed rclone's resync message still
			// reports the cancel, not a failure.
			f := newBisyncFixture(t, &fakeSupervisor{
				err: tt.err, result: proc.Result{ExitCode: tt.exit},
				lines: []string{`{"level":"error","msg":"Bisync interrupted. Must run --resync to recover."}`},
			})
			if _, err := f.runner.Run(t.Context(), f.job(), nil); !tt.check(err) {
				t.Errorf("Run() = %v", err)
			}
			// The pair's lock is released in every case.
			p, err := f.states.Open(testStateKey)
			if err != nil {
				t.Fatalf("pair still locked: %v", err)
			}
			_ = p.Close()
		})
	}
}
