//go:build integration

package rclone

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/bisyncstate"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// Two-way sync integration tests run rclone bisync (SYNCR_TEST_RCLONE)
// through the real supervisor, env builder, path policy and state store,
// between two local folders. rclone names its state files after both full
// paths, at most 200 bytes together, so the tests work in a short base
// directory (shortBase).

const (
	reasonNone      = syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_UNSPECIFIED
	reasonFirstRun  = syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_FIRST_RUN
	reasonRequested = syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_REQUESTED
)

type bisyncITest struct {
	base, root, scratch string
	a, b                string // the two sides, below root
	states              *bisyncstate.Store
	cfg                 Config
	runner              *BisyncRunner
}

func newBisyncITest(t *testing.T) *bisyncITest {
	t.Helper()
	bin := os.Getenv("SYNCR_TEST_RCLONE")
	if bin == "" {
		t.Skip("SYNCR_TEST_RCLONE not set")
	}
	base := shortBase(t)
	it := &bisyncITest{base: base, root: filepath.Join(base, "d"), scratch: filepath.Join(base, "s")}
	it.a, it.b = filepath.Join(it.root, "a"), filepath.Join(it.root, "b")
	for _, d := range []string{it.a, it.b} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	policy, err := localfs.NewPolicy([]string{it.root})
	if err != nil {
		t.Fatal(err)
	}
	it.states, err = bisyncstate.New(bisyncstate.Config{Root: filepath.Join(base, "st")})
	if err != nil {
		t.Fatal(err)
	}
	it.cfg = Config{
		Binary:     bin,
		Supervisor: proc.New(proc.Options{GracePeriod: 10 * time.Second}),
		Env:        rcloneenv.NewBuilder(policy),
		Paths:      policy,
		ScratchDir: it.scratch,
	}
	it.runner = it.newRunner(t, it.cfg)
	return it
}

func (it *bisyncITest) newRunner(t *testing.T, cfg Config) *BisyncRunner {
	t.Helper()
	r, err := NewBisync(cfg, it.states)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// workDir is rclone's work directory of the pair of spec.
func (it *bisyncITest) workDir() string {
	return filepath.Join(it.states.Root(), strings.Repeat("ab", 32), bisyncstate.WorkDirName)
}

// spec is a two-way sync of it.a and it.b.
func (it *bisyncITest) spec(opts *syncrv1.RcloneBisyncOptions, mutate ...func(*syncrv1.RcloneBisync)) *syncrv1.JobSpec {
	op := &syncrv1.RcloneBisync{
		Path1:    local(it.a),
		Path2:    local(it.b),
		Options:  opts,
		StateKey: strings.Repeat("ab", 32),
	}
	for _, m := range mutate {
		m(op)
	}
	return &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_RcloneBisync{RcloneBisync: op}}
}

// run runs spec with r (it.runner if nil) and checks that the scratch
// directory is cleaned up.
func (it *bisyncITest) run(ctx context.Context, t *testing.T, r *BisyncRunner, spec *syncrv1.JobSpec, emit runner.EmitFunc) (runner.Result, error) {
	t.Helper()
	if r == nil {
		r = it.runner
	}
	res, err := r.Run(ctx, runner.Job{ID: "it-bisync", Spec: spec}, emit)
	if entries, _ := os.ReadDir(it.scratch); len(entries) != 0 {
		t.Errorf("scratch dir not cleaned up: %v", entries)
	}
	return res, err
}

// sync runs a two-way sync that must succeed and returns its output.
func (it *bisyncITest) sync(t *testing.T, spec *syncrv1.JobSpec) (*syncrv1.RcloneBisyncOutput, runner.Result) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	res, err := it.run(ctx, t, nil, spec, nil)
	if err != nil {
		t.Fatalf("two-way sync: %v (code %v)", err, runner.CodeOf(err))
	}
	return bisyncOutput(t, res), res
}

// start makes the pair's state with a resync someone asked for: the
// first run of a pair whose Folder B starts empty, which an automatic
// first run refuses to fill.
func (it *bisyncITest) start(t *testing.T) {
	t.Helper()
	if out, _ := it.sync(t, it.spec(nil, func(op *syncrv1.RcloneBisync) { op.Resync = true })); out.GetResyncReason() != reasonRequested {
		t.Fatalf("the first resync: %v", out)
	}
}

// fail runs a two-way sync that must fail with code and a message
// containing want.
func (it *bisyncITest) fail(t *testing.T, spec *syncrv1.JobSpec, code syncrv1.JobErrorCode, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	_, err := it.run(ctx, t, nil, spec, nil)
	if err == nil || runner.CodeOf(err) != code || !strings.Contains(err.Error(), want) {
		t.Fatalf("two-way sync = %v (code %v), want code %v and %q", err, runner.CodeOf(err), code, want)
	}
}

// put writes files below dir.
func put(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func remove(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(n))); err != nil {
			t.Fatal(err)
		}
	}
}

