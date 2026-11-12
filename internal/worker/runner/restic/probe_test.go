package restic

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

const testProbeID = "0195f0a4-3b7c-7d1e-9f00-0000000000aa"

// reply is the scripted outcome of one child process.
type reply struct {
	stdout, stderr []string
	exit           int
	err            error // instead of an exit code
}

// call is one child process a scriptSupervisor ran: the tool ("restic" or
// "rclone"), its verb and its arguments.
type call struct {
	tool, verb string
	args       []string
	env        map[string]string
	dir        string
}

// scriptSupervisor answers every child process with handle and records the
// calls in order.
type scriptSupervisor struct {
	mu     sync.Mutex
	calls  []call
	handle func(c call) reply
}

var resticVerbs = []string{"init", "cat", "snapshots", "ls", "unlock", "list", "backup", "restore", "forget", "prune", "check"}

func (s *scriptSupervisor) Run(_ context.Context, spec proc.Spec) (proc.Result, error) {
	c := call{args: spec.Args, env: spec.Env, dir: spec.Dir}
	switch spec.Path {
	case fakeResticBin:
		c.tool = "restic"
		for _, a := range spec.Args {
			if slices.Contains(resticVerbs, a) {
				c.verb = a
				break
			}
		}
	case fakeRcloneBin:
		c.tool = "rclone"
		for _, a := range spec.Args {
			if !strings.HasPrefix(a, "-") {
				c.verb = a
				break
			}
		}
	}
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
	r := s.handle(c)
	for _, l := range r.stdout {
		spec.Stdout(proc.Line{Data: []byte(l)})
	}
	for _, l := range r.stderr {
		spec.Stderr(proc.Line{Data: []byte(l)})
	}
	if r.err != nil {
		return proc.Result{ExitCode: -1}, r.err
	}
	if r.exit != 0 {
		return proc.Result{ExitCode: r.exit}, &proc.ExitError{Path: spec.Path, ExitCode: r.exit}
	}
	return proc.Result{}, nil
}

// verbs returns "tool verb" of every call, in order.
func (s *scriptSupervisor) verbs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.calls))
	for i, c := range s.calls {
		out[i] = c.tool + " " + c.verb
	}
	return out
}

func exitError(code int, message string) reply {
	return reply{exit: code, stdout: []string{`{"message_type":"exit_error","code":` + strconv.Itoa(code) + `,"message":"` + message + `"}`}}
}

func newTestProber(t *testing.T, handle func(c call) reply) (*Prober, *scriptSupervisor, *testEnv) {
	t.Helper()
	sup := &scriptSupervisor{handle: handle}
	te := newTestEnv(t, func(c *Config) { c.Supervisor = sup })
	p, err := NewProber(te.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p, sup, te
}

func remotesBk() []*syncrv1.RemoteDefinition { return []*syncrv1.RemoteDefinition{s3Remote()} }

func TestProberSnapshots(t *testing.T) {
	t.Parallel()
	p, sup, te := newTestProber(t, func(call) reply { return reply{stdout: fixture(t, "snapshots_summary.out")} })
	out, err := p.Snapshots(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticSnapshots{Repository: remoteRepo()})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.GetSnapshots()) != 2 || out.GetTruncated() {
		t.Fatalf("output = %v", out)
	}
	first, second := out.GetSnapshots()[0], out.GetSnapshots()[1]
	if first.GetUsername() != "ada" || first.GetHost() != "h1" || first.GetProgramVersion() != "0.19.1" ||
		!slices.Equal(first.GetTags(), []string{"e2e"}) || !slices.Equal(first.GetPaths(), []string{"/srv/src"}) {
		t.Errorf("first = %v", first)
	}
	sum := first.GetSummary()
	if sum.GetFilesNew() != 4 || sum.GetTotalFilesProcessed() != 4 || sum.GetDataAddedBytes() == 0 ||
		sum.GetDataAddedPackedBytes() == 0 || sum.GetBackupStart() == nil || sum.GetBackupEnd() == nil {
		t.Errorf("summary = %v", sum)
	}
	if second.GetParent() != first.GetId() || second.GetSummary().GetFilesChanged() != 1 {
		t.Errorf("second = %v", second)
	}
	// The read takes no lock, and the scratch directory is the probe's.
	c := sup.calls[0]
	want := append(remoteGlobals(true), "--no-lock", "snapshots")
	if !slices.Equal(c.args, want) {
		t.Errorf("args = %q\nwant %q", c.args, want)
	}
	if scratch := c.env["TMPDIR"]; filepath.Base(scratch) != "probe-"+testProbeID || filepath.Dir(scratch) != c.dir || c.dir != te.scratch {
		t.Errorf("TMPDIR = %q, working directory %q; want the probe's directory under the scratch root %q", scratch, c.dir, te.scratch)
	}
	if _, err := os.Stat(c.env["TMPDIR"]); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("scratch directory left behind: %v", err)
	}
	if c.env["RESTIC_PASSWORD"] != testPassword || c.env["RESTIC_CACHE_DIR"] != te.cache {
		t.Errorf("env = %v", keys(c.env))
	}
}

