package rclone

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

const probeID = "0195f0a4-3b7c-7d1e-9f00-0000000000cc"

// probeSupervisor replays stdout and stderr lines and records the spec. It
// stops feeding stdout once the context is cancelled, like a killed rclone.
type probeSupervisor struct {
	stdout, stderr []string
	exit           int

	spec    proc.Spec
	calls   int
	fed     int // stdout lines delivered
	stopped bool
}

func (s *probeSupervisor) Run(ctx context.Context, spec proc.Spec) (proc.Result, error) {
	s.spec = spec
	s.calls++
	for _, l := range s.stderr {
		spec.Stderr(proc.Line{Data: []byte(l)})
	}
	for _, l := range s.stdout {
		if ctx.Err() != nil {
			s.stopped = true
			return proc.Result{ExitCode: -1}, &proc.CanceledError{Path: spec.Path, ExitCode: -1, Cause: context.Cause(ctx)}
		}
		spec.Stdout(proc.Line{Data: []byte(l)})
		s.fed++
	}
	if s.exit != 0 {
		return proc.Result{ExitCode: s.exit}, &proc.ExitError{Path: spec.Path, ExitCode: s.exit}
	}
	return proc.Result{}, nil
}

func newTestProber(t *testing.T, sup *probeSupervisor) (*Prober, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, scratch := filepath.Join(base, "data"), filepath.Join(base, "scratch")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	policy, err := localfs.NewPolicy([]string{data})
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProber(Config{Binary: "/opt/syncr/rclone", Supervisor: sup, Env: rcloneenv.NewBuilder(policy), Paths: policy, ScratchDir: scratch})
	if err != nil {
		t.Fatal(err)
	}
	return p, scratch
}

func lsjson(items ...string) []string {
	out := []string{"["}
	for i, it := range items {
		if i < len(items)-1 {
			it += ","
		}
		out = append(out, it)
	}
	return append(out, "]")
}

func item(name string, dir bool, size int64) string {
	return fmt.Sprintf(`{"Path":%q,"Name":%q,"Size":%d,"ModTime":"2026-01-02T03:04:05.123456789Z","IsDir":%v}`, name, name, size, dir)
}

func entryNames(l *syncrv1.ProbeListing) []string {
	out := make([]string, len(l.GetEntries()))
	for i, e := range l.GetEntries() {
		out[i] = e.GetName()
	}
	return out
}

func mustFailCode(t *testing.T, err error, code syncrv1.JobErrorCode) {
	t.Helper()
	if err == nil || runner.CodeOf(err) != code {
		t.Fatalf("err = %v (code %v), want code %v", err, runner.CodeOf(err), code)
	}
}

func TestProberList(t *testing.T) {
	t.Parallel()
	sup := &probeSupervisor{stdout: lsjson(item("b.txt", false, 7), item("zdir", true, -1), item("a b.txt", false, 0), item("adir", true, 0))}
	p, scratch := newTestProber(t, sup)
	op := &syncrv1.ProbeListRemote{Location: &syncrv1.Location{Remote: "box", Path: "bucket/sub"}}
	l, err := p.List(t.Context(), probeID, []*syncrv1.RemoteDefinition{sftpRemote("box", "hunter2-secret")}, op)
	if err != nil {
		t.Fatal(err)
	}
	// Directories first, then by name.
	if want := []string{"adir", "zdir", "a b.txt", "b.txt"}; !slices.Equal(entryNames(l), want) || l.GetPath() != "bucket/sub" || l.GetTruncated() {
		t.Errorf("listing %q = %v, want %v", l.GetPath(), entryNames(l), want)
	}
	file, dir := l.GetEntries()[3], l.GetEntries()[1]
	if file.GetPath() != "bucket/sub/b.txt" || file.GetSize() != 7 || file.GetType() != syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_FILE ||
		file.GetModTime().AsTime().Year() != 2026 {
		t.Errorf("file = %v", file)
	}
	if dir.GetType() != syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR || dir.GetSize() != 0 {
		t.Errorf("dir = %v (a directory has no size)", dir)
	}
	if want := []string{"lsjson", "--max-depth=1", "--no-mimetype", "--", "box:bucket/sub"}; !slices.Equal(sup.spec.Args, want) {
		t.Errorf("args = %q, want %q", sup.spec.Args, want)
	}
	// The remote reaches rclone through the environment only, isolated
	// from the host's configuration, and the scratch directory is removed.
	if sup.spec.Env["RCLONE_CONFIG_BOX_TYPE"] != "sftp" || sup.spec.Env[rcloneenv.EnvConfig] == "" {
		t.Errorf("env = %v", sup.spec.Env)
	}
	if !strings.HasPrefix(filepath.Base(sup.spec.Dir), "rclone-probe-") {
		t.Errorf("scratch dir = %q", sup.spec.Dir)
	}
	if left, _ := os.ReadDir(scratch); len(left) != 0 {
		t.Errorf("scratch entries left: %v", left)
	}

	// dirs_only is passed to rclone.
	op.DirsOnly = true
	if _, err := p.List(t.Context(), probeID, []*syncrv1.RemoteDefinition{sftpRemote("box", "x")}, op); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(sup.spec.Args, "--dirs-only") {
		t.Errorf("args = %q, want --dirs-only", sup.spec.Args)
	}
}