// names returns the sorted relative paths of the files below dir.
func names(t *testing.T, dir string) []string {
	t.Helper()
	got := slices.Sorted(func(yield func(string) bool) {
		for k := range tree(t, dir) {
			if !yield(k) {
				return
			}
		}
	})
	return got
}

// inStep checks that both sides hold the same files with the same
// contents, and returns their names.
func (it *bisyncITest) inStep(t *testing.T) []string {
	t.Helper()
	sameTree(t, tree(t, it.b), tree(t, it.a))
	return names(t, it.a)
}

func TestIntegrationBisyncLocal(t *testing.T) {
	it := newBisyncITest(t)
	put(t, it.a, map[string]string{"a1.txt": "from a", "sub/s.txt": "shared", "k1.txt": "1", "k2.txt": "2", "k3.txt": "3", "k4.txt": "4"})
	put(t, it.b, map[string]string{"b1.txt": "from b"})

	// The first run of a pair resyncs: the union of both sides.
	out, _ := it.sync(t, it.spec(nil))
	if out.GetResyncReason() != reasonFirstRun || out.GetConflicts() != 0 {
		t.Errorf("first run output = %v", out)
	}
	if got, want := it.inStep(t), []string{"a1.txt", "b1.txt", "k1.txt", "k2.txt", "k3.txt", "k4.txt", "sub/s.txt"}; !slices.Equal(got, want) {
		t.Errorf("after the first run: %v, want %v", got, want)
	}

	// New and changed files spread in both directions, without a resync.
	put(t, it.a, map[string]string{"new-a.txt": "new on a", "k1.txt": "changed on a"})
	put(t, it.b, map[string]string{"new-b.txt": "new on b", "sub/s.txt": "changed on b"})
	out, _ = it.sync(t, it.spec(nil))
	if out.GetResyncReason() != reasonNone {
		t.Errorf("second run resynced: %v", out)
	}
	it.inStep(t)
	if got := string(tree(t, it.b)["k1.txt"]); got != "changed on a" {
		t.Errorf("k1.txt on b = %q", got)
	}
	if got := string(tree(t, it.a)["sub/s.txt"]); got != "changed on b" {
		t.Errorf("sub/s.txt on a = %q", got)
	}

	// A deletion spreads.
	remove(t, it.b, "a1.txt")
	_, res := it.sync(t, it.spec(nil))
	if got := it.inStep(t); slices.Contains(got, "a1.txt") {
		t.Errorf("a1.txt not deleted: %v", got)
	}
	if res.Stats.Deletes != 1 {
		t.Errorf("deletes = %d, want 1", res.Stats.Deletes)
	}

	// A file changed differently on both sides is a conflict: both versions
	// are kept. One changed the same way on both sides is not.
	put(t, it.a, map[string]string{"k2.txt": "edited on a", "k3.txt": "same edit"})
	put(t, it.b, map[string]string{"k2.txt": "edited on b, longer", "k3.txt": "same edit"})
	out, res = it.sync(t, it.spec(nil))
	if out.GetConflicts() != 1 || !slices.Equal(out.GetConflictPaths(), []string{"k2.txt"}) {
		t.Errorf("conflicts = %d %q, want k2.txt only", out.GetConflicts(), out.GetConflictPaths())
	}
	if len(res.Warnings) != 1 || !strings.HasPrefix(res.Warnings[0], "1 file changed on both sides; both versions were kept") {
		t.Errorf("warnings = %q", res.Warnings)
	}
	got := it.inStep(t)
	if slices.Contains(got, "k2.txt") || !slices.Contains(got, "k2.txt.conflict1") || !slices.Contains(got, "k2.txt.conflict2") {
		t.Errorf("after the conflict: %v", got)
	}
	versions := []string{string(tree(t, it.a)["k2.txt.conflict1"]), string(tree(t, it.a)["k2.txt.conflict2"])}
	if !slices.Equal(versions, []string{"edited on a", "edited on b, longer"}) {
		t.Errorf("conflict versions = %q", versions)
	}

	// A dry run changes nothing and says so.
	put(t, it.a, map[string]string{"dry.txt": "x"})
	out, _ = it.sync(t, it.spec(&syncrv1.RcloneBisyncOptions{DryRun: true}))
	if !out.GetDryRun() || slices.Contains(names(t, it.b), "dry.txt") {
		t.Errorf("dry run: output %v, b has %v", out, names(t, it.b))
	}
	out, _ = it.sync(t, it.spec(nil))
	if out.GetResyncReason() != reasonNone || !slices.Contains(it.inStep(t), "dry.txt") {
		t.Errorf("after the dry run: output %v", out)
	}
}

