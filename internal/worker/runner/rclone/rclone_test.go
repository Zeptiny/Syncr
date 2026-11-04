package rclone

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// fakeSupervisor records the spec, replays output lines and returns a
// canned result.
type fakeSupervisor struct {
	lines  []string
	result proc.Result
	err    error

	called  bool
	spec    proc.Spec
	dirSeen bool // the scratch dir existed while the child "ran"
}

func (f *fakeSupervisor) Run(_ context.Context, spec proc.Spec) (proc.Result, error) {
	f.called = true
	f.spec = spec
	if _, err := os.Stat(spec.Env[rcloneenv.EnvConfig]); err == nil {
		f.dirSeen = true
	}
	for _, l := range f.lines {
		spec.Stderr(proc.Line{Data: []byte(l)})
	}
	return f.result, f.err
}

type fixture struct {
	scratch string
	data    string // the only local root
	sup     *fakeSupervisor
	runner  *Runner
}

func newFixture(t *testing.T, sup *fakeSupervisor) *fixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{scratch: filepath.Join(base, "scratch"), data: filepath.Join(base, "data"), sup: sup}
	if err := os.MkdirAll(f.data, 0o700); err != nil {
		t.Fatal(err)
	}
	policy, err := localfs.NewPolicy([]string{f.data})
	if err != nil {
		t.Fatal(err)
	}
	f.runner, err = New(Config{
		Binary:     "/opt/syncr/rclone",
		Supervisor: sup,
		Env:        rcloneenv.NewBuilder(policy),
		Paths:      policy,
		ScratchDir: f.scratch,
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// scratchEntries lists what is left in the scratch root.
func (f *fixture) scratchEntries(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(f.scratch)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func transferJob(src, dst *syncrv1.Location, remotes ...*syncrv1.RemoteDefinition) runner.Job {
	return runner.Job{
		ID: "0b8a5a8e-6c1f-4a8e-9a43-1d2f3e4c5b6a",
		Spec: &syncrv1.JobSpec{
			Remotes: remotes,
			Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{
				Mode:        modeCopy,
				Source:      src,
				Destination: dst,
				Options:     &syncrv1.RcloneTransferOptions{Transfers: ptr[int32](2)},
			}},
		},
	}
}

func sftpRemote(alias, pass string) *syncrv1.RemoteDefinition {
	return &syncrv1.RemoteDefinition{
		Alias:       alias,
		BackendType: "sftp",
		Options:     map[string]string{"host": "sftp.example.com", "user": "syncr"},
		Secrets:     map[string]string{"pass": pass},
		ObscureKeys: []string{"pass"},
	}
}

func TestKind(t *testing.T) {
	t.Parallel()
	f := newFixture(t, &fakeSupervisor{})
	if f.runner.Kind() != "rclone_transfer" || !link.IsKind(f.runner.Kind()) {
		t.Errorf("Kind() = %q", f.runner.Kind())
	}
}

func TestNewValidatesConfig(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{}); err == nil {
		t.Fatal("New(Config{}) succeeded")
	} else {
		for _, want := range []string{"binary", "supervisor", "env builder", "path policy", "scratch dir"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %s", err, want)
			}
		}
	}
}

