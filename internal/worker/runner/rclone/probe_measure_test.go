package rclone

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// The testdata/lsjson_recursive*.out and .err files are the standard
// output and error of rclone v1.75.2 running
//
//	rclone --config= lsjson -R --no-mimetype --no-modtime --use-json-log -- <target>
//
// over the same small tree: a.txt (4 bytes), docs/readme.md (5),
// docs/deep/e.txt (5), photos/2025/{IMG2.JPG (5000), img1.jpg (3000)},
// locked/inner/l.txt (2), an empty folder "empty" and a symbolic link
// "link". The targets: a union remote of a local folder (lsjson_recursive,
// and _unreadable as an unprivileged user that cannot read "locked", exit
// code 6; _missing for a folder that does not exist, exit code 3), an sftp
// server where "locked" and "private" belong to another user
// (_sftp_unreadable, exit code 1, no "empty"), and a MinIO bucket (_s3: no
// empty folder, folders synthesized and printed last). Local paths in
// messages were replaced by /data.

func readFixture(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.TrimRight(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func measureOp(path string) *syncrv1.ProbeMeasureRemote {
	return &syncrv1.ProbeMeasureRemote{Location: &syncrv1.Location{Remote: "box", Path: path}}
}

func childTotals(m *syncrv1.ProbeMeasurement) []string {
	var out []string
	for _, c := range m.GetChildren() {
		out = append(out, fmt.Sprintf("%s %d %d %d", c.GetName(), c.GetBytes(), c.GetFiles(), c.GetDirs()))
	}
	return out
}

func TestProberMeasure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		fixture  string
		exit     int
		bytes    int64
		files    int64
		dirs     int64
		children []string
		unread   int64
	}{
		{"local", "lsjson_recursive", 0, 8016, 6, 7, []string{"photos 8000 2 1", "docs 10 2 1", "locked 2 1 1", "empty 0 0 0"}, 0},
		{"bucket", "lsjson_recursive_s3", 0, 8016, 6, 6, []string{"photos 8000 2 1", "docs 10 2 1", "locked 2 1 1"}, 0},
		// rclone lists the rest, then fails: the folders it could not read
		// are counted as unreadable.
		{"unreadable", "lsjson_recursive_unreadable", 6, 8014, 5, 6, []string{"photos 8000 2 1", "docs 10 2 1", "empty 0 0 0", "locked 0 0 0"}, 1},
		{"sftp unreadable", "lsjson_recursive_sftp_unreadable", 1, 14, 3, 4, []string{"docs 10 2 1", "locked 0 0 0", "private 0 0 0"}, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sup := &probeSupervisor{stdout: readFixture(t, tc.fixture+".out"), exit: tc.exit}
			if _, err := os.Stat(filepath.Join("testdata", tc.fixture+".err")); err == nil {
				sup.stderr = readFixture(t, tc.fixture+".err")
			}
			p, scratch := newTestProber(t, sup)
			m, err := p.Measure(t.Context(), probeID, []*syncrv1.RemoteDefinition{sftpRemote("box", "hunter2-secret")}, measureOp("data/tree"), 10)
			if err != nil {
				t.Fatal(err)
			}
			if m.GetPath() != "data/tree" || m.GetBytes() != tc.bytes || m.GetFiles() != tc.files || m.GetDirs() != tc.dirs ||
				!m.GetComplete() || m.GetUnreadable() != tc.unread || m.GetChildrenTruncated() {
				t.Errorf("measurement = %v", m)
			}
			if got := childTotals(m); !slices.Equal(got, tc.children) {
				t.Errorf("children = %q, want %q", got, tc.children)
			}
			want := []string{"lsjson", "-R", "--no-mimetype", "--no-modtime", "--use-json-log", "--", "box:data/tree"}
			if !slices.Equal(sup.spec.Args, want) || sup.spec.GracePeriod != scanGrace {
				t.Errorf("args = %q, grace %v; want %q, %v", sup.spec.Args, sup.spec.GracePeriod, want, scanGrace)
			}
			if sup.spec.Env["RCLONE_CONFIG_BOX_TYPE"] != "sftp" || sup.spec.Env[rcloneenv.EnvConfig] == "" {
				t.Errorf("env = %v", sup.spec.Env)
			}
			if left, _ := os.ReadDir(scratch); len(left) != 0 {
				t.Errorf("scratch entries left: %v", left)
			}
		})
	}
}