func TestIntegrationBisyncDeleteLimit(t *testing.T) {
	it := newBisyncITest(t)
	files := map[string]string{}
	for i := range 8 {
		files["f"+string(rune('1'+i))+".txt"] = "file"
	}
	put(t, it.a, files)
	it.start(t)

	remove(t, it.a, "f1.txt", "f2.txt", "f3.txt", "f4.txt", "f5.txt")
	it.fail(t, it.spec(nil), syncrv1.JobErrorCode_JOB_ERROR_CODE_SAFETY_ABORT, "too many deletes (>50%, 5 of 8)")
	if got := len(names(t, it.b)); got != 8 {
		t.Errorf("b has %d files after the abort, want 8", got)
	}
	// A higher limit lets it through; so does force, once.
	it.fail(t, it.spec(&syncrv1.RcloneBisyncOptions{MaxDeletePercent: ptr[int32](60)}),
		syncrv1.JobErrorCode_JOB_ERROR_CODE_SAFETY_ABORT, "too many deletes (>60%")
	out, res := it.sync(t, it.spec(nil, func(op *syncrv1.RcloneBisync) { op.Force = true }))
	if out.GetResyncReason() != reasonNone || res.Stats.Deletes != 5 {
		t.Errorf("forced run: output %v, deletes %d", out, res.Stats.Deletes)
	}
	if got := it.inStep(t); len(got) != 3 {
		t.Errorf("after the forced run: %v", got)
	}
	it.sync(t, it.spec(nil))
}

func TestIntegrationBisyncFilterChange(t *testing.T) {
	it := newBisyncITest(t)
	put(t, it.a, map[string]string{"doc.txt": "d", "k1.txt": "1", "k2.txt": "2"})
	it.start(t)

	filtered := &syncrv1.RcloneBisyncOptions{Filters: []*syncrv1.FilterRule{exclude("*.tmp")}}
	put(t, it.a, map[string]string{"scratch.tmp": "t", "new.txt": "n"})
	// Changing the filters needs a deliberate resync, every time until
	// there is one.
	for range 2 {
		it.fail(t, it.spec(filtered), syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED, "a resync is required: filters file has changed")
	}
	if slices.Contains(names(t, it.b), "new.txt") {
		t.Error("a run that needs a resync changed b")
	}
	out, _ := it.sync(t, it.spec(filtered, func(op *syncrv1.RcloneBisync) { op.Resync = true }))
	if out.GetResyncReason() != reasonRequested {
		t.Errorf("resync output = %v", out)
	}
	if got := names(t, it.b); slices.Contains(got, "scratch.tmp") || !slices.Contains(got, "new.txt") {
		t.Errorf("b after the resync: %v", got)
	}
	out, _ = it.sync(t, it.spec(filtered))
	if out.GetResyncReason() != reasonNone {
		t.Errorf("run after the resync: %v", out)
	}
	// Removing every rule is a change too.
	it.fail(t, it.spec(nil), syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED, "filters file has changed")
}