func TestRunSuccess(t *testing.T) {
	t.Parallel()
	sup := &fakeSupervisor{lines: fixtureLines(t, "copy_progress.jsonl"), result: proc.Result{ExitCode: 0, Duration: 5 * time.Second}}
	f := newFixture(t, sup)
	dst := filepath.Join(f.data, "backup", "..", "backup")
	job := transferJob(
		&syncrv1.Location{Remote: "box", Path: "/upload/photos"},
		&syncrv1.Location{Path: dst},
		sftpRemote("box", "pa:ss w'rd"),
	)

	var emitted []runner.Progress
	res, err := f.runner.Run(t.Context(), job, func(p runner.Progress) { emitted = append(emitted, p) })
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}

	// Arguments: fixed flags, then the paths after "--"; the local path is
	// the cleaned one returned by the policy.
	wantArgs := []string{
		"copy", "--use-json-log", "--log-level=NOTICE", "--stats=1s", "--stats-log-level=NOTICE", "--transfers=2",
		"--", "box:/upload/photos", filepath.Join(f.data, "backup"),
	}
	if !slices.Equal(sup.spec.Args, wantArgs) {
		t.Errorf("args =\n%q\nwant\n%q", sup.spec.Args, wantArgs)
	}
	if sup.spec.Path != "/opt/syncr/rclone" {
		t.Errorf("path = %q", sup.spec.Path)
	}

	// Environment: remote definition plus isolation inside the job's dir.
	env := sup.spec.Env
	if env["RCLONE_CONFIG_BOX_TYPE"] != "sftp" || env["RCLONE_CONFIG_BOX_HOST"] != "sftp.example.com" {
		t.Errorf("remote env = %q", env)
	}
	if p := env["RCLONE_CONFIG_BOX_PASS"]; p == "" || p == "pa:ss w'rd" {
		t.Errorf("pass not obscured: %q", p)
	}
	if env[rcloneenv.EnvAskPassword] != "false" || filepath.Dir(env[rcloneenv.EnvConfig]) != sup.spec.Dir ||
		filepath.Dir(sup.spec.Dir) != f.scratch || env[rcloneenv.EnvCacheDir] != filepath.Join(sup.spec.Dir, "cache") {
		t.Errorf("isolation env = %q, dir %q", env, sup.spec.Dir)
	}
	if !strings.HasPrefix(filepath.Base(sup.spec.Dir), "rclone-0b8a5a8e-6c1f-4a8e-9a43-1d2f3e4c5b6a-") {
		t.Errorf("scratch dir name = %q", filepath.Base(sup.spec.Dir))
	}
	for _, arg := range sup.spec.Args {
		if strings.Contains(arg, "pa:ss") || strings.Contains(arg, env["RCLONE_CONFIG_BOX_PASS"]) {
			t.Errorf("secret on the command line: %q", arg)
		}
	}

	// Progress and result.
	if len(emitted) != 3 || emitted[2].Bytes != 2800006 || emitted[2].Files != 3 {
		t.Errorf("emitted %+v", emitted)
	}
	if res.ExitCode != 0 || res.Stats.Bytes != 2800006 || res.Stats.TotalFiles != 3 || res.Stats.Elapsed != 5*time.Second {
		t.Errorf("result = %+v", res)
	}

	// The scratch dir existed during the run and is gone afterwards.
	if !sup.dirSeen {
		t.Error("config file missing while rclone ran")
	}
	if left := f.scratchEntries(t); len(left) != 0 {
		t.Errorf("scratch dir not cleaned up: %q", left)
	}
}

func TestRunRejectsBeforeStarting(t *testing.T) {
	t.Parallel()
	const secret = "TOP-SECRET-PASS"
	box := sftpRemote("box", secret)
	inside := func(f *fixture) *syncrv1.Location { return &syncrv1.Location{Path: filepath.Join(f.data, "x")} }
	outside := func(f *fixture) *syncrv1.Location { return &syncrv1.Location{Path: filepath.Dir(f.data)} }
	remote := func(alias string) func(*fixture) *syncrv1.Location {
		return func(*fixture) *syncrv1.Location { return &syncrv1.Location{Remote: alias, Path: "p"} }
	}
	none := func(*fixture) *syncrv1.Location { return nil }
	union := func(f *fixture) *syncrv1.RemoteDefinition {
		return &syncrv1.RemoteDefinition{Alias: "un", BackendType: "union", UnionUpstreams: []*syncrv1.UnionUpstream{
			{Location: &syncrv1.Location{Path: filepath.Join(f.data, "a")}},
			{Location: &syncrv1.Location{Path: filepath.Dir(f.data)}},
		}}
	}

	tests := []struct {
		name     string
		src, dst func(*fixture) *syncrv1.Location
		remotes  func(*fixture) []*syncrv1.RemoteDefinition
		mutate   func(*syncrv1.RcloneTransfer)
		code     syncrv1.JobErrorCode
	}{
		{"local source outside roots", outside, remote("box"), nil, nil, syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED},
		{"local destination outside roots", remote("box"), outside, nil, nil, syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED},
		{"relative local path", func(*fixture) *syncrv1.Location { return &syncrv1.Location{Path: "rel/dir"} }, remote("box"), nil, nil, syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED},
		{"connection string as local path", func(*fixture) *syncrv1.Location { return &syncrv1.Location{Path: ":local,copy_links:/etc"} }, remote("box"), nil, nil, syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED},
		{"undefined remote", inside, remote("nope"), nil, nil, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC},
		{"missing source", none, inside, nil, nil, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC},
		{"missing destination", inside, none, nil, nil, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC},
		{"NUL in remote path", func(*fixture) *syncrv1.Location { return &syncrv1.Location{Remote: "box", Path: "a\x00b"} }, inside, nil, nil, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC},
		{"invalid remote", inside, remote("box"), func(*fixture) []*syncrv1.RemoteDefinition {
			return []*syncrv1.RemoteDefinition{box, {Alias: "evil", BackendType: "local"}}
		}, nil, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC},
		{"union upstream outside roots", inside, remote("un"), func(f *fixture) []*syncrv1.RemoteDefinition {
			return []*syncrv1.RemoteDefinition{box, union(f)}
		}, nil, syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED},
		{"invalid options", inside, remote("box"), nil, func(t *syncrv1.RcloneTransfer) {
			t.Options = &syncrv1.RcloneTransferOptions{Checksum: true, SizeOnly: true}
		}, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC},
		{"no mode", inside, remote("box"), nil, func(t *syncrv1.RcloneTransfer) { t.Mode = 0 }, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, &fakeSupervisor{})
			remotes := []*syncrv1.RemoteDefinition{box}
			if tt.remotes != nil {
				remotes = tt.remotes(f)
			}
			job := transferJob(tt.src(f), tt.dst(f), remotes...)
			if tt.mutate != nil {
				tt.mutate(job.Spec.GetRcloneTransfer())
			}
			res, err := f.runner.Run(t.Context(), job, nil)
			if err == nil || runner.CodeOf(err) != tt.code {
				t.Fatalf("Run() = %v (code %v), want code %v", err, runner.CodeOf(err), tt.code)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error leaks the secret: %v", err)
			}
			if f.sup.called {
				t.Error("rclone was started")
			}
			if res.ExitCode != -1 {
				t.Errorf("exit code = %d, want -1", res.ExitCode)
			}
			if left := f.scratchEntries(t); len(left) != 0 {
				t.Errorf("scratch dir not cleaned up: %q", left)
			}
		})
	}
}

