package restic

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// The scan fixtures are the output of restic 0.19.1 (recorded with
// --no-lock --json, the hosts given with --host; user names, IDs and the
// temporary folder's path replaced) over one repository with four
// snapshots, newest first:
//
//	865ccb3d win-host /C/Users  report.docx changed (18 bytes), "new folder/plan.txt" added, cat.png removed
//	6e6e19e7 win-host /C/Users  /C/Users/ada/Documents/report.docx (10 bytes), /C/Users/ada/Pictures/cat.png
//	db00132f it-host  /srv/data a.txt changed (13 bytes), docs/readme.md removed, docs/new.txt added,
//	                            img1.jpg touched, link became a file, notes.txt became a folder notes/, music/ added
//	aaccfc37 it-host  /srv/data a.txt (4), docs/readme.md (5), docs/deep/e.txt (5), photos/2025/img1.jpg (3000),
//	                            photos/2025/IMG2.JPG (5000), star*.txt (5), starX.txt (2), notes.txt (6),
//	                            a symbolic link "link", an empty folder "empty"
//
// The two Windows-style snapshots were made of the relative path C/Users,
// which restic stores as /C/Users like a snapshot made on Windows.
//
//	ls_recursive.out          ls --recursive -- aaccfc37 /srv/data
//	ls_recursive_root.out     ls --recursive -- aaccfc37 /
//	snapshots_scan.out        snapshots
//	find.out                  find -- a.txt 'star[*].txt'
//	find_ignore_case.out      find --ignore-case -- '*.JPG'
//	find_none.out             find -- no-such-file
//	find_windows.out          find --snapshot 6e6e19e7... --snapshot 865ccb3d... -- /C/Users/ada/Documents/report.docx
//	find_unknown_snapshot.*   find --snapshot deadbeef -- a.txt (stdout, stderr; exit code 0)
//	diff.out                  diff -- aaccfc37... db00132f...
//	diff_folder.out           diff -- aaccfc37...:/srv/data/docs db00132f...:/srv/data/docs
//	diff_windows.out          diff -- 6e6e19e7... 865ccb3d...
//	diff_windows_folder.out   diff -- 6e6e19e7...:/C/Users/ada/Documents 865ccb3d...:/C/Users/ada/Documents
//	diff_missing_folder.err   diff -- aaccfc37...:/srv/data/nope ... (stderr; exit code 1)
const (
	snapL1 = "aaccfc378cedc448695f508ada61bfdded73217851f07cc9d2374370d643390a"
	snapL2 = "db00132fa3811fd9acfb2c5cf52d18f53de1c847bda7685498cb0fe34df8c82b"
	snapW1 = "6e6e19e71383a7fd54af390ed628483531a68ca7693d9343bf9ee0ab483fb94b"
	snapW2 = "865ccb3da8d371cd5820dd82e691eef531c5a6002bc36470f7d191675c50290c"
)

// scanReply is the scripted outcome of one restic process: its standard
// output in pieces (lines, or chunks for find), standard error, and exit
// code.
type scanReply struct {
	stdout, stderr []string
	exit           int
}

// scanCall is one restic process a scanSupervisor ran.
type scanCall struct {
	verb   string
	args   []string
	chunks bool
	grace  time.Duration
}

// scanSupervisor answers each restic process by its verb. Like a stopped
// restic, it stops printing once its context ends: it checks the context
// before each piece of standard output.
type scanSupervisor struct {
	replies map[string]scanReply
	// pause, if set, is waited before each piece of standard output (a
	// slow restic, in a synctest bubble).
	pause time.Duration

	mu    sync.Mutex
	calls []scanCall
	fed   int // pieces of standard output delivered
}

func (s *scanSupervisor) Run(ctx context.Context, spec proc.Spec) (proc.Result, error) {
	c := scanCall{args: spec.Args, chunks: spec.StdoutChunks, grace: spec.GracePeriod}
	for _, a := range spec.Args {
		if slices.Contains([]string{"snapshots", "ls", "find", "diff"}, a) {
			c.verb = a
			break
		}
	}
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
	r := s.replies[c.verb]
	for _, l := range r.stderr {
		spec.Stderr(proc.Line{Data: []byte(l)})
	}
	for _, l := range r.stdout {
		if s.pause > 0 {
			time.Sleep(s.pause)
		}
		if ctx.Err() != nil {
			return proc.Result{ExitCode: 130}, &proc.CanceledError{Path: spec.Path, ExitCode: 130, Cause: context.Cause(ctx)}
		}
		spec.Stdout(proc.Line{Data: []byte(l)})
		s.mu.Lock()
		s.fed++
		s.mu.Unlock()
	}
	if r.exit != 0 {
		return proc.Result{ExitCode: r.exit}, &proc.ExitError{Path: spec.Path, ExitCode: r.exit}
	}
	return proc.Result{}, nil
}

func (s *scanSupervisor) verbs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.calls {
		out = append(out, c.verb)
	}
	return out
}

func newScanProber(t *testing.T, replies map[string]scanReply) (*Prober, *scanSupervisor) {
	t.Helper()
	sup := &scanSupervisor{replies: replies}
	te := newTestEnv(t, func(c *Config) { c.Supervisor = sup })
	p, err := NewProber(te.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p, sup
}

// chunks splits the whole fixture into pieces of n bytes, as restic find
// prints its result without newlines.
func chunks(t *testing.T, name string, n int) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for len(data) > 0 {
		k := min(n, len(data))
		out = append(out, string(data[:k]))
		data = data[k:]
	}
	return out
}