func TestIntegrationBisyncSideUnavailable(t *testing.T) {
	it := newBisyncITest(t)
	put(t, it.a, map[string]string{"k1.txt": "1", "k2.txt": "2"})
	it.start(t)

	// An empty side (an unmounted drive's mount point) stops the run, even
	// with force; the state is kept, so the run after the fix is normal.
	hidden := filepath.Join(it.base, "hidden")
	if err := os.Rename(it.a, hidden); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(it.a, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, force := range []bool{false, true} {
		it.fail(t, it.spec(nil, func(op *syncrv1.RcloneBisync) { op.Force = force }),
			syncrv1.JobErrorCode_JOB_ERROR_CODE_SIDE_UNAVAILABLE, "Folder A (path1) is empty")
	}
	if got := len(names(t, it.b)); got != 2 {
		t.Errorf("b has %d files, want 2", got)
	}

	// A side that does not exist.
	if err := os.Remove(it.a); err != nil {
		t.Fatal(err)
	}
	it.fail(t, it.spec(nil), syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "a side of the two-way sync does not exist")

	if err := os.Rename(hidden, it.a); err != nil {
		t.Fatal(err)
	}
	if out, _ := it.sync(t, it.spec(nil)); out.GetResyncReason() != reasonNone {
		t.Errorf("run after the fix: %v", out)
	}

	// check_access needs the RCLONE_TEST files on both sides.
	access := &syncrv1.RcloneBisyncOptions{CheckAccess: true}
	it.fail(t, it.spec(access), syncrv1.JobErrorCode_JOB_ERROR_CODE_SIDE_UNAVAILABLE, "RCLONE_TEST files not found")
	put(t, it.a, map[string]string{"RCLONE_TEST": ""})
	put(t, it.b, map[string]string{"RCLONE_TEST": ""})
	it.sync(t, it.spec(access))
}

func TestIntegrationBisyncCheckAccessFirstRun(t *testing.T) {
	it := newBisyncITest(t)
	// A fresh pair with check_access and the RCLONE_TEST file on one side
	// only: the first run's resync fails the access test. rclone also says
	// "Must run --resync", but the missing file is the problem, so the
	// code is SIDE_UNAVAILABLE, and the pair stays fresh.
	put(t, it.a, map[string]string{"RCLONE_TEST": "", "k1.txt": "1"})
	put(t, it.b, map[string]string{"k2.txt": "2"})
	access := &syncrv1.RcloneBisyncOptions{CheckAccess: true}
	it.fail(t, it.spec(access), syncrv1.JobErrorCode_JOB_ERROR_CODE_SIDE_UNAVAILABLE, "RCLONE_TEST files not found")
	put(t, it.b, map[string]string{"RCLONE_TEST": ""})
	if out, _ := it.sync(t, it.spec(access)); out.GetResyncReason() != reasonFirstRun {
		t.Errorf("run once both files are there: %v", out)
	}
	if got := it.inStep(t); len(got) != 3 {
		t.Errorf("in step: %v", got)
	}
}

func TestIntegrationBisyncEmptyStart(t *testing.T) {
	it := newBisyncITest(t)
	// Two empty folders: there is nothing to fill, so the first run
	// resyncs (nothing). rclone cannot sync against its empty listings, so
	// the pair counts as fresh until it has files.
	if out, _ := it.sync(t, it.spec(nil)); out.GetResyncReason() != reasonFirstRun {
		t.Errorf("first run: %v", out)
	}
	// Files on one side: the next run is a first run again, which does not
	// fill the empty side on its own; a resync someone asks for does.
	put(t, it.b, map[string]string{"first.txt": "1", "second.txt": "2"})
	it.fail(t, it.spec(nil), syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED, "Folder A (path1) is empty")
	it.start(t)
	put(t, it.a, map[string]string{"third.txt": "3"})
	if out, _ := it.sync(t, it.spec(nil)); out.GetResyncReason() != reasonNone {
		t.Errorf("run with a baseline: %v", out)
	}
	if got := it.inStep(t); len(got) != 3 {
		t.Errorf("files = %v", got)
	}
}

// TestIntegrationBisyncFirstRunEmptyFolder: an automatic first run never
// fills an empty local folder (an unmounted drive's mount point looks the
// same), and leaves both sides and the pair's state as they were; a folder
// with files resyncs on its own, and a resync someone asks for fills an
// empty one. A missing folder is not created.
func TestIntegrationBisyncFirstRunEmptyFolder(t *testing.T) {
	it := newBisyncITest(t)
	put(t, it.b, map[string]string{"k1.txt": "1", "sub/k2.txt": "2"})
	before := tree(t, it.b)
	noState := func() {
		t.Helper()
		if l, _ := filepath.Glob(filepath.Join(it.workDir(), "*.lst*")); len(l) != 0 {
			t.Errorf("listings after the refusal: %v", l)
		}
	}

	// The drive behind Folder A is not mounted: its mount point is empty.
	want := "Folder A (path1) is empty; a first run does not fill an empty folder on its own"
	for range 2 {
		it.fail(t, it.spec(nil), syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED, want)
	}
	sameTree(t, tree(t, it.b), before)
	if got := names(t, it.a); len(got) != 0 {
		t.Errorf("Folder A was filled: %v", got)
	}
	noState()

	// A folder that does not exist is not created either, not even by a
	// resync someone asked for.
	missing := filepath.Join(it.root, "missing")
	for _, resync := range []bool{false, true} {
		it.fail(t, it.spec(nil, func(op *syncrv1.RcloneBisync) { op.Path1, op.Resync = local(missing), resync }),
			syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "Folder A (path1) does not exist")
	}
	if _, err := os.Stat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the missing folder: %v", err)
	}

	// The drive is mounted: Folder A has files, and the first run resyncs
	// on its own (a union).
	put(t, it.a, map[string]string{"k3.txt": "3"})
	if out, _ := it.sync(t, it.spec(nil)); out.GetResyncReason() != reasonFirstRun {
		t.Errorf("first run with files: %v", out)
	}
	if got := it.inStep(t); !slices.Equal(got, []string{"k1.txt", "k3.txt", "sub/k2.txt"}) {
		t.Errorf("after the first run: %v", got)
	}

	// A new machine: an empty folder against a remote (a union over a
	// folder, standing in for the bucket). Asked for, a resync fills it.
	laptop, readOnly := filepath.Join(it.root, "laptop"), filepath.Join(it.root, "ro")
	for _, d := range []string{laptop, readOnly} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	remote := func(op *syncrv1.RcloneBisync) {
		op.Path1, op.Path2 = local(laptop), &syncrv1.Location{Remote: "r1", Path: "b"}
		op.StateKey = strings.Repeat("cd", 32)
	}
	spec := func(mutate ...func(*syncrv1.RcloneBisync)) *syncrv1.JobSpec {
		s := it.spec(nil, append([]func(*syncrv1.RcloneBisync){remote}, mutate...)...)
		s.Remotes = []*syncrv1.RemoteDefinition{{Alias: "r1", BackendType: "union", UnionUpstreams: []*syncrv1.UnionUpstream{
			{Location: local(it.root)},
			{Location: local(readOnly), Mode: syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_ONLY},
		}}}
		return s
	}
	it.fail(t, spec(), syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED, "If this is a new machine, run it once with resync to fill it from Folder B (path2)")
	if out, _ := it.sync(t, spec(func(op *syncrv1.RcloneBisync) { op.Resync = true })); out.GetResyncReason() != reasonRequested {
		t.Errorf("the resync: %v", out)
	}
	if got := names(t, laptop); !slices.Equal(got, []string{"k1.txt", "k3.txt", "sub/k2.txt"}) {
		t.Errorf("the laptop's folder after the resync: %v", got)
	}
	if out, _ := it.sync(t, spec()); out.GetResyncReason() != reasonNone {
		t.Errorf("the run after the resync: %v", out)
	}

	// With check_access, a resync someone asks for does not fill a folder
	// whose drive is not mounted either.
	access := &syncrv1.RcloneBisyncOptions{CheckAccess: true}
	put(t, it.a, map[string]string{"RCLONE_TEST": ""})
	put(t, it.b, map[string]string{"RCLONE_TEST": ""})
	it.sync(t, it.spec(access))
	hidden := filepath.Join(it.base, "hidden")
	if err := os.Rename(it.a, hidden); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(it.a, 0o700); err != nil {
		t.Fatal(err)
	}
	it.fail(t, it.spec(access, func(op *syncrv1.RcloneBisync) { op.Resync = true }),
		syncrv1.JobErrorCode_JOB_ERROR_CODE_SIDE_UNAVAILABLE, "RCLONE_TEST files not found")
	if got := names(t, it.a); len(got) != 0 {
		t.Errorf("Folder A was filled: %v", got)
	}
}