func TestRunWrongOperation(t *testing.T) {
	t.Parallel()
	f := newFixture(t, &fakeSupervisor{})
	for _, spec := range []*syncrv1.JobSpec{nil, {}, {Operation: &syncrv1.JobSpec_ResticInit{ResticInit: &syncrv1.ResticInit{}}}} {
		_, err := f.runner.Run(t.Context(), runner.Job{ID: "j", Spec: spec}, nil)
		if runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC {
			t.Errorf("spec %v: err = %v", spec, err)
		}
	}
}

func TestRunAlreadyCanceled(t *testing.T) {
	t.Parallel()
	f := newFixture(t, &fakeSupervisor{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := f.runner.Run(ctx, transferJob(&syncrv1.Location{Path: f.data}, &syncrv1.Location{Path: f.data}), nil)
	if !runner.Canceled(err) || f.sup.called {
		t.Errorf("Run() = %v, started %v", err, f.sup.called)
	}
}

func TestRunExitCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		exit int
		code syncrv1.JobErrorCode // UNSPECIFIED: success
		text string
	}{
		{0, 0, ""},
		{1, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "error (exit code 1)"},
		{2, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "rejected the arguments"},
		{3, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "directory not found"},
		{4, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "file not found"},
		{5, syncrv1.JobErrorCode_JOB_ERROR_CODE_TEMPORARY, "temporary"},
		{6, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "retrying will not fix"},
		{7, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "fatal"},
		{8, syncrv1.JobErrorCode_JOB_ERROR_CODE_LIMIT_REACHED, "max_transfer_bytes"},
		{9, 0, ""},
		{10, syncrv1.JobErrorCode_JOB_ERROR_CODE_LIMIT_REACHED, "max_duration"},
		{11, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "unexpected exit code (exit code 11)"},
		{-1, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "terminated by a signal"},
	}
	for _, tt := range tests {
		sup := &fakeSupervisor{result: proc.Result{ExitCode: tt.exit}}
		if tt.exit != 0 {
			sup.err = &proc.ExitError{Path: "rclone", ExitCode: tt.exit, Err: errors.New("exit status")}
		}
		f := newFixture(t, sup)
		res, err := f.runner.Run(t.Context(), transferJob(&syncrv1.Location{Path: f.data}, &syncrv1.Location{Path: f.data}), nil)
		if res.ExitCode != tt.exit {
			t.Errorf("exit %d: result exit code %d", tt.exit, res.ExitCode)
		}
		if tt.code == 0 {
			if err != nil {
				t.Errorf("exit %d: Run() = %v, want success", tt.exit, err)
			}
			continue
		}
		if runner.CodeOf(err) != tt.code || !strings.Contains(err.Error(), tt.text) || !strings.HasPrefix(err.Error(), "rclone copy failed: ") {
			t.Errorf("exit %d: Run() = %v (code %v), want code %v and %q", tt.exit, err, runner.CodeOf(err), tt.code, tt.text)
		}
		if _, ok := errors.AsType[*proc.ExitError](err); !ok {
			t.Errorf("exit %d: error does not unwrap to *proc.ExitError", tt.exit)
		}
	}
}