// snapshotsOf returns the snapshots_scan.out listing restricted to ids, as
// "restic snapshots -- <id>..." prints it.
func snapshotsOf(t *testing.T, ids ...string) []string {
	t.Helper()
	var all []map[string]any
	if err := json.Unmarshal([]byte(fixture(t, "snapshots_scan.out")[0]), &all); err != nil {
		t.Fatal(err)
	}
	all = slices.DeleteFunc(all, func(sn map[string]any) bool {
		full, _ := sn["id"].(string)
		return !slices.ContainsFunc(ids, func(id string) bool { return strings.HasPrefix(full, id) })
	})
	data, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	return []string{string(data)}
}

// endAfter is a context whose Err reports err from its n-th call on.
type endAfter struct {
	context.Context
	err   error
	mu    sync.Mutex
	calls int
	n     int
}

func (c *endAfter) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls >= c.n {
		return c.err
	}
	return nil
}

func childLines(m *syncrv1.ProbeMeasurement) []string {
	var out []string
	for _, c := range m.GetChildren() {
		out = append(out, fmt.Sprintf("%s %d %d %d", c.GetName(), c.GetBytes(), c.GetFiles(), c.GetDirs()))
	}
	return out
}

func TestProberMeasure(t *testing.T) {
	t.Parallel()
	p, sup := newScanProber(t, map[string]scanReply{"ls": {stdout: fixture(t, "ls_recursive.out")}})
	op := &syncrv1.ProbeResticMeasure{Repository: remoteRepo(), SnapshotId: "aaccfc37", Path: "/srv/data/"}
	m, err := p.Measure(t.Context(), testProbeID, remotesBk(), op, 10)
	if err != nil {
		t.Fatal(err)
	}
	// 8027 bytes in 8 files and a link; 5 folders.
	if m.GetPath() != "/srv/data" || m.GetBytes() != 8027 || m.GetFiles() != 9 || m.GetDirs() != 5 || !m.GetComplete() ||
		m.GetUnreadable() != 0 || m.GetChildrenTruncated() {
		t.Errorf("measurement = %v", m)
	}
	if want := []string{"photos 8000 2 1", "docs 10 2 1", "empty 0 0 0"}; !slices.Equal(childLines(m), want) {
		t.Errorf("children = %q, want %q", childLines(m), want)
	}
	c := sup.calls[0]
	if want := append(remoteGlobals(true), "--no-lock", "ls", "--recursive", "--", "aaccfc37", "/srv/data"); !slices.Equal(c.args, want) {
		t.Errorf("args = %q\nwant %q", c.args, want)
	}
	if c.grace != scanGrace || c.chunks {
		t.Errorf("grace %v, chunks %v", c.grace, c.chunks)
	}

	// The root, and a bound on the children.
	p, _ = newScanProber(t, map[string]scanReply{"ls": {stdout: fixture(t, "ls_recursive_root.out")}})
	op = &syncrv1.ProbeResticMeasure{Repository: remoteRepo(), SnapshotId: "latest", Path: "/"}
	m, err = p.Measure(t.Context(), testProbeID, remotesBk(), op, 0)
	if err != nil || m.GetBytes() != 8027 || m.GetFiles() != 9 || m.GetDirs() != 7 || len(m.GetChildren()) != 0 || !m.GetChildrenTruncated() {
		t.Errorf("root = %v, %v", m, err)
	}
	m, _ = p.Measure(t.Context(), testProbeID, remotesBk(), op, 5)
	if want := []string{"srv 8027 9 6"}; !slices.Equal(childLines(m), want) {
		t.Errorf("root children = %q, want %q", childLines(m), want)
	}
}

func TestProberMeasureMissingFolder(t *testing.T) {
	t.Parallel()
	// restic lists a missing folder as an empty one: only the snapshot.
	p, _ := newScanProber(t, map[string]scanReply{"ls": {stdout: fixture(t, "ls_recursive.out")[:1]}})
	_, err := p.Measure(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticMeasure{Repository: remoteRepo(), SnapshotId: "aaccfc37", Path: "/nope"}, 5)
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND)
	// A file is not a folder: restic lists the file itself.
	p, _ = newScanProber(t, map[string]scanReply{"ls": {stdout: fixture(t, "ls_recursive.out")[:3]}})
	_, err = p.Measure(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticMeasure{Repository: remoteRepo(), SnapshotId: "aaccfc37", Path: "/srv/data/a.txt"}, 5)
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND)
	// An unknown snapshot.
	p, _ = newScanProber(t, map[string]scanReply{"ls": {exit: 1, stderr: []string{`{"message_type":"exit_error","code":1,"message":"Fatal: no matching ID found for prefix \"deadbeef\""}`}}})
	_, err = p.Measure(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticMeasure{Repository: remoteRepo(), SnapshotId: "deadbeef", Path: "/"}, 5)
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND)
}