// unreadable writes the file name below dir without read permission, and
// returns the function that makes it readable again. It skips the test
// where permissions do not stop the file's owner (Windows, root).
func unreadable(t *testing.T, dir, name string) func() {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("file modes do not make a file unreadable on Windows")
	}
	p := filepath.Join(dir, name)
	put(t, dir, map[string]string{name: "cannot be read"})
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	fix := func() {
		if err := os.Chmod(p, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(fix)
	if f, err := os.Open(p); err == nil {
		_ = f.Close()
		t.Skip("this user reads files without read permission")
	}
	return fix
}

// TestIntegrationBisyncCriticalErrorRecovers: an unreadable file stops a
// run with a critical error; rclone asks for a resync, but with --recover
// the next normal run goes on from the previous listings, so the job says
// to fix the cause and run again (EXECUTION_FAILED), and once the file can
// be read a normal run succeeds, without a resync.
func TestIntegrationBisyncCriticalErrorRecovers(t *testing.T) {
	it := newBisyncITest(t)
	put(t, it.a, map[string]string{"k1.txt": "1", "k2.txt": "2", "k3.txt": "3", "k4.txt": "4"})
	it.start(t)
	put(t, it.a, map[string]string{"new.txt": "n"})
	it.sync(t, it.spec(nil)) // a normal run keeps the listings before it

	fix := unreadable(t, it.a, "locked.txt")
	put(t, it.b, map[string]string{"from-b.txt": "b", "k2.txt": "2 changed on b"})
	want := "permission denied; fix the cause and run it again: the next run goes on from the pair's last good state, without a resync"
	for range 2 {
		it.fail(t, it.spec(nil), syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, want)
	}
	if errs, _ := filepath.Glob(filepath.Join(it.workDir(), "*.lst-err")); len(errs) != 2 {
		t.Errorf("listings after the critical errors: %v", errs)
	}

	fix()
	out, _ := it.sync(t, it.spec(nil))
	if out.GetResyncReason() != reasonNone || out.GetConflicts() != 0 {
		t.Errorf("run after the fix: %v", out)
	}
	if got := it.inStep(t); !slices.Equal(got, []string{"from-b.txt", "k1.txt", "k2.txt", "k3.txt", "k4.txt", "locked.txt", "new.txt"}) {
		t.Errorf("after the fix: %v", got)
	}
	if got := string(tree(t, it.a)["k2.txt"]); got != "2 changed on b" {
		t.Errorf("k2.txt on a = %q", got)
	}
	// Deletions spread again.
	remove(t, it.b, "k4.txt")
	if out, _ := it.sync(t, it.spec(nil)); out.GetResyncReason() != reasonNone || slices.Contains(it.inStep(t), "k4.txt") {
		t.Errorf("run after that: %v, files %v", out, names(t, it.a))
	}
}

// TestIntegrationBisyncCriticalErrorNeedsResync: the same error on the
// first run after a resync, before rclone kept previous listings: only a
// resync helps, once the cause is fixed (RESYNC_REQUIRED, which says so).
// On a first run's resync, the pair keeps no baseline, so the run after
// the fix resyncs on its own.
func TestIntegrationBisyncCriticalErrorNeedsResync(t *testing.T) {
	it := newBisyncITest(t)
	put(t, it.a, map[string]string{"k1.txt": "1", "k2.txt": "2"})
	it.start(t)
	fix := unreadable(t, it.a, "locked.txt")
	it.fail(t, it.spec(nil), syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED,
		"permission denied, and the pair has no earlier state to go on from; fix the cause first")
	it.fail(t, it.spec(nil), syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED, "cannot find prior Path1 or Path2 listings")
	fix()
	if out, _ := it.sync(t, it.spec(nil, func(op *syncrv1.RcloneBisync) { op.Resync = true })); out.GetResyncReason() != reasonRequested {
		t.Errorf("the resync: %v", out)
	}
	if out, _ := it.sync(t, it.spec(nil)); out.GetResyncReason() != reasonNone || len(it.inStep(t)) != 3 {
		t.Errorf("run after the resync: %v", out)
	}

	// A first run (a resync) that stops on the file.
	other := filepath.Join(it.root, "c")
	put(t, other, map[string]string{"c.txt": "c"})
	fix = unreadable(t, other, "locked-too.txt")
	first := it.spec(nil, func(op *syncrv1.RcloneBisync) { op.Path1, op.StateKey = local(other), strings.Repeat("ef", 32) })
	it.fail(t, first, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED,
		"the resync stopped with a critical error: failed to open source object")
	it.fail(t, first, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "the pair has no state on this worker yet, so its next run resyncs")
	fix()
	if out, _ := it.sync(t, first); out.GetResyncReason() != reasonFirstRun {
		t.Errorf("first run after the fix: %v", out)
	}
}

func TestIntegrationBisyncEveryOption(t *testing.T) {
	it := newBisyncITest(t)
	put(t, it.a, map[string]string{"k1.txt": "1", "k2.txt": "2", "k3.txt": "3", "skip.tmp": "t", "empty/.keep": ""})
	put(t, it.b, map[string]string{"k1.txt": "1 on b"})
	opts := &syncrv1.RcloneBisyncOptions{
		Transfers:                    ptr[int32](2),
		Checkers:                     ptr[int32](4),
		BandwidthLimitBytesPerSecond: ptr[int64](10 << 20),
		Compare: []syncrv1.BisyncCompare{
			syncrv1.BisyncCompare_BISYNC_COMPARE_SIZE, syncrv1.BisyncCompare_BISYNC_COMPARE_MODTIME, syncrv1.BisyncCompare_BISYNC_COMPARE_CHECKSUM,
		},
		ConflictResolve:    syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_NEWER,
		ConflictLoser:      syncrv1.BisyncConflictLoser_BISYNC_CONFLICT_LOSER_PATHNAME,
		ConflictSuffix:     "mine,theirs",
		MaxDeletePercent:   ptr[int32](90),
		ResyncMode:         syncrv1.BisyncResyncMode_BISYNC_RESYNC_MODE_LARGER,
		CreateEmptySrcDirs: true,
		LowLevelRetries:    ptr[int32](2),
		Filters:            []*syncrv1.FilterRule{exclude("*.tmp"), include("**")},
	}
	// rclone accepts every flag (exit code 2 would be INVALID_SPEC).
	out, _ := it.sync(t, it.spec(opts))
	if out.GetResyncReason() != reasonFirstRun {
		t.Errorf("first run: %v", out)
	}
	if got := string(tree(t, it.a)["k1.txt"]); got != "1 on b" {
		t.Errorf("resync mode larger kept %q", got)
	}
	// A conflict with a winner: the newer version keeps the name, the other
	// is kept with its side's suffix.
	put(t, it.a, map[string]string{"k2.txt": "edited on a"})
	put(t, it.b, map[string]string{"k2.txt": "edited on b"})
	older := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(it.a, "k2.txt"), older, older); err != nil {
		t.Fatal(err)
	}
	out, _ = it.sync(t, it.spec(opts))
	if out.GetConflicts() != 1 {
		t.Errorf("conflicts = %v", out)
	}
	// With two suffixes, pathname names the loser by its side alone.
	if got := tree(t, it.a); string(got["k2.txt"]) != "edited on b" || string(got["k2.txt.mine"]) != "edited on a" {
		t.Errorf("after the conflict a has %v", names(t, it.a))
	}
	// The filters kept skip.tmp on a.
	if got, want := names(t, it.b), []string{"empty/.keep", "k1.txt", "k2.txt", "k2.txt.mine", "k3.txt"}; !slices.Equal(got, want) {
		t.Errorf("b = %v, want %v", got, want)
	}
}