func TestProberSnapshotsKeepsTheNewest(t *testing.T) {
	t.Parallel()
	p, _, _ := newTestProber(t, func(call) reply { return reply{stdout: fixture(t, "snapshots_summary.out")} })
	out, err := p.Snapshots(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticSnapshots{Repository: remoteRepo(), MaxSnapshots: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.GetSnapshots()) != 1 || !out.GetTruncated() || out.GetSnapshots()[0].GetParent() == "" {
		t.Errorf("output = %v, want only the newest snapshot (the one with a parent)", out)
	}
}

func TestProberLs(t *testing.T) {
	t.Parallel()
	p, sup, _ := newTestProber(t, func(call) reply { return reply{stdout: fixture(t, "ls.out")} })
	op := &syncrv1.ProbeResticLs{Repository: remoteRepo(), SnapshotId: "2a8b1c3d", Path: "/srv/src/"}
	out, err := p.Ls(t.Context(), testProbeID, remotesBk(), op)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range out.GetEntries() {
		got = append(got, e.GetType().String()[len("PROBE_ENTRY_TYPE_"):]+" "+e.GetPath())
	}
	want := []string{
		"DIR /srv/src/docs", "FILE /srv/src/[x]*?.txt", "FILE /srv/src/a b.txt", "SYMLINK /srv/src/link", "FILE /srv/src/ü.txt",
	}
	if !slices.Equal(got, want) || out.GetPath() != "/srv/src" || out.GetTruncated() {
		t.Errorf("listing %q truncated=%v =\n  %q\nwant\n  %q", out.GetPath(), out.GetTruncated(), got, want)
	}
	file := out.GetEntries()[2]
	if file.GetName() != "a b.txt" || file.GetSize() != 4 || file.GetMode() != "-rw-r--r--" || file.GetModTime() == nil {
		t.Errorf("entry = %v", file)
	}
	if wantArgs := append(remoteGlobals(true), "--no-lock", "ls", "--", "2a8b1c3d", "/srv/src"); !slices.Equal(sup.calls[0].args, wantArgs) {
		t.Errorf("args = %q\nwant %q", sup.calls[0].args, wantArgs)
	}

	// max_entries cuts the listing.
	op.MaxEntries = 2
	out, err = p.Ls(t.Context(), testProbeID, remotesBk(), op)
	if err != nil || len(out.GetEntries()) != 2 || !out.GetTruncated() {
		t.Errorf("limited listing = %v, %v", out, err)
	}
}

func TestProberLsMissingFolder(t *testing.T) {
	t.Parallel()
	// restic lists a missing folder as an empty one: only the snapshot line.
	p, _, _ := newTestProber(t, func(call) reply { return reply{stdout: fixture(t, "ls.out")[:1]} })
	_, err := p.Ls(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticLs{Repository: remoteRepo(), SnapshotId: "latest", Path: "/nope"})
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND)

	// The root is never listed itself; an empty root is not an error.
	out, err := p.Ls(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticLs{Repository: remoteRepo(), SnapshotId: "latest", Path: "/"})
	if err != nil || len(out.GetEntries()) != 0 {
		t.Errorf("root listing = %v, %v", out, err)
	}
}

func TestProberLsRejectsBadInput(t *testing.T) {
	t.Parallel()
	p, sup, _ := newTestProber(t, func(call) reply { return reply{} })
	for _, op := range []*syncrv1.ProbeResticLs{
		{Repository: remoteRepo(), SnapshotId: "--help", Path: "/"},
		{Repository: remoteRepo(), SnapshotId: "latest", Path: "relative"},
		{Repository: remoteRepo(), SnapshotId: "latest", Path: "/a/../b"},
		{Repository: remoteRepo(), SnapshotId: "latest", Path: "/a\nb"},
	} {
		_, err := p.Ls(t.Context(), testProbeID, remotesBk(), op)
		mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC)
	}
	if len(sup.calls) != 0 {
		t.Errorf("restic started for rejected input: %v", sup.verbs())
	}
}