func TestProberMeasureStopsWithItsContext(t *testing.T) {
	t.Parallel()
	op := &syncrv1.ProbeResticMeasure{Repository: remoteRepo(), SnapshotId: "aaccfc37", Path: "/srv/data"}
	// The soft deadline after the snapshot, the folder and a.txt: what was
	// found, not complete, and not "not found" either.
	for _, n := range []int{1, 4} {
		p, sup := newScanProber(t, map[string]scanReply{"ls": {stdout: fixture(t, "ls_recursive.out")}})
		m, err := p.Measure(&endAfter{Context: t.Context(), err: context.DeadlineExceeded, n: n}, testProbeID, remotesBk(), op, 10)
		if err != nil || m.GetComplete() || m.GetBytes() != int64(min(n-1, 3)/3*4) || sup.fed != n-1 {
			t.Errorf("deadline after %d: %v, %v (fed %d)", n, m, err, sup.fed)
		}
	}
	p, _ := newScanProber(t, map[string]scanReply{"ls": {stdout: fixture(t, "ls_recursive.out")}})
	_, err := p.Measure(&endAfter{Context: t.Context(), err: context.Canceled, n: 3}, testProbeID, remotesBk(), op, 10)
	if !runner.Canceled(err) {
		t.Errorf("err = %v, want a cancellation", err)
	}
}

func TestProberScansRejectBadInput(t *testing.T) {
	t.Parallel()
	p, sup := newScanProber(t, nil)
	repo := remoteRepo()
	long := strings.Repeat("x", maxPatternBytes+1)
	many := make([]string, maxFindSnapshots+1)
	for i := range many {
		many[i] = fmt.Sprintf("%08x", i)
	}
	runs := map[string]func() error{
		"measure snapshot": func() error {
			_, err := p.Measure(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticMeasure{Repository: repo, SnapshotId: "--help", Path: "/"}, 5)
			return err
		},
		"measure relative": func() error {
			_, err := p.Measure(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticMeasure{Repository: repo, SnapshotId: "latest", Path: "srv"}, 5)
			return err
		},
		"measure dot-dot": func() error {
			_, err := p.Measure(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticMeasure{Repository: repo, SnapshotId: "latest", Path: "/a/../b"}, 5)
			return err
		},
		"diff latest": func() error {
			_, err := p.Diff(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticDiff{Repository: repo, FromSnapshotId: "latest", ToSnapshotId: snapL2})
			return err
		},
		"diff flag": func() error {
			_, err := p.Diff(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticDiff{Repository: repo, FromSnapshotId: snapL1, ToSnapshotId: "--help"})
			return err
		},
		"diff path": func() error {
			_, err := p.Diff(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticDiff{Repository: repo, FromSnapshotId: snapL1, ToSnapshotId: snapL2, Path: "srv/data"})
			return err
		},
		"diff control": func() error {
			_, err := p.Diff(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticDiff{Repository: repo, FromSnapshotId: snapL1, ToSnapshotId: snapL2, Path: "/a\nb"})
			return err
		},
	}
	for name, patterns := range map[string][]string{
		"no pattern": nil, "eleven patterns": slices.Repeat([]string{"x"}, maxFindPatterns+1), "empty pattern": {"a", ""},
		"long pattern": {long}, "invalid UTF-8": {"a\xffb"}, "NUL": {"a\x00b"}, "newline": {"a\nb"}, "carriage return": {"a\rb"},
	} {
		runs["find "+name] = func() error {
			_, err := p.Find(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticFind{Repository: repo, Patterns: patterns})
			return err
		}
	}
	for name, ids := range map[string][]string{"latest": {"latest"}, "flag": {"--help"}, "short": {"abc"}, "too many": many} {
		runs["find snapshots "+name] = func() error {
			_, err := p.Find(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticFind{Repository: repo, Patterns: []string{"x"}, SnapshotIds: ids})
			return err
		}
	}
	for name, run := range runs {
		if err := run(); runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC {
			t.Errorf("%s: err = %v, want INVALID_SPEC", name, err)
		}
	}
	if len(sup.calls) != 0 {
		t.Errorf("restic started for rejected input: %v", sup.verbs())
	}
	// The longest pattern and the most patterns allowed pass.
	p, _ = newScanProber(t, map[string]scanReply{"snapshots": {stdout: []string{"[]"}}, "find": {stdout: []string{"[]"}}})
	ok := append(slices.Repeat([]string{"x"}, maxFindPatterns-1), strings.Repeat("é", maxPatternBytes/2))
	if _, err := p.Find(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticFind{Repository: repo, Patterns: ok}); err != nil {
		t.Errorf("patterns at the bounds: %v", err)
	}
}

func matchLines(f *syncrv1.ResticFindOutput) []string {
	var out []string
	for _, m := range f.GetMatches() {
		e := m.GetEntry()
		out = append(out, fmt.Sprintf("%s %s %s %d", m.GetSnapshotId()[:8], e.GetType().String()[len("PROBE_ENTRY_TYPE_"):], e.GetPath(), e.GetSize()))
	}
	return out
}