func TestProberListStopsAtTheBound(t *testing.T) {
	t.Parallel()
	var items []string
	for i := range 50 {
		items = append(items, item(fmt.Sprintf("f%02d", i), false, 1))
	}
	sup := &probeSupervisor{stdout: lsjson(items...)}
	p, _ := newTestProber(t, sup)
	l, err := p.List(t.Context(), probeID, []*syncrv1.RemoteDefinition{sftpRemote("box", "x")},
		&syncrv1.ProbeListRemote{Location: &syncrv1.Location{Remote: "box"}, MaxEntries: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.GetEntries()) != 5 || !l.GetTruncated() {
		t.Errorf("%d entries, truncated=%v", len(l.GetEntries()), l.GetTruncated())
	}
	// rclone was stopped instead of listing everything.
	if !sup.stopped || sup.fed > 10 {
		t.Errorf("rclone was fed %d lines (stopped=%v), want it stopped right after the bound", sup.fed, sup.stopped)
	}

	// Exactly the bound is not truncated.
	sup = &probeSupervisor{stdout: lsjson(items[:5]...)}
	p, _ = newTestProber(t, sup)
	l, err = p.List(t.Context(), probeID, []*syncrv1.RemoteDefinition{sftpRemote("box", "x")},
		&syncrv1.ProbeListRemote{Location: &syncrv1.Location{Remote: "box"}, MaxEntries: 5})
	if err != nil || len(l.GetEntries()) != 5 || l.GetTruncated() {
		t.Errorf("exact bound: %d entries, truncated=%v, %v", len(l.GetEntries()), l.GetTruncated(), err)
	}
}

func TestProberListFailures(t *testing.T) {
	t.Parallel()
	remotes := []*syncrv1.RemoteDefinition{sftpRemote("box", "hunter2-secret")}
	loc := &syncrv1.Location{Remote: "box", Path: "x"}

	// A missing directory.
	sup := &probeSupervisor{exit: 3, stdout: []string{"["}, stderr: []string{
		"2026/01/02 03:04:05 ERROR : error listing: directory not found",
		"2026/01/02 03:04:05 NOTICE: Failed to lsjson with 2 errors: last error was: error in ListJSON: directory not found",
	}}
	p, _ := newTestProber(t, sup)
	_, err := p.List(t.Context(), probeID, remotes, &syncrv1.ProbeListRemote{Location: loc})
	mustFailCode(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND)

	// A failure message is scrubbed of the remote's secret.
	sup = &probeSupervisor{exit: 1, stderr: []string{"2026/01/02 03:04:05 CRITICAL: Failed to create file system: ssh: bad password hunter2-secret"}}
	p, _ = newTestProber(t, sup)
	_, err = p.List(t.Context(), probeID, remotes, &syncrv1.ProbeListRemote{Location: loc})
	mustFailCode(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED)
	if strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "bad password") {
		t.Errorf("message = %q", err)
	}

	// Bad specs never start rclone.
	sup = &probeSupervisor{}
	p, scratch := newTestProber(t, sup)
	for name, tc := range map[string]struct {
		remotes []*syncrv1.RemoteDefinition
		loc     *syncrv1.Location
		want    syncrv1.JobErrorCode
	}{
		"no remote":        {remotes, &syncrv1.Location{Path: "/etc"}, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC},
		"no location":      {remotes, nil, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC},
		"undefined remote": {remotes, &syncrv1.Location{Remote: "other"}, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC},
		"invalid remote":   {[]*syncrv1.RemoteDefinition{{Alias: "box", BackendType: "nope"}}, loc, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC},
		"NUL in the path":  {remotes, &syncrv1.Location{Remote: "box", Path: "a\x00b"}, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC},
	} {
		_, err := p.List(t.Context(), probeID, tc.remotes, &syncrv1.ProbeListRemote{Location: tc.loc})
		if err == nil || runner.CodeOf(err) != tc.want {
			t.Errorf("%s: err = %v, want code %v", name, err, tc.want)
		}
		if _, err := p.About(t.Context(), probeID, tc.remotes, &syncrv1.ProbeAbout{Location: tc.loc}); err == nil {
			t.Errorf("%s: about succeeded", name)
		}
	}
	if sup.calls != 0 {
		t.Errorf("rclone started %d times for bad specs", sup.calls)
	}
	if left, _ := os.ReadDir(scratch); len(left) != 0 {
		t.Errorf("scratch entries left after bad specs: %v", left)
	}
}