func TestProberInitThenReadsTheConfig(t *testing.T) {
	t.Parallel()
	p, sup, _ := newTestProber(t, func(c call) reply {
		if c.verb == "init" {
			return reply{stdout: fixture(t, "init.out")}
		}
		return reply{stdout: fixture(t, "cat_config.out")}
	})
	cfg, err := p.Init(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticInit{Repository: remoteRepo()})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GetId() != "2d1912a057d761ffe3979dcdddfff925ef2ba21a67ea44b3e7b948a3cbaf4231" || cfg.GetVersion() != 2 {
		t.Errorf("config = %v", cfg)
	}
	if got := sup.verbs(); !slices.Equal(got, []string{"restic init", "restic cat"}) {
		t.Errorf("calls = %v", got)
	}
	if want := append(remoteGlobals(false), "--no-lock", "cat", "config"); !slices.Equal(sup.calls[1].args, want) {
		t.Errorf("cat args = %q\nwant %q", sup.calls[1].args, want)
	}
}

func TestProberErrorCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		reply reply
		run   func(p *Prober) error
		want  syncrv1.JobErrorCode
	}{
		{
			"init over a repository", exitError(1, "Fatal: create repository at rclone:bk:x failed: config file already exists"),
			func(p *Prober) error {
				_, err := p.Init(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticInit{Repository: remoteRepo()})
				return err
			},
			syncrv1.JobErrorCode_JOB_ERROR_CODE_ALREADY_EXISTS,
		},
		{
			"config with the wrong password", exitError(12, "Fatal: wrong password or no key found"),
			func(p *Prober) error {
				_, err := p.Config(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticConfig{Repository: remoteRepo()})
				return err
			},
			syncrv1.JobErrorCode_JOB_ERROR_CODE_WRONG_PASSWORD,
		},
		{
			"config without a repository", exitError(10, "Fatal: repository does not exist"),
			func(p *Prober) error {
				_, err := p.Config(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticConfig{Repository: remoteRepo()})
				return err
			},
			syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_NOT_FOUND,
		},
		{
			"ls of an unknown snapshot", exitError(1, `no matching ID found for prefix \"deadbeef\"`),
			func(p *Prober) error {
				_, err := p.Ls(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticLs{Repository: remoteRepo(), SnapshotId: "deadbeef", Path: "/"})
				return err
			},
			syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND,
		},
		{
			"garbage instead of a config", reply{stdout: []string{"not json"}},
			func(p *Prober) error {
				_, err := p.Config(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticConfig{Repository: remoteRepo()})
				return err
			},
			syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, _, _ := newTestProber(t, func(call) reply { return tc.reply })
			mustFail(t, tc.run(p), tc.want)
		})
	}
}

func TestProberScrubsSecrets(t *testing.T) {
	t.Parallel()
	p, _, _ := newTestProber(t, func(call) reply {
		return reply{exit: 1, stderr: []string{"cannot open " + testPassword + " with key " + testSecret}}
	})
	_, err := p.Snapshots(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticSnapshots{Repository: remoteRepo()})
	if err == nil {
		t.Fatal("no error")
	}
	mustNotLeak(t, err.Error())
}

func TestProberRejectsABadProbeID(t *testing.T) {
	t.Parallel()
	p, sup, _ := newTestProber(t, func(call) reply { return reply{} })
	_, err := p.Snapshots(t.Context(), "../escape", remotesBk(), &syncrv1.ProbeResticSnapshots{Repository: remoteRepo()})
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL)
	if len(sup.calls) != 0 {
		t.Error("restic started")
	}
}

func TestProberPassesCancellationThrough(t *testing.T) {
	t.Parallel()
	p, _, _ := newTestProber(t, func(call) reply { return reply{err: &proc.CanceledError{Cause: context.Canceled}} })
	_, err := p.Snapshots(t.Context(), testProbeID, remotesBk(), &syncrv1.ProbeResticSnapshots{Repository: remoteRepo()})
	if !runner.Canceled(err) {
		t.Errorf("err = %v, want a cancellation", err)
	}
}