func TestProberFind(t *testing.T) {
	t.Parallel()
	for _, size := range []int{1, 7, 100, 1 << 20} {
		p, sup := newScanProber(t, map[string]scanReply{
			"snapshots": {stdout: fixture(t, "snapshots_scan.out")},
			"find":      {stdout: chunks(t, "find.out", size)},
		})
		f, err := p.Find(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticFind{Repository: remoteRepo(), Patterns: []string{"a.txt", "star[*].txt"}})
		if err != nil {
			t.Fatal(err)
		}
		// Two versions of each file, the newest snapshot first; all four
		// snapshots were searched.
		want := []string{
			"db00132f FILE /srv/data/a.txt 13", "db00132f FILE /srv/data/star*.txt 5",
			"aaccfc37 FILE /srv/data/a.txt 4", "aaccfc37 FILE /srv/data/star*.txt 5",
		}
		if got := matchLines(f); !slices.Equal(got, want) || !f.GetComplete() || f.GetTruncated() || f.GetSnapshotsSearched() != 4 {
			t.Errorf("chunks of %d: matches %q, complete %v, truncated %v, searched %d", size, got, f.GetComplete(), f.GetTruncated(), f.GetSnapshotsSearched())
		}
		e := f.GetMatches()[0]
		if e.GetSnapshotId() != snapL2 || e.GetEntry().GetName() != "a.txt" || e.GetEntry().GetMode() != "-rw-r--r--" || e.GetEntry().GetModTime() == nil {
			t.Errorf("match = %v", e)
		}
		if got := sup.verbs(); !slices.Equal(got, []string{"snapshots", "find"}) {
			t.Fatalf("calls = %v", got)
		}
		if want := append(remoteGlobals(true), "--no-lock", "snapshots"); !slices.Equal(sup.calls[0].args, want) {
			t.Errorf("snapshots args = %q\nwant %q", sup.calls[0].args, want)
		}
		if want := append(remoteGlobals(true), "--no-lock", "find", "--", "a.txt", "star[*].txt"); !slices.Equal(sup.calls[1].args, want) {
			t.Errorf("find args = %q\nwant %q", sup.calls[1].args, want)
		}
		if c := sup.calls[1]; !c.chunks || c.grace != scanGrace {
			t.Errorf("find read in chunks %v, grace %v", c.chunks, c.grace)
		}
	}
}