func TestProberListCancelled(t *testing.T) {
	t.Parallel()
	sup := &probeSupervisor{stdout: lsjson(item("a", false, 1))}
	p, _ := newTestProber(t, sup)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := p.List(ctx, probeID, []*syncrv1.RemoteDefinition{sftpRemote("box", "x")}, &syncrv1.ProbeListRemote{Location: &syncrv1.Location{Remote: "box"}})
	if !runner.Canceled(err) {
		t.Errorf("err = %v, want a cancellation", err)
	}
}

func TestParseLsjsonLine(t *testing.T) {
	t.Parallel()
	for _, line := range []string{"", "[", "]", ",", "not json", `{"Name":""}`, `{"Name":`, `["x"]`} {
		if e, ok := parseLsjsonLine([]byte(line), ""); ok {
			t.Errorf("parseLsjsonLine(%q) = %v", line, e)
		}
	}
	e, ok := parseLsjsonLine([]byte(`  {"Path":"x","Name":"x","Size":-1,"ModTime":"","IsDir":false} , `), "")
	if !ok || e.GetPath() != "x" || e.GetSize() != 0 || e.GetModTime() != nil {
		t.Errorf("entry = %v, %v (an unknown size and time are left unset)", e, ok)
	}
	if e, _ := parseLsjsonLine([]byte(`{"Name":"bad\ud800name","IsDir":true}`), "dir/"); e.GetPath() != "dir/bad�name" {
		t.Errorf("path = %q", e.GetPath())
	}
}

func TestProberAbout(t *testing.T) {
	t.Parallel()
	remotes := []*syncrv1.RemoteDefinition{sftpRemote("box", "hunter2-secret")}
	op := &syncrv1.ProbeAbout{Location: &syncrv1.Location{Remote: "box", Path: "data"}}

	sup := &probeSupervisor{stdout: []string{"{", `	"total": 8330108928,`, `	"used": 1373343744,`, `	"free": 6956765184`, "}"}}
	p, _ := newTestProber(t, sup)
	u, err := p.About(t.Context(), probeID, remotes, op)
	if err != nil {
		t.Fatal(err)
	}
	if u.GetTotal() != 8330108928 || u.GetUsed() != 1373343744 || u.GetFree() != 6956765184 || u.Trashed != nil || u.Objects != nil || u.GetUnsupported() {
		t.Errorf("usage = %v", u)
	}
	if want := []string{"about", "--json", "--", "box:data"}; !slices.Equal(sup.spec.Args, want) {
		t.Errorf("args = %q, want %q", sup.spec.Args, want)
	}

	// A backend without usage information is not an error.
	// rclone names the reason only in its final notice.
	sup = &probeSupervisor{exit: 1, stderr: []string{`2026/01/02 03:04:05 NOTICE: Failed to about: S3 bucket bucket path x doesn't support about`}}
	p, _ = newTestProber(t, sup)
	u, err = p.About(t.Context(), probeID, remotes, op)
	if err != nil || !u.GetUnsupported() || u.Total != nil {
		t.Errorf("unsupported = %v, %v", u, err)
	}

	// Another failure is one, scrubbed.
	sup = &probeSupervisor{exit: 1, stderr: []string{"2026/01/02 03:04:05 CRITICAL: Failed to create file system: bad password hunter2-secret"}}
	p, _ = newTestProber(t, sup)
	_, err = p.About(t.Context(), probeID, remotes, op)
	mustFailCode(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED)
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("message leaks the secret: %q", err)
	}

	// A failure rclone explains only in its final notice keeps the reason.
	sup = &probeSupervisor{exit: 1, stderr: []string{"2026/01/02 03:04:05 NOTICE: Failed to about: connection refused"}}
	p, _ = newTestProber(t, sup)
	if _, err = p.About(t.Context(), probeID, remotes, op); err == nil || !strings.Contains(err.Error(), "failed to about: connection refused") {
		t.Errorf("message = %v", err)
	}

	// Garbage instead of JSON.
	sup = &probeSupervisor{stdout: []string{"not json"}}
	p, _ = newTestProber(t, sup)
	_, err = p.About(t.Context(), probeID, remotes, op)
	mustFailCode(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL)
}