func TestRunFailureMessageIsScrubbed(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-Pa55"
	f := newFixture(t, &fakeSupervisor{})
	// The child's environment is only known once the runner built it, so
	// the fake echoes the obscured value it receives.
	f.sup.err = &proc.ExitError{Path: "rclone", ExitCode: 1, Err: errors.New("exit status 1")}
	f.sup.result = proc.Result{ExitCode: 1}
	sup := &echoSupervisor{fakeSupervisor: f.sup, secret: secret}
	f.runner.cfg.Supervisor = sup

	job := transferJob(&syncrv1.Location{Remote: "box", Path: "x"}, &syncrv1.Location{Path: f.data}, sftpRemote("box", secret))
	_, err := f.runner.Run(t.Context(), job, nil)
	if err == nil {
		t.Fatal("Run() succeeded")
	}
	msg := err.Error()
	if strings.Contains(msg, secret) || strings.Contains(msg, sup.obscured) || sup.obscured == "" {
		t.Errorf("message leaks a secret: %q", msg)
	}
	for _, want := range []string{"rclone copy failed: error (exit code 1): ", "box: login with [redacted] failed", "obscured [redacted] rejected", "url p%40ss=[redacted]"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
}

// echoSupervisor writes error lines containing the secret in several forms.
type echoSupervisor struct {
	*fakeSupervisor
	secret   string
	obscured string
}

func (e *echoSupervisor) Run(ctx context.Context, spec proc.Spec) (proc.Result, error) {
	e.obscured = spec.Env["RCLONE_CONFIG_BOX_PASS"]
	e.lines = []string{
		`{"level":"error","msg":"login with ` + e.secret + ` failed","object":"box"}`,
		`{"level":"error","msg":"obscured ` + e.obscured + ` rejected"}`,
		`{"level":"error","msg":"url p%40ss=` + e.secret + `"}`,
	}
	return e.fakeSupervisor.Run(ctx, spec)
}

func TestRunSupervisorErrors(t *testing.T) {
	t.Parallel()
	canceled := &proc.CanceledError{Path: "rclone", ExitCode: 130, Cause: context.Canceled}
	tests := []struct {
		name   string
		err    error
		exit   int
		check  func(error) bool
		reason string
	}{
		{"canceled", canceled, 130, func(err error) bool { return errors.Is(err, canceled) && runner.Canceled(err) }, "passes *proc.CanceledError through"},
		{"start", &proc.StartError{Path: "rclone", Err: exec.ErrNotFound}, -1, func(err error) bool {
			return runner.CodeOf(err) == syncrv1.JobErrorCode_JOB_ERROR_CODE_TOOL_UNAVAILABLE && errors.Is(err, exec.ErrNotFound)
		}, "TOOL_UNAVAILABLE"},
		{"wait delay after success", exec.ErrWaitDelay, 0, func(err error) bool { return err == nil }, "success"},
		{"wait delay after failure", exec.ErrWaitDelay, 3, func(err error) bool {
			return runner.CodeOf(err) == syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL
		}, "INTERNAL"},
	}
	for _, tt := range tests {
		f := newFixture(t, &fakeSupervisor{err: tt.err, result: proc.Result{ExitCode: tt.exit}})
		_, err := f.runner.Run(t.Context(), transferJob(&syncrv1.Location{Path: f.data}, &syncrv1.Location{Path: f.data}), nil)
		if !tt.check(err) {
			t.Errorf("%s: Run() = %v, want %s", tt.name, err, tt.reason)
		}
		if left := f.scratchEntries(t); len(left) != 0 {
			t.Errorf("%s: scratch dir not cleaned up: %q", tt.name, left)
		}
	}
}

func TestSafeName(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"0b8a5a8e-6c1f-4a8e-9a43-1d2f3e4c5b6a": "0b8a5a8e-6c1f-4a8e-9a43-1d2f3e4c5b6a",
		"../../etc/passwd":                     "etcpasswd",
		`C:\x*?`:                               "Cx",
		strings.Repeat("a", 100):               strings.Repeat("a", 36),
		"":                                     "",
	} {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
}