func TestProberFindChosenSnapshots(t *testing.T) {
	t.Parallel()
	// A Windows-style tree, in the two snapshots named by short IDs.
	p, sup := newScanProber(t, map[string]scanReply{
		"snapshots": {stdout: snapshotsOf(t, "6e6e19e7", "865ccb3d")},
		"find":      {stdout: chunks(t, "find_windows.out", 50)},
	})
	f, err := p.Find(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticFind{
		Repository: remoteRepo(), Patterns: []string{"/C/Users/ada/Documents/report.docx"}, IgnoreCase: true, SnapshotIds: []string{"6e6e19e7", "865ccb3d"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"865ccb3d FILE /C/Users/ada/Documents/report.docx 18", "6e6e19e7 FILE /C/Users/ada/Documents/report.docx 10"}
	if got := matchLines(f); !slices.Equal(got, want) || !f.GetComplete() || f.GetSnapshotsSearched() != 2 {
		t.Errorf("matches %q, complete %v, searched %d", got, f.GetComplete(), f.GetSnapshotsSearched())
	}
	if f.GetMatches()[0].GetEntry().GetName() != "report.docx" {
		t.Errorf("name = %q", f.GetMatches()[0].GetEntry().GetName())
	}
	if want := append(remoteGlobals(true), "--no-lock", "snapshots", "--", "6e6e19e7", "865ccb3d"); !slices.Equal(sup.calls[0].args, want) {
		t.Errorf("snapshots args = %q\nwant %q", sup.calls[0].args, want)
	}
	// The full IDs, newest first.
	want = append(remoteGlobals(true), "--no-lock", "find", "--ignore-case", "--snapshot="+snapW2, "--snapshot="+snapW1, "--", "/C/Users/ada/Documents/report.docx")
	if !slices.Equal(sup.calls[1].args, want) {
		t.Errorf("find args = %q\nwant %q", sup.calls[1].args, want)
	}

	// A snapshot the repository does not have (restic snapshots ignores
	// it with a warning): NOT_FOUND, and no search.
	p, sup = newScanProber(t, map[string]scanReply{
		"snapshots": {stdout: snapshotsOf(t, "6e6e19e7"), stderr: fixture(t, "find_unknown_snapshot.err")},
	})
	_, err = p.Find(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticFind{Repository: remoteRepo(), Patterns: []string{"x"}, SnapshotIds: []string{"6e6e19e7", "deadbeef"}})
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND)
	if !strings.Contains(err.Error(), "deadbeef") || slices.Contains(sup.verbs(), "find") {
		t.Errorf("err = %v, calls %v", err, sup.verbs())
	}
}

func TestProberFindIgnoringCase(t *testing.T) {
	t.Parallel()
	p, sup := newScanProber(t, map[string]scanReply{
		"snapshots": {stdout: fixture(t, "snapshots_scan.out")},
		"find":      {stdout: chunks(t, "find_ignore_case.out", 64)},
	})
	f, err := p.Find(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticFind{Repository: remoteRepo(), Patterns: []string{"*.JPG"}, IgnoreCase: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"db00132f FILE /srv/data/photos/2025/IMG2.JPG 5000", "db00132f FILE /srv/data/photos/2025/img1.jpg 3000",
		"aaccfc37 FILE /srv/data/photos/2025/IMG2.JPG 5000", "aaccfc37 FILE /srv/data/photos/2025/img1.jpg 3000",
	}
	if got := matchLines(f); !slices.Equal(got, want) || !f.GetComplete() {
		t.Errorf("matches %q, complete %v", got, f.GetComplete())
	}
	if want := append(remoteGlobals(true), "--no-lock", "find", "--ignore-case", "--", "*.JPG"); !slices.Equal(sup.calls[1].args, want) {
		t.Errorf("find args = %q\nwant %q", sup.calls[1].args, want)
	}
}

func TestProberFindNothing(t *testing.T) {
	t.Parallel()
	p, _ := newScanProber(t, map[string]scanReply{
		"snapshots": {stdout: fixture(t, "snapshots_scan.out")},
		"find":      {stdout: fixture(t, "find_none.out")},
	})
	f, err := p.Find(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticFind{Repository: remoteRepo(), Patterns: []string{"no-such-file"}})
	if err != nil || len(f.GetMatches()) != 0 || !f.GetComplete() || f.GetSnapshotsSearched() != 4 {
		t.Errorf("find = %v, %v", f, err)
	}
	// An empty repository.
	p, _ = newScanProber(t, map[string]scanReply{"snapshots": {stdout: []string{"[]"}}, "find": {stdout: []string{"[]"}}})
	f, err = p.Find(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticFind{Repository: remoteRepo(), Patterns: []string{"x"}})
	if err != nil || !f.GetComplete() || f.GetSnapshotsSearched() != 0 {
		t.Errorf("find in an empty repository = %v, %v", f, err)
	}
}

// TestProberFindStopsAtTheBound: past max_matches, the search ends with
// the snapshot in which it passed them.
func TestProberFindStopsAtTheBound(t *testing.T) {
	t.Parallel()
	p, sup := newScanProber(t, map[string]scanReply{
		"snapshots": {stdout: fixture(t, "snapshots_scan.out")},
		"find":      {stdout: chunks(t, "find.out", 20)},
	})
	f, err := p.Find(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticFind{Repository: remoteRepo(), Patterns: []string{"a.txt", "star[*].txt"}, MaxMatches: 1})
	if err != nil {
		t.Fatal(err)
	}
	// The newest snapshot with matches (db00132f) is the third newest.
	if got := matchLines(f); !slices.Equal(got, []string{"db00132f FILE /srv/data/a.txt 13"}) || !f.GetTruncated() || f.GetComplete() || f.GetSnapshotsSearched() != 3 {
		t.Errorf("matches %q, truncated %v, complete %v, searched %d", got, f.GetTruncated(), f.GetComplete(), f.GetSnapshotsSearched())
	}
	if all := len(chunks(t, "find.out", 20)); sup.fed >= all {
		t.Errorf("restic was not stopped: fed all %d chunks", all)
	}

	// Exactly the bound: complete, not truncated.
	p, _ = newScanProber(t, map[string]scanReply{"snapshots": {stdout: fixture(t, "snapshots_scan.out")}, "find": {stdout: chunks(t, "find.out", 20)}})
	f, err = p.Find(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticFind{Repository: remoteRepo(), Patterns: []string{"x"}, MaxMatches: 4})
	if err != nil || len(f.GetMatches()) != 4 || f.GetTruncated() || !f.GetComplete() {
		t.Errorf("exact bound: %v, %v", f, err)
	}
	// restic finishing on its own right after it was asked to stop: what
	// was read, and every snapshot searched.
	p, _ = newScanProber(t, map[string]scanReply{"snapshots": {stdout: fixture(t, "snapshots_scan.out")}, "find": {stdout: chunks(t, "find.out", 1<<20)}})
	f, err = p.Find(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticFind{Repository: remoteRepo(), Patterns: []string{"x"}, MaxMatches: 1})
	if err != nil || len(f.GetMatches()) != 1 || !f.GetTruncated() || !f.GetComplete() || f.GetSnapshotsSearched() != 4 {
		t.Errorf("finished after the stop: %v, %v", f, err)
	}
}

// TestProberFindSoftDeadline: what restic reported before the deadline.
// The searches run in a synctest bubble with a restic that prints one
// piece a second, because Find stops restic through a context of its own,
// derived from the probe's.
func TestProberFindSoftDeadline(t *testing.T) {
	t.Parallel()
	all := chunks(t, "find.out", 10)
	firstEnd := 0 // the piece that ends the first group (db00132f)
	for i := range all {
		if strings.Contains(strings.Join(all[:i+1], ""), `"snapshot":"`+snapL2+`"}`) {
			firstEnd = i + 1
			break
		}
	}
	// find runs after the snapshot listing, one piece printed at 1s.
	run := func(t *testing.T, replies map[string]scanReply, pieces int, req *syncrv1.ProbeResticFind) (*syncrv1.ResticFindOutput, error) {
		t.Helper()
		p, sup := newScanProber(t, replies)
		sup.pause = time.Second
		ctx, cancel := context.WithTimeout(t.Context(), time.Duration(pieces+1)*time.Second+time.Second/2)
		defer cancel()
		return p.Find(ctx, testProbeID, remotesBk(), req)
	}
	req := &syncrv1.ProbeResticFind{Repository: remoteRepo(), Patterns: []string{"x"}}
	for _, tc := range []struct {
		name   string
		pieces int // of find's output read
		want   []string
		found  int32
	}{
		// Four snapshots left: whose matches restic was printing is not
		// known, so they are dropped.
		{"in the first group", firstEnd - 3, nil, 0},
		// Only the oldest snapshot is left after db00132f: the matches
		// restic was printing are its.
		{"in the second group", firstEnd + 30, []string{
			"db00132f FILE /srv/data/a.txt 13", "db00132f FILE /srv/data/star*.txt 5", "aaccfc37 FILE /srv/data/a.txt 4",
		}, 3},
	} {
		synctest.Test(t, func(t *testing.T) {
			f, err := run(t, map[string]scanReply{"snapshots": {stdout: fixture(t, "snapshots_scan.out")}, "find": {stdout: all}}, tc.pieces, req)
			if err != nil || !slices.Equal(matchLines(f), tc.want) || f.GetComplete() || f.GetTruncated() || f.GetSnapshotsSearched() != tc.found {
				t.Errorf("%s: %q, complete %v, searched %d, %v", tc.name, matchLines(f), f.GetComplete(), f.GetSnapshotsSearched(), err)
			}
		})
	}

	// With one snapshot left, the matches restic was printing are its.
	one := chunks(t, "find_windows.out", 10)
	synctest.Test(t, func(t *testing.T) {
		req := &syncrv1.ProbeResticFind{Repository: remoteRepo(), Patterns: []string{"x"}, SnapshotIds: []string{"865ccb3d"}}
		f, err := run(t, map[string]scanReply{"snapshots": {stdout: snapshotsOf(t, "865ccb3d")}, "find": {stdout: one}}, len(one)/2-3, req)
		if want := []string{"865ccb3d FILE /C/Users/ada/Documents/report.docx 18"}; err != nil || !slices.Equal(matchLines(f), want) || f.GetComplete() || f.GetSnapshotsSearched() != 0 {
			t.Errorf("one snapshot: %q, complete %v, searched %d, %v", matchLines(f), f.GetComplete(), f.GetSnapshotsSearched(), err)
		}
	})

	// The deadline during the snapshot listing: nothing, not complete.
	synctest.Test(t, func(t *testing.T) {
		f, err := run(t, map[string]scanReply{"snapshots": {stdout: fixture(t, "snapshots_scan.out")}}, -1, req)
		if err != nil || f.GetComplete() || len(f.GetMatches()) != 0 {
			t.Errorf("deadline while listing: %v, %v", f, err)
		}
	})
	// A cancellation fails.
	synctest.Test(t, func(t *testing.T) {
		p, sup := newScanProber(t, map[string]scanReply{"snapshots": {stdout: fixture(t, "snapshots_scan.out")}, "find": {stdout: all}})
		sup.pause = time.Second
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(10*time.Second, cancel)
		if _, err := p.Find(ctx, testProbeID, remotesBk(), req); !runner.Canceled(err) {
			t.Errorf("err = %v, want a cancellation", err)
		}
	})
}

func TestProberFindFailures(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		replies map[string]scanReply
		want    syncrv1.JobErrorCode
	}{
		"wrong password": {map[string]scanReply{"snapshots": {exit: 12, stderr: []string{`{"message_type":"exit_error","code":12,"message":"Fatal: wrong password or no key found"}`}}},
			syncrv1.JobErrorCode_JOB_ERROR_CODE_WRONG_PASSWORD},
		"garbage": {map[string]scanReply{"snapshots": {stdout: []string{"[]"}}, "find": {stdout: []string{"not json"}}},
			syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL},
		"cut short": {map[string]scanReply{"snapshots": {stdout: []string{"[]"}}, "find": {stdout: chunks(t, "find.out", 1<<20)[:1], exit: 0}},
			syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSPECIFIED},
		"find failed": {map[string]scanReply{"snapshots": {stdout: []string{"[]"}}, "find": {exit: 1, stderr: []string{`{"message_type":"exit_error","code":1,"message":"Fatal: ` + testPassword + ` boom"}`}}},
			syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED},
	} {
		p, _ := newScanProber(t, tc.replies)
		_, err := p.Find(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticFind{Repository: remoteRepo(), Patterns: []string{"x"}})
		if tc.want == syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSPECIFIED {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		mustFail(t, err, tc.want)
		mustNotLeak(t, err.Error())
	}
}

func TestFindStream(t *testing.T) {
	t.Parallel()
	// Brackets and escaped quotes inside strings, nested objects in a
	// match and in a group, and a match too long to keep.
	odd := `/a/b[x]{y}"q\".txt`
	quoted, _ := json.Marshal(odd)
	stream := `[{"matches":[{"path":` + string(quoted) + `,"type":"file","size":3,"extended_attributes":[{"name":"user.x","value":"e30="}]},` +
		`{"path":"/big","type":"file","linktarget":"` + strings.Repeat("x", maxFindObjectBytes) + `"},` +
		`{"path":"/d","type":"dir"}],"hits":3,"future":{"a":[1,{"b":2}]},"snapshot":"` + snapL1 + `"},` +
		`{"matches":[{"path":"/s","type":"symlink"},{"path":"/o","type":"fifo"}],"hits":2,"snapshot":"not-an-id"},` +
		`{"matches":[{"path":"/e","type":"file","size":1}],"hits":1,"snapshot":"` + snapL2 + `"}]`
	for _, step := range []int{1, 3, len(stream)} {
		f := &findStream{max: 10, enough: func() { t.Error("stopped") }}
		for i := 0; i < len(stream); i += step {
			f.write([]byte(stream[i:min(i+step, len(stream))]))
		}
		want := []string{"aaccfc37 FILE " + odd + " 3", "aaccfc37 DIR /d 0", "db00132f FILE /e 1"}
		if got := matchLines(&syncrv1.ResticFindOutput{Matches: f.matches}); !slices.Equal(got, want) || !f.ended || f.extra {
			t.Errorf("step %d: matches %q, ended %v, extra %v", step, got, f.ended, f.extra)
		}
		if !slices.Equal(f.groups, []string{snapL1, snapL2}) {
			t.Errorf("step %d: groups %q", step, f.groups)
		}
	}
}

func changeLines(d *syncrv1.ResticDiffOutput) []string {
	var out []string
	for _, c := range d.GetChanges() {
		out = append(out, c.GetModifier().String()[len("RESTIC_DIFF_MODIFIER_"):]+" "+c.GetPath())
	}
	return out
}

func TestProberDiff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		fixture, path, from, to string
		changes                 []string
		stats                   *syncrv1.ResticDiffStats
	}{
		// The link that became a file is a type change ("T"); img1.jpg,
		// whose time alone changed, is not reported (no --metadata).
		{"diff.out", "", snapL1, snapL2, []string{
			"CHANGED /srv/data/a.txt", "ADDED /srv/data/docs/new.txt", "REMOVED /srv/data/docs/readme.md", "TYPE /srv/data/link",
			"ADDED /srv/data/music/", "ADDED /srv/data/music/song.mp3", "ADDED /srv/data/notes/", "ADDED /srv/data/notes/n.txt",
			"REMOVED /srv/data/notes.txt",
		}, &syncrv1.ResticDiffStats{ChangedFiles: 1, AddedFiles: 3, AddedDirs: 2, AddedBytes: 6148, RemovedFiles: 2, RemovedBytes: 4983}},
		// A folder: restic prints paths below it, Diff makes them tree
		// paths again.
		{"diff_folder.out", "/srv/data/docs/", snapL1 + ":/srv/data/docs", snapL2 + ":/srv/data/docs", []string{
			"ADDED /srv/data/docs/new.txt", "REMOVED /srv/data/docs/readme.md",
		}, &syncrv1.ResticDiffStats{AddedFiles: 1, AddedBytes: 693, RemovedFiles: 1, RemovedBytes: 675}},
		{"diff_windows.out", "/", snapW1, snapW2, []string{
			"ADDED /C/Users/ada/Documents/new folder/", "ADDED /C/Users/ada/Documents/new folder/plan.txt",
			"CHANGED /C/Users/ada/Documents/report.docx", "REMOVED /C/Users/ada/Pictures/cat.png",
		}, &syncrv1.ResticDiffStats{ChangedFiles: 1, AddedFiles: 1, AddedDirs: 1, AddedBytes: 2825, RemovedFiles: 1, RemovedBytes: 4461}},
		{"diff_windows_folder.out", "/C/Users/ada/Documents", snapW1 + ":/C/Users/ada/Documents", snapW2 + ":/C/Users/ada/Documents", []string{
			"ADDED /C/Users/ada/Documents/new folder/", "ADDED /C/Users/ada/Documents/new folder/plan.txt", "CHANGED /C/Users/ada/Documents/report.docx",
		}, &syncrv1.ResticDiffStats{ChangedFiles: 1, AddedFiles: 1, AddedDirs: 1, AddedBytes: 1076, RemovedBytes: 367}},
	}
	for _, tc := range tests {
		p, sup := newScanProber(t, map[string]scanReply{"diff": {stdout: fixture(t, tc.fixture)}})
		from, to := tc.from[:64], tc.to[:64]
		d, err := p.Diff(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticDiff{Repository: remoteRepo(), FromSnapshotId: from, ToSnapshotId: to, Path: tc.path})
		if err != nil {
			t.Fatalf("%s: %v", tc.fixture, err)
		}
		if got := changeLines(d); !slices.Equal(got, tc.changes) {
			t.Errorf("%s: changes =\n  %q\nwant\n  %q", tc.fixture, got, tc.changes)
		}
		if !d.GetComplete() || d.GetTruncated() || d.GetStats().String() != tc.stats.String() {
			t.Errorf("%s: complete %v, truncated %v, stats %v; want %v", tc.fixture, d.GetComplete(), d.GetTruncated(), d.GetStats(), tc.stats)
		}
		want := append(remoteGlobals(true), "--no-lock", "diff", "--", tc.from, tc.to)
		if c := sup.calls[0]; !slices.Equal(c.args, want) || c.grace != scanGrace || c.chunks {
			t.Errorf("%s: args = %q\nwant %q", tc.fixture, c.args, want)
		}
	}
}