// TestIntegrationBisyncLongPaths: two local paths of 200 bytes together
// work (rclone's state file names stay under 255 bytes; on Windows the work
// directory's paths exceed 260 characters); one byte more is refused before
// rclone starts.
func TestIntegrationBisyncLongPaths(t *testing.T) {
	it := newBisyncITest(t)
	side := func(name byte, n int) string {
		return filepath.Join(it.root, strings.Repeat(string(name), n-len(it.root)-1))
	}
	half := maxBisyncPathBytes / 2
	if len(it.root) > half-10 {
		t.Skipf("the test directory %q is too long for two paths of %d bytes", it.root, half)
	}
	a, b := side('a', half), side('b', half)
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	put(t, a, map[string]string{"x.txt": "x"})
	spec := it.spec(nil, func(op *syncrv1.RcloneBisync) { op.Path1, op.Path2 = local(a), local(b) })
	it.sync(t, it.spec(nil, func(op *syncrv1.RcloneBisync) { op.Path1, op.Path2, op.Resync = local(a), local(b), true }))
	put(t, b, map[string]string{"y.txt": "y"})
	if out, _ := it.sync(t, spec); out.GetResyncReason() != reasonNone {
		t.Errorf("second run: %v", out)
	}
	if got := names(t, a); !slices.Equal(got, []string{"x.txt", "y.txt"}) {
		t.Errorf("a = %v", got)
	}

	longer := side('c', half+1)
	if err := os.MkdirAll(longer, 0o700); err != nil {
		t.Fatal(err)
	}
	it.fail(t, it.spec(nil, func(op *syncrv1.RcloneBisync) { op.Path1, op.Path2 = local(a), local(longer) }),
		syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "too long for a two-way sync")
}