func TestProberMeasureChildrenBound(t *testing.T) {
	t.Parallel()
	sup := &probeSupervisor{stdout: readFixture(t, "lsjson_recursive.out")}
	p, _ := newTestProber(t, sup)
	m, err := p.Measure(t.Context(), probeID, []*syncrv1.RemoteDefinition{sftpRemote("box", "x")}, measureOp(""), 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := childTotals(m); !slices.Equal(got, []string{"photos 8000 2 1", "docs 10 2 1"}) || !m.GetChildrenTruncated() || m.GetBytes() != 8016 {
		t.Errorf("children = %q, truncated %v", got, m.GetChildrenTruncated())
	}
}

func TestProberMeasureFailures(t *testing.T) {
	t.Parallel()
	remotes := []*syncrv1.RemoteDefinition{sftpRemote("box", "hunter2-secret")}
	errLine := func(object, msg string) string {
		return `{"level":"error","msg":"` + msg + `","object":"` + object + `","objectType":"string","source":"walk/walk.go:175"}`
	}
	notice := func(msg string) string { return `{"level":"notice","msg":"` + msg + `","source":"cmd/cmd.go:336"}` }
	tests := []struct {
		name   string
		sup    *probeSupervisor
		want   syncrv1.JobErrorCode
		inText string
	}{
		{
			"missing folder",
			&probeSupervisor{exit: 3, stdout: readFixture(t, "lsjson_recursive_missing.out"), stderr: readFixture(t, "lsjson_recursive_missing.err")},
			syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "directory not found",
		},
		{
			// An error that names no entry fails the measurement, even
			// beside entry errors.
			"error without an entry",
			&probeSupervisor{exit: 1, stdout: readFixture(t, "lsjson_recursive.out"), stderr: []string{
				errLine("locked", "error listing: permission denied"), errLine("", "error listing: connection reset"),
				notice("Failed to lsjson with 2 errors: last error was: connection reset"),
			}},
			syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "connection reset",
		},
		{
			// rclone counted more errors than it logged entries: one of
			// them was something else.
			"more errors than entries",
			&probeSupervisor{exit: 1, stdout: readFixture(t, "lsjson_recursive.out"), stderr: []string{
				errLine("locked", "error listing: permission denied"),
				notice("Failed to lsjson with 4 errors: last error was: quota exceeded"),
			}},
			syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "permission denied",
		},
		{
			"no final notice",
			&probeSupervisor{exit: 1, stderr: []string{errLine("locked", "error listing: permission denied")}},
			syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "",
		},
		{
			"bad credentials, scrubbed",
			&probeSupervisor{exit: 1, stderr: []string{`{"level":"critical","msg":"Failed to create file system: ssh: bad password hunter2-secret"}`}},
			syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "bad password",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, _ := newTestProber(t, tc.sup)
			_, err := p.Measure(t.Context(), probeID, remotes, measureOp("data"), 10)
			mustFailCode(t, err, tc.want)
			if !strings.Contains(err.Error(), tc.inText) || strings.Contains(err.Error(), "hunter2") {
				t.Errorf("message = %q", err)
			}
		})
	}

	// Bad specs never start rclone.
	sup := &probeSupervisor{}
	p, _ := newTestProber(t, sup)
	for _, op := range []*syncrv1.ProbeMeasureRemote{{}, {Location: &syncrv1.Location{Path: "/etc"}}, {Location: &syncrv1.Location{Remote: "other"}}} {
		if _, err := p.Measure(t.Context(), probeID, remotes, op, 10); runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC {
			t.Errorf("measure %v = %v, want INVALID_SPEC", op, err)
		}
	}
	if sup.calls != 0 {
		t.Errorf("rclone started %d times for bad specs", sup.calls)
	}
}

// endAfter is a context whose Err reports err from its n-th call on; the
// fake supervisor checks it before each line, as a stopped rclone would
// stop printing.
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

func TestProberMeasureStopsWithItsContext(t *testing.T) {
	t.Parallel()
	remotes := []*syncrv1.RemoteDefinition{sftpRemote("box", "x")}
	// The soft deadline: what was counted, with complete=false.
	sup := &probeSupervisor{stdout: readFixture(t, "lsjson_recursive.out")}
	p, _ := newTestProber(t, sup)
	// Stopped before the 11th line: "[", then a.txt, readme.md and seven
	// folders.
	ctx := &endAfter{Context: t.Context(), err: context.DeadlineExceeded, n: 11}
	m, err := p.Measure(ctx, probeID, remotes, measureOp("data"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if m.GetComplete() || !sup.stopped || m.GetFiles() != 2 || m.GetDirs() != 7 || m.GetBytes() != 9 {
		t.Errorf("partial measurement = %v (stopped %v)", m, sup.stopped)
	}
	// A cancellation fails.
	sup = &probeSupervisor{stdout: readFixture(t, "lsjson_recursive.out")}
	p, _ = newTestProber(t, sup)
	_, err = p.Measure(&endAfter{Context: t.Context(), err: context.Canceled, n: 3}, probeID, remotes, measureOp("data"), 10)
	if !runner.Canceled(err) {
		t.Errorf("err = %v, want a cancellation", err)
	}
}

func TestParseMeasureLine(t *testing.T) {
	t.Parallel()
	for _, line := range []string{"", "[", "]", ",", "not json", `{"Path":""}`, `{"Path":`, `["x"]`} {
		if it, ok := parseMeasureLine([]byte(line)); ok {
			t.Errorf("parseMeasureLine(%q) = %v", line, it)
		}
	}
	it, ok := parseMeasureLine([]byte(` {"Path":"a/b c","Name":"b c","Size":-1,"ModTime":"","IsDir":true}, `))
	if !ok || it.Path != "a/b c" || !it.IsDir || it.Size != -1 {
		t.Errorf("item = %+v, %v", it, ok)
	}
}