func TestProberDiffBoundsAndDeadline(t *testing.T) {
	t.Parallel()
	op := &syncrv1.ProbeResticDiff{Repository: remoteRepo(), FromSnapshotId: snapL1, ToSnapshotId: snapL2, MaxChanges: 3}
	// Past max_changes the changes are cut, but restic runs on for its
	// statistics.
	p, sup := newScanProber(t, map[string]scanReply{"diff": {stdout: fixture(t, "diff.out")}})
	d, err := p.Diff(t.Context(), testProbeID, remotesBk(), op)
	if err != nil || len(d.GetChanges()) != 3 || !d.GetTruncated() || !d.GetComplete() || d.GetStats().GetAddedFiles() != 3 || sup.fed != 10 {
		t.Errorf("bounded diff = %v, %v (fed %d)", d, err, sup.fed)
	}
	// The soft deadline: the changes so far, without statistics.
	op.MaxChanges = 0
	p, _ = newScanProber(t, map[string]scanReply{"diff": {stdout: fixture(t, "diff.out")}})
	d, err = p.Diff(&endAfter{Context: t.Context(), err: context.DeadlineExceeded, n: 6}, testProbeID, remotesBk(), op)
	if err != nil || len(d.GetChanges()) != 5 || d.GetComplete() || d.GetStats() != nil || d.GetTruncated() {
		t.Errorf("diff at the deadline = %v, %v", d, err)
	}
	// Restic stopped right after its statistics: the statistics count
	// only if it finished.
	p, _ = newScanProber(t, map[string]scanReply{"diff": {stdout: append(fixture(t, "diff.out"), "")}})
	d, err = p.Diff(&endAfter{Context: t.Context(), err: context.DeadlineExceeded, n: 11}, testProbeID, remotesBk(), op)
	if err != nil || d.GetComplete() || d.GetStats() != nil || len(d.GetChanges()) != 9 {
		t.Errorf("diff stopped after its statistics = %v, %v", d, err)
	}
	_, err = p.Diff(&endAfter{Context: t.Context(), err: context.Canceled, n: 2}, testProbeID, remotesBk(), op)
	if !runner.Canceled(err) {
		t.Errorf("err = %v, want a cancellation", err)
	}
}