// TestIntegrationBisyncRemoteSide syncs a folder with a remote (a union
// over local folders, which needs no server): remote paths reach rclone
// as "<alias>:<path>".
func TestIntegrationBisyncRemoteSide(t *testing.T) {
	it := newBisyncITest(t)
	upstream, readOnly := filepath.Join(it.root, "u"), filepath.Join(it.root, "ro")
	put(t, upstream, map[string]string{"shared/r.txt": "remote"})
	if err := os.MkdirAll(readOnly, 0o700); err != nil {
		t.Fatal(err)
	}
	put(t, it.a, map[string]string{"l.txt": "local"})
	union := &syncrv1.RemoteDefinition{Alias: "r1", BackendType: "union", UnionUpstreams: []*syncrv1.UnionUpstream{
		{Location: local(upstream)},
		{Location: local(readOnly), Mode: syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_ONLY},
	}}
	spec := it.spec(nil, func(op *syncrv1.RcloneBisync) { op.Path2 = &syncrv1.Location{Remote: "r1", Path: "shared"} })
	spec.Remotes = []*syncrv1.RemoteDefinition{union}
	if out, _ := it.sync(t, spec); out.GetResyncReason() != reasonFirstRun {
		t.Errorf("first run: %v", out)
	}
	if got := names(t, filepath.Join(upstream, "shared")); !slices.Equal(got, []string{"l.txt", "r.txt"}) {
		t.Errorf("remote side = %v", got)
	}
	remove(t, filepath.Join(upstream, "shared"), "r.txt")
	if out, _ := it.sync(t, spec); out.GetResyncReason() != reasonNone {
		t.Errorf("second run: %v", out)
	}
	if got := names(t, it.a); !slices.Equal(got, []string{"l.txt"}) {
		t.Errorf("local side = %v", got)
	}
}

// startBig writes a file of size bytes on side a and returns options that
// make its transfer take a few seconds (2 MiB/s).
//
// The callers cancel at the first progress with bytes, about a second in,
// with a file of 12 MiB: about 6 s of transfer. On SIGINT, rclone 1.75.2
// bisync lets transfers in flight finish for up to 30 s before it cancels
// them, so the graceful stop ends with that file, within proc's 10 s grace
// period. The margin to the end of the transfer matters: when SIGINT
// arrives just as a file's transfer ends, rclone's clean-up removes the
// ".partial" file it is still setting the time of, and it reports that as
// a critical error ("Must run --resync"): the run after it goes on from the
// previous listings, or, right after a resync, needs one. That race is a
// real, rare effect of a cancel (docs/bisync.md), not what these tests are
// about; a 6 MiB file left too little margin on a loaded machine.
func startBig(t *testing.T, it *bisyncITest, size int) *syncrv1.RcloneBisyncOptions {
	t.Helper()
	writeFiles(t, it.a, map[string]int{"big.bin": size})
	return &syncrv1.RcloneBisyncOptions{BandwidthLimitBytesPerSecond: ptr[int64](2 << 20)}
}

