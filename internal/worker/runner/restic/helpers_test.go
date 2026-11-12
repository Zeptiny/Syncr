package restic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

const (
	testPassword  = "hunter2-repo-password"
	testSecret    = "s3cr3t-access-key-value"
	fakeResticBin = "/fake/bin/restic"
	fakeRcloneBin = "/fake/bin/rclone"
)

// fakeSupervisor records the spec it was given and replays scripted output.
type fakeSupervisor struct {
	mu     sync.Mutex
	spec   proc.Spec
	called int

	stdout []string
	stderr []string
	result proc.Result
	err    error
	// onRun runs inside Run, before output is replayed.
	onRun func(spec proc.Spec)
}

func (f *fakeSupervisor) Run(_ context.Context, spec proc.Spec) (proc.Result, error) {
	f.mu.Lock()
	f.spec = spec
	f.called++
	f.mu.Unlock()
	if f.onRun != nil {
		f.onRun(spec)
	}
	for _, l := range f.stdout {
		spec.Stdout(proc.Line{Data: []byte(l)})
	}
	for _, l := range f.stderr {
		spec.Stderr(proc.Line{Data: []byte(l)})
	}
	return f.result, f.err
}

// exited scripts a restic exit with code.
func (f *fakeSupervisor) exited(code int) *fakeSupervisor {
	f.result = proc.Result{ExitCode: code}
	if code != 0 {
		f.err = &proc.ExitError{Path: fakeResticBin, ExitCode: code}
	}
	return f
}

// fakeRemotes defines every remote as an rclone remote of its backend type
// with all options and secrets as RCLONE_CONFIG_<ALIAS>_<OPTION>.
type fakeRemotes struct{ err error }

func (f fakeRemotes) Env(remotes []*syncrv1.RemoteDefinition) (map[string]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	env := map[string]string{}
	for _, r := range remotes {
		prefix := "RCLONE_CONFIG_" + strings.ToUpper(r.GetAlias()) + "_"
		env[prefix+"TYPE"] = r.GetBackendType()
		for k, v := range r.GetOptions() {
			env[prefix+strings.ToUpper(k)] = v
		}
		for k, v := range r.GetSecrets() {
			env[prefix+strings.ToUpper(k)] = v
		}
	}
	return env, nil
}

// rootPolicy allows paths inside root and resolves them with filepath.Clean.
type rootPolicy struct{ root string }

func (p rootPolicy) Resolve(path string) (string, error) {
	clean := filepath.Clean(path)
	rel, err := filepath.Rel(p.root, clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("outside local_roots (%s)", p.root)
	}
	return clean, nil
}

// testEnv is a configured engine with fakes.
type testEnv struct {
	t       *testing.T
	sup     *fakeSupervisor
	root    string // allowed local root
	scratch string
	cache   string
	runners map[string]runner.Runner
	cfg     Config
}

func newTestEnv(t *testing.T, mutate ...func(*Config)) *testEnv {
	t.Helper()
	base := t.TempDir()
	te := &testEnv{
		t:       t,
		sup:     &fakeSupervisor{},
		root:    filepath.Join(base, "roots"),
		scratch: filepath.Join(base, "scratch"),
		cache:   filepath.Join(base, "cache"),
	}
	te.cfg = Config{
		ResticPath:  fakeResticBin,
		RclonePath:  fakeRcloneBin,
		Supervisor:  te.sup,
		Remotes:     fakeRemotes{},
		Paths:       rootPolicy{root: te.root},
		ScratchRoot: te.scratch,
		CacheDir:    te.cache,
	}
	for _, m := range mutate {
		m(&te.cfg)
	}
	rs, err := New(te.cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	te.runners = map[string]runner.Runner{}
	for _, r := range rs {
		te.runners[r.Kind()] = r
	}
	return te
}

// local returns an absolute path inside the allowed root.
func (te *testEnv) local(elem ...string) string {
	return filepath.Join(append([]string{te.root}, elem...)...)
}

// run executes spec on the runner of its kind and returns the result, the
// error and the progress snapshots emitted.
func (te *testEnv) run(spec *syncrv1.JobSpec) (runner.Result, []runner.Progress, error) {
	te.t.Helper()
	var snaps []runner.Progress
	r, ok := te.runners[kindOf(spec)]
	if !ok {
		te.t.Fatalf("no runner for %q", kindOf(spec))
	}
	res, err := r.Run(te.t.Context(), runner.Job{ID: "job-1", Spec: spec}, func(p runner.Progress) { snaps = append(snaps, p) })
	return res, snaps, err
}

func kindOf(spec *syncrv1.JobSpec) string {
	switch spec.GetOperation().(type) {
	case *syncrv1.JobSpec_ResticInit:
		return kindInit
	case *syncrv1.JobSpec_ResticBackup:
		return kindBackup
	case *syncrv1.JobSpec_ResticForget:
		return kindForget
	case *syncrv1.JobSpec_ResticPrune:
		return kindPrune
	case *syncrv1.JobSpec_ResticCheck:
		return kindCheck
	case *syncrv1.JobSpec_ResticRestore:
		return kindRestore
	case *syncrv1.JobSpec_ResticSnapshots:
		return kindSnapshots
	case *syncrv1.JobSpec_ResticDeleteData:
		return kindDeleteData
	}
	return ""
}

// s3Remote is a remote definition carrying a secret.
func s3Remote() *syncrv1.RemoteDefinition {
	return &syncrv1.RemoteDefinition{
		Alias:       "bk",
		BackendType: "s3",
		Options:     map[string]string{"provider": "Minio", "endpoint": "http://127.0.0.1:9000"},
		Secrets:     map[string]string{"secret_access_key": testSecret},
	}
}

// remoteRepo is a repository on remote bk.
func remoteRepo() *syncrv1.ResticRepository {
	return &syncrv1.ResticRepository{
		Location: &syncrv1.Location{Remote: "bk", Path: "bucket/repo one"},
		Password: testPassword,
	}
}

func localRepo(path string) *syncrv1.ResticRepository {
	return &syncrv1.ResticRepository{Location: &syncrv1.Location{Path: path}, Password: testPassword}
}

func specWith(remotes []*syncrv1.RemoteDefinition, op func(*syncrv1.JobSpec)) *syncrv1.JobSpec {
	s := &syncrv1.JobSpec{Remotes: remotes}
	op(s)
	return s
}

func backupSpec(repo *syncrv1.ResticRepository, b *syncrv1.ResticBackup) *syncrv1.JobSpec {
	b.Repository = repo
	return specWith([]*syncrv1.RemoteDefinition{s3Remote()}, func(s *syncrv1.JobSpec) {
		s.Operation = &syncrv1.JobSpec_ResticBackup{ResticBackup: b}
	})
}

// fixture returns the lines of testdata/name.
func fixture(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	var lines []string
	for l := range strings.SplitSeq(strings.TrimRight(string(data), "\n"), "\n") {
		lines = append(lines, l)
	}
	return lines
}

func mustFail(t *testing.T, err error, code syncrv1.JobErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want code %v", code)
	}
	if got := runner.CodeOf(err); got != code {
		t.Fatalf("code = %v (%v), want %v", got, err, code)
	}
}

var errBoom = errors.New("boom")

type runnerProgress = runner.Progress

func runnerJob(spec *syncrv1.JobSpec) runner.Job { return runner.Job{ID: "job-1", Spec: spec} }