func TestProberDiffFailures(t *testing.T) {
	t.Parallel()
	for name, stderr := range map[string]string{
		"missing folder": fixture(t, "diff_missing_folder.err")[0],
		"a file":         `{"message_type":"exit_error","code":1,"message":"path srv/data/a.txt: not a directory"}`,
		"no snapshot":    `{"message_type":"exit_error","code":1,"message":"Fatal: no matching ID found for prefix \"deadbeef\""}`,
	} {
		p, _ := newScanProber(t, map[string]scanReply{"diff": {exit: 1, stderr: []string{stderr}}})
		_, err := p.Diff(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticDiff{Repository: remoteRepo(), FromSnapshotId: snapL1, ToSnapshotId: "deadbeef", Path: "/srv/data/nope"})
		if runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND {
			t.Errorf("%s: err = %v (%v), want NOT_FOUND", name, err, runner.CodeOf(err))
		}
	}
	p, _ := newScanProber(t, map[string]scanReply{"diff": {exit: 1, stderr: []string{`{"message_type":"exit_error","code":1,"message":"Fatal: load index: ` + testSecret + `"}`}}})
	_, err := p.Diff(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticDiff{Repository: remoteRepo(), FromSnapshotId: snapL1, ToSnapshotId: snapL2})
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED)
	mustNotLeak(t, err.Error())
}