// interrupt runs spec with r and cancels it once bytes have been
// transferred; the run must report the cancellation.
func interrupt(t *testing.T, it *bisyncITest, r *BisyncRunner, spec *syncrv1.JobSpec) runner.Result {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	start := time.Now()
	res, err := it.run(ctx, t, r, spec, func(p runner.Progress) {
		if p.Bytes > 0 {
			cancel()
		}
	})
	if !runner.Canceled(err) {
		t.Fatalf("Run() = %v, want a cancellation", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("cancel took %v", elapsed)
	}
	if res.Stats.Bytes == 0 {
		t.Errorf("stats = %+v, want a transfer under way", res.Stats)
	}
	return res
}

// TestIntegrationBisyncInterrupted: a run stopped mid-transfer, gracefully
// (cancel: SIGINT on Unix; Windows kills) or killed (a grace period too
// short for rclone's graceful stop), is followed by a normal run that
// succeeds without a resync. A kill can leave a partial file that the next
// run copies across; the run after that removes it, so the sides are in
// step after two runs.
func TestIntegrationBisyncInterrupted(t *testing.T) {
	for _, tt := range []struct {
		name  string
		grace time.Duration
	}{
		{"cancelled", 10 * time.Second},
		{"killed", 50 * time.Millisecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			it := newBisyncITest(t)
			put(t, it.a, map[string]string{"k1.txt": "1", "k2.txt": "2", "k3.txt": "3"})
			it.start(t)

			opts := startBig(t, it, 12<<20)
			cfg := it.cfg
			cfg.Supervisor = proc.New(proc.Options{GracePeriod: tt.grace})
			res := interrupt(t, it, it.newRunner(t, cfg), it.spec(opts))
			killed := tt.name == "killed" || runtime.GOOS == "windows"
			if !killed && res.ExitCode != 130 {
				t.Errorf("exit code %d, want 130 (rclone stopped gracefully on SIGINT)", res.ExitCode)
			}
			// A killed rclone leaves its lock file; the next run removes it.
			if locks, _ := filepath.Glob(filepath.Join(it.workDir(), "*.lck")); killed != (len(locks) == 1) {
				t.Errorf("lock files after the interruption: %v (killed: %v)", locks, killed)
			}

			for i := range 2 {
				if out, _ := it.sync(t, it.spec(nil)); out.GetResyncReason() != reasonNone {
					t.Errorf("run %d after the interruption resynced: %v", i+1, out)
				}
			}
			if got := it.inStep(t); !slices.Equal(got, []string{"big.bin", "k1.txt", "k2.txt", "k3.txt"}) {
				t.Errorf("files = %v", got)
			}
		})
	}
}

// TestIntegrationBisyncFirstRunInterrupted: a first run (a resync) stopped
// part-way, cancelled or killed, leaves no baseline, so the next run
// resyncs again (rclone: "just run it again"); the one after is normal.
func TestIntegrationBisyncFirstRunInterrupted(t *testing.T) {
	for _, tt := range []struct {
		name  string
		grace time.Duration
	}{
		{"cancelled", 10 * time.Second},
		{"killed", 50 * time.Millisecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			it := newBisyncITest(t)
			put(t, it.a, map[string]string{"k1.txt": "1"})
			put(t, it.b, map[string]string{"k2.txt": "2"})
			opts := startBig(t, it, 12<<20)
			cfg := it.cfg
			cfg.Supervisor = proc.New(proc.Options{GracePeriod: tt.grace})
			interrupt(t, it, it.newRunner(t, cfg), it.spec(opts))

			for i, want := range []syncrv1.BisyncResyncReason{reasonFirstRun, reasonNone} {
				if out, _ := it.sync(t, it.spec(nil)); out.GetResyncReason() != want {
					t.Errorf("run %d after the interruption: %v, want reason %v", i+1, out, want)
				}
			}
			if got := it.inStep(t); !slices.Equal(got, []string{"big.bin", "k1.txt", "k2.txt"}) {
				t.Errorf("files = %v", got)
			}
		})
	}
}

// TestIntegrationBisyncStaleRcloneLock: a lock file rclone left (a worker
// killed with its job) is removed under the pair's lock.
func TestIntegrationBisyncStaleRcloneLock(t *testing.T) {
	it := newBisyncITest(t)
	put(t, it.a, map[string]string{"k1.txt": "1"})
	it.start(t)
	work := it.workDir()
	entries, err := os.ReadDir(work)
	if err != nil {
		t.Fatal(err)
	}
	var session string
	for _, e := range entries {
		if s, ok := strings.CutSuffix(e.Name(), ".path1.lst"); ok {
			session = s
		}
	}
	if session == "" {
		t.Fatalf("no listing in %v", entries)
	}
	lck := filepath.Join(work, session+".lck")
	put(t, work, map[string]string{session + ".lck": `{"Session":"x","PID":"999999","TimeRenewed":"2026-11-12T00:00:00Z","TimeExpires":"2099-01-01T00:00:00Z"}`})
	put(t, it.b, map[string]string{"k2.txt": "2"})
	if out, _ := it.sync(t, it.spec(nil)); out.GetResyncReason() != reasonNone {
		t.Errorf("run with a stale lock: %v", out)
	}
	if _, err := os.Stat(lck); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stale lock left: %v", err)
	}
	it.inStep(t)
}