func TestDiffModifier(t *testing.T) {
	t.Parallel()
	// Recorded with restic 0.19.1 as Diff runs it (no --metadata): "+",
	// "-", "M", "T". With --metadata, which Diff does not pass, restic
	// 0.19.1 also printed "U" and "TU"; they stay mapped in case a later
	// restic reports them anyway. "?" (bitrot) needs a file whose content
	// changed with every piece of metadata, ctime included, kept: not
	// reproducible here. The others are synthetic.
	for m, want := range map[string]syncrv1.ResticDiffModifier{
		"+":  syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_ADDED,
		"-":  syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_REMOVED,
		"M":  syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_CHANGED,
		"U":  syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_METADATA,
		"T":  syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_TYPE,
		"TU": syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_TYPE,
		"MU": syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_CHANGED,
		"?":  syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_OTHER,
		"":   syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_OTHER,
		"X":  syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_OTHER,
	} {
		if got := diffModifier(m); got != want {
			t.Errorf("diffModifier(%q) = %v, want %v", m, got, want)
		}
	}
	// Lines as restic 0.19.1 printed them with --metadata, and a bitrot
	// line.
	d := &diffReader{dir: "/", max: 10, out: &syncrv1.ResticDiffOutput{}}
	for _, l := range []string{
		`{"message_type":"change","path":"/srv/data/","modifier":"U"}`,
		`{"message_type":"change","path":"/srv/data/link","modifier":"TU"}`,
		`{"message_type":"change","path":"/srv/data/a.txt","modifier":"?"}`,
	} {
		d.line(proc.Line{Data: []byte(l)})
	}
	if got, want := changeLines(d.out), []string{"METADATA /srv/data/", "TYPE /srv/data/link", "OTHER /srv/data/a.txt"}; !slices.Equal(got, want) {
		t.Errorf("changes = %q, want %q", got, want)
	}
}

func TestTreePathIn(t *testing.T) {
	t.Parallel()
	for _, tc := range [][3]string{
		{"/", "/a/b", "/a/b"},
		{"/srv/data", "/new.txt", "/srv/data/new.txt"},
		{"/srv/data", "/sub/", "/srv/data/sub/"},
		{"/srv/data", "/", "/srv/data/"},
		{"/srv/data", "odd", "/srv/data/odd"},
	} {
		if got := treePathIn(tc[0], tc[1]); got != tc[2] {
			t.Errorf("treePathIn(%q, %q) = %q, want %q", tc[0], tc[1], got, tc[2])
		}
	}
}
