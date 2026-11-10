package restic

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

func pruneSpec() *syncrv1.JobSpec {
	return remoteSpec(func(s *syncrv1.JobSpec) {
		s.Operation = &syncrv1.JobSpec_ResticPrune{ResticPrune: &syncrv1.ResticPrune{Repository: remoteRepo()}}
	})
}

func TestNewRegistersEveryResticKind(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	reg := runner.NewRegistry()
	rs := make([]runner.Runner, 0, len(te.runners))
	for _, r := range te.runners {
		rs = append(rs, r)
	}
	if err := reg.Register(rs...); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"restic_backup", "restic_check", "restic_delete_data", "restic_forget", "restic_init",
		"restic_prune", "restic_restore", "restic_snapshots",
	}
	if got := reg.Kinds(); !slices.Equal(got, want) {
		t.Errorf("Kinds() = %v, want %v", got, want)
	}
	// Every restic kind of the wire contract has a runner.
	for _, k := range link.Kinds() {
		if strings.HasPrefix(k, "restic_") {
			if _, ok := reg.Lookup(k); !ok {
				t.Errorf("no runner for wire kind %q", k)
			}
		}
	}
}

func TestNewValidatesConfig(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	tests := map[string]func(*Config){
		"restic path": func(c *Config) { c.ResticPath = "" },
		"rclone path": func(c *Config) { c.RclonePath = "" },
		"scratch":     func(c *Config) { c.ScratchRoot = "" },
		"supervisor":  func(c *Config) { c.Supervisor = nil },
		"remotes":     func(c *Config) { c.Remotes = nil },
		"paths":       func(c *Config) { c.Paths = nil },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := te.cfg
			mutate(&cfg)
			if _, err := New(cfg); err == nil {
				t.Error("New succeeded with an incomplete config")
			}
		})
	}
}

func TestEnvironment(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	te.sup.exited(0)
	if _, _, err := te.run(pruneSpec()); err != nil {
		t.Fatal(err)
	}
	env := te.sup.spec.Env

	if env["RESTIC_PASSWORD"] != testPassword {
		t.Errorf("RESTIC_PASSWORD = %q, want the repository password", env["RESTIC_PASSWORD"])
	}
	if env["RESTIC_CACHE_DIR"] != te.cache {
		t.Errorf("RESTIC_CACHE_DIR = %q, want %q", env["RESTIC_CACHE_DIR"], te.cache)
	}
	if env["RESTIC_PROGRESS_FPS"] != "1" {
		t.Errorf("RESTIC_PROGRESS_FPS = %q, want 1", env["RESTIC_PROGRESS_FPS"])
	}
	// The remote reaches restic (and so the rclone it starts) as environment.
	if env["RCLONE_CONFIG_BK_TYPE"] != "s3" || env["RCLONE_CONFIG_BK_SECRET_ACCESS_KEY"] != testSecret {
		t.Errorf("remote environment missing: %v", keys(env))
	}
	for _, name := range []string{"TMPDIR", "TEMP", "TMP"} {
		if env[name] == "" || env[name] != te.sup.spec.Dir {
			t.Errorf("%s = %q, want the job scratch directory %q", name, env[name], te.sup.spec.Dir)
		}
	}
	// No secret on the command line, in any form.
	for _, arg := range te.sup.spec.Args {
		for _, secret := range []string{testPassword, testSecret} {
			if strings.Contains(arg, secret) {
				t.Errorf("secret found in argument %q", arg)
			}
		}
	}
	for _, flag := range []string{"--password-file", "--password-command", "-p"} {
		if slices.Contains(te.sup.spec.Args, flag) {
			t.Errorf("unexpected flag %s", flag)
		}
	}
}

func TestLocalRepositoryNeedsNoRcloneEnvironment(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, func(c *Config) { c.Remotes = fakeRemotes{err: errBoom} })
	te.sup.exited(0)
	spec := specWith(nil, func(s *syncrv1.JobSpec) {
		s.Operation = &syncrv1.JobSpec_ResticPrune{ResticPrune: &syncrv1.ResticPrune{Repository: localRepo(te.local("repo"))}}
	})
	if _, _, err := te.run(spec); err != nil {
		t.Fatal(err)
	}
	for name := range te.sup.spec.Env {
		if strings.HasPrefix(name, "RCLONE_") {
			t.Errorf("local repository got %s", name)
		}
	}
	for _, a := range te.sup.spec.Args {
		if strings.Contains(a, "rclone") {
			t.Errorf("local repository got rclone argument %q", a)
		}
	}
}

func TestWithoutCacheDirUsesNoCache(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, func(c *Config) { c.CacheDir = "" })
	te.sup.exited(0)
	if _, _, err := te.run(pruneSpec()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(te.sup.spec.Args, "--no-cache") {
		t.Errorf("args = %q, want --no-cache", te.sup.spec.Args)
	}
	if v, ok := te.sup.spec.Env["RESTIC_CACHE_DIR"]; ok {
		t.Errorf("RESTIC_CACHE_DIR = %q without a cache dir", v)
	}
}

func TestRemoteEnvIsRestrictedToRcloneConfig(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, func(c *Config) {
		c.Remotes = envFunc(func([]*syncrv1.RemoteDefinition) (map[string]string, error) {
			return map[string]string{"RESTIC_PASSWORD": "attacker", "RCLONE_CONFIG_BK_TYPE": "s3"}, nil
		})
	})
	_, _, err := te.run(pruneSpec())
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL)
	if te.sup.called != 0 {
		t.Error("restic started with a foreign environment variable")
	}
}

type envFunc func([]*syncrv1.RemoteDefinition) (map[string]string, error)

func (f envFunc) Env(r []*syncrv1.RemoteDefinition) (map[string]string, error) { return f(r) }

func TestRemoteEnvFailureIsInvalidSpec(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, func(c *Config) { c.Remotes = fakeRemotes{err: errors.New("unsupported backend widget")} })
	_, _, err := te.run(pruneSpec())
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC)
}

func TestScratchDirectoryLifecycle(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	var existedDuring bool
	var mode os.FileMode
	te.sup.exited(0).onRun = func(spec proc.Spec) {
		fi, err := os.Stat(spec.Dir)
		existedDuring = err == nil && fi.IsDir()
		if err == nil {
			mode = fi.Mode().Perm()
		}
		// A leftover of a crashed run must not leak into this one.
	}
	// Leftover from an earlier crashed run with the same job ID.
	if err := os.MkdirAll(te.scratch+"/restic-job-1", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(te.scratch+"/restic-job-1/stale", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := te.run(pruneSpec()); err != nil {
		t.Fatal(err)
	}
	if !existedDuring {
		t.Error("scratch directory did not exist while restic ran")
	}
	if runtime.GOOS != "windows" && mode != 0o700 {
		t.Errorf("scratch mode = %o, want 700", mode)
	}
	if _, err := os.Stat(te.sup.spec.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("scratch directory still exists after the job: %v", err)
	}
}

func TestInvalidJobIDIsRejected(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	for _, id := range []string{"", "../escape", "a/b", `a\b`, ".", "a b"} {
		_, err := te.runners[kindPrune].Run(t.Context(), runner.Job{ID: id, Spec: pruneSpec()}, func(runner.Progress) {})
		mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL)
	}
	if te.sup.called != 0 {
		t.Error("restic started with an unsafe job ID")
	}
}

func TestSupervisorErrors(t *testing.T) {
	t.Parallel()
	t.Run("start error", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		te.sup.result = proc.Result{ExitCode: -1}
		te.sup.err = &proc.StartError{Path: fakeResticBin, Err: os.ErrNotExist}
		res, _, err := te.run(pruneSpec())
		mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_TOOL_UNAVAILABLE)
		if res.ExitCode != -1 {
			t.Errorf("ExitCode = %d, want -1", res.ExitCode)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		te.sup.result = proc.Result{ExitCode: 130}
		te.sup.err = &proc.CanceledError{Path: fakeResticBin, ExitCode: 130, Cause: context.Canceled}
		te.sup.stdout = []string{`{"message_type":"status","bytes_done":10,"total_bytes":100,"files_done":1,"total_files":4}`}
		res, _, err := te.run(backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{te.local("a")}}))
		if _, ok := errors.AsType[*proc.CanceledError](err); !ok || !runner.Canceled(err) {
			t.Fatalf("err = %v, want *proc.CanceledError", err)
		}
		if res.Stats.Bytes != 10 || res.Stats.TotalBytes != 100 || res.Output != nil || res.ExitCode != 130 {
			t.Errorf("result = %+v, want the last known stats and no output", res)
		}
	})
}

func TestExitCodes(t *testing.T) {
	t.Parallel()
	failed := syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED
	tests := []struct {
		code     int
		sentinel error
		wire     syncrv1.JobErrorCode
		contains string
	}{
		{1, nil, failed, "command failed (exit code 1)"},
		{2, nil, failed, "internal error in restic (exit code 2)"},
		{3, nil, failed, "some source data could not be read (exit code 3)"}, // not a backup
		{10, ErrRepositoryNotFound, syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_NOT_FOUND, "repository does not exist (exit code 10)"},
		{11, ErrRepositoryLocked, syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_LOCKED, "repository is locked by another operation (exit code 11)"},
		{12, ErrWrongPassword, syncrv1.JobErrorCode_JOB_ERROR_CODE_WRONG_PASSWORD, "wrong repository password (exit code 12)"},
		{130, ErrInterrupted, failed, "interrupted (exit code 130)"},
		{42, nil, failed, "(exit code 42)"},
	}
	for _, tc := range tests {
		t.Run(tc.contains, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			te.sup.exited(tc.code).stderr = []string{`{"message_type":"exit_error","code":1,"message":"Fatal: details from restic"}`}
			res, _, err := te.run(pruneSpec())
			mustFail(t, err, tc.wire)
			if res.ExitCode != tc.code {
				t.Errorf("ExitCode = %d, want %d", res.ExitCode, tc.code)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("error = %q, want it to contain %q", err, tc.contains)
			}
			if !strings.Contains(err.Error(), "details from restic") {
				t.Errorf("error = %q, want restic's own message", err)
			}
			for _, s := range []error{ErrRepositoryNotFound, ErrRepositoryLocked, ErrWrongPassword, ErrInterrupted} {
				if got, want := errors.Is(err, s), errors.Is(s, tc.sentinel); got != want {
					t.Errorf("errors.Is(err, %v) = %v, want %v", s, got, want)
				}
			}
			if exitErr, ok := errors.AsType[*ExitError](err); !ok || exitErr.ExitCode != tc.code || exitErr.Verb != "prune" {
				t.Errorf("errors.As(*ExitError) = %+v, %v", exitErr, ok)
			}
		})
	}
}

func TestKilledBySignal(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	te.sup.result = proc.Result{ExitCode: -1}
	te.sup.err = &proc.ExitError{Path: fakeResticBin, ExitCode: -1}
	_, _, err := te.run(pruneSpec())
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED)
	if !strings.Contains(err.Error(), "terminated by a signal") {
		t.Errorf("error = %q", err)
	}
}

func TestFailureFallsBackToDiagnosticLines(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	te.sup.exited(1).stderr = []string{
		"rclone: 2026/11/04 04:01:31 CRITICAL: Failed to create file system for \"bk:x\": didn't find section in config file",
		"not json at all",
	}
	_, _, err := te.run(pruneSpec())
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED)
	if !strings.Contains(err.Error(), "didn't find section") {
		t.Errorf("error = %q, want the rclone diagnostic", err)
	}
}

func TestBackupWithUnreadableFilesSucceedsWithWarnings(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	te.sup.exited(3)
	te.sup.stdout = fixture(t, "backup_warn.out")
	te.sup.stderr = fixture(t, "backup_warn.stderr")
	res, _, err := te.run(backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{te.local("src")}}))
	if err != nil {
		t.Fatalf("err = %v, want nil: the snapshot exists", err)
	}
	if res.ExitCode != ExitIncomplete {
		t.Errorf("ExitCode = %d, want %d", res.ExitCode, ExitIncomplete)
	}
	out := res.Output.GetResticBackup()
	if out.GetSnapshotId() == "" || out.GetFilesNew() != 1 {
		t.Errorf("output = %v, want the snapshot of the partial backup", out)
	}
	if res.Stats.Errors != 1 {
		t.Errorf("Stats.Errors = %d, want 1 (one unreadable directory)", res.Stats.Errors)
	}
	if len(res.Warnings) != 2 ||
		res.Warnings[0] != "1 item could not be read; the snapshot is incomplete" ||
		!strings.Contains(res.Warnings[1], "/srv/data/src/locked") ||
		!strings.Contains(res.Warnings[1], "permission denied") {
		t.Errorf("Warnings = %q, want the count and the reason for the unreadable directory", res.Warnings)
	}
}

func TestBackupWarningsAreScrubbedAndBounded(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	te.sup.exited(3)
	te.sup.stdout = fixture(t, "backup_warn.out")[:1] // the summary only
	for i := range 30 {
		item := fmt.Sprintf("/data/f%02d", i)
		msg := "open " + item + ": denied " + testSecret + strings.Repeat("x", 800)
		te.sup.stderr = append(te.sup.stderr,
			fmt.Sprintf(`{"message_type":"error","error":{"message":%q},"during":"archival","item":%q}`, msg, item))
	}
	res, _, err := te.run(backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{te.local("src")}}))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if res.Stats.Errors != 30 || res.Warnings[0] != "30 items could not be read; the snapshot is incomplete" {
		t.Errorf("Errors = %d, Warnings[0] = %q", res.Stats.Errors, res.Warnings[0])
	}
	if len(res.Warnings) != runner.MaxWarnings {
		t.Errorf("len(Warnings) = %d, want %d", len(res.Warnings), runner.MaxWarnings)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, testSecret) {
			t.Errorf("warning leaks a secret: %q", w)
		}
		if len(w) > runner.MaxWarningBytes+len("...") {
			t.Errorf("warning has %d bytes", len(w))
		}
	}
}

func TestCleanBackupHasNoWarnings(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	te.sup.exited(0)
	te.sup.stdout = fixture(t, "backup.out")
	res, _, err := te.run(backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{te.local("src")}}))
	if err != nil || len(res.Warnings) != 0 {
		t.Errorf("err = %v, Warnings = %q", err, res.Warnings)
	}
}

func TestBackupExit3WithoutSnapshotFails(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	te.sup.exited(3)
	_, _, err := te.run(backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{te.local("src")}}))
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED)
}

func TestSuccessfulBackupWithoutSummaryIsInternalError(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	te.sup.exited(0)
	_, _, err := te.run(backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{te.local("src")}}))
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL)
}

// TestSecretsAreScrubbed checks that no failure path leaks the repository
// password or a remote secret, even when the tool echoes them.
func TestSecretsAreScrubbed(t *testing.T) {
	t.Parallel()
	leaky := func(prefix string) string {
		return prefix + " password=" + testPassword + " key=" + testSecret
	}
	tests := []struct {
		name  string
		setup func(te *testEnv)
		run   func(te *testEnv) error
	}{
		{"exit_error message", func(te *testEnv) {
			te.sup.exited(12).stderr = []string{`{"message_type":"exit_error","code":12,"message":"` + leaky("Fatal:") + `"}`}
		}, func(te *testEnv) error { _, _, err := te.run(pruneSpec()); return err }},
		{"diagnostic lines", func(te *testEnv) {
			te.sup.exited(1).stderr = []string{leaky("rclone: CRITICAL:")}
		}, func(te *testEnv) error { _, _, err := te.run(pruneSpec()); return err }},
		{"path policy error", func(*testEnv) {},
			func(te *testEnv) error {
				te.cfg.Paths = leakyPolicy{}
				rs, err := New(te.cfg)
				if err != nil {
					return err
				}
				spec := backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{te.local("a")}})
				_, err = rs[1].Run(t.Context(), runnerJob(spec), func(runner.Progress) {})
				return err
			}},
		{"remote env error", func(te *testEnv) {
			te.cfg.Remotes = fakeRemotes{err: errors.New(leaky("bad remote:"))}
		}, func(te *testEnv) error {
			rs, err := New(te.cfg)
			if err != nil {
				return err
			}
			_, err = rs[3].Run(t.Context(), runnerJob(pruneSpec()), func(runner.Progress) {})
			return err
		}},
		{"supervisor error", func(te *testEnv) {
			te.sup.err = errors.New(leaky("collect output:"))
		}, func(te *testEnv) error { _, _, err := te.run(pruneSpec()); return err }},
		{"check failure", func(te *testEnv) {
			te.sup.exited(1).stdout = []string{`{"message_type":"summary","num_errors":2}`}
			te.sup.stderr = []string{leaky("pack failed:")}
		}, func(te *testEnv) error {
			_, _, err := te.run(remoteSpec(func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticCheck{ResticCheck: &syncrv1.ResticCheck{Repository: remoteRepo()}}
			}))
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			tc.setup(te)
			err := tc.run(te)
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, secret := range []string{testPassword, testSecret} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error leaks a secret: %q", err)
				}
			}
			if code := runner.CodeOf(err); code == syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSPECIFIED {
				t.Errorf("code = %v", code)
			}
		})
	}
}

// TestSecretsAreScrubbedBeforeClipping puts secrets across every length
// limit restic's output is cut to: clipping before scrubbing would leave a
// prefix of the secret that matches nothing and leaks.
func TestSecretsAreScrubbedBeforeClipping(t *testing.T) {
	t.Parallel()
	straddle := func(limit int) string { return strings.Repeat("x", limit-8) + testSecret + " tail" }
	tests := []struct {
		name  string
		setup func(te *testEnv)
		check func(t *testing.T, res runner.Result, err error)
	}{
		{"diagnostic line", func(te *testEnv) {
			te.sup.exited(1).stderr = []string{straddle(maxTailLineBytes)}
		}, func(t *testing.T, _ runner.Result, err error) {
			t.Helper()
			mustNotLeak(t, err.Error())
		}},
		{"exit_error message", func(te *testEnv) {
			te.sup.exited(1).stderr = []string{`{"message_type":"exit_error","code":1,"message":"` + straddle(maxFailureDetail) + `"}`}
		}, func(t *testing.T, _ runner.Result, err error) {
			t.Helper()
			mustNotLeak(t, err.Error())
		}},
		{"item error", func(te *testEnv) {
			te.sup.exited(3)
			te.sup.stdout = fixture(t, "backup_warn.out")[:1]
			te.sup.stderr = []string{`{"message_type":"error","error":{"message":"` + straddle(maxTailLineBytes-len("/data/f: ")) + `"},"during":"archival","item":"/data/f"}`}
		}, func(t *testing.T, res runner.Result, err error) {
			t.Helper()
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			for _, w := range res.Warnings {
				mustNotLeak(t, w)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			tc.setup(te)
			res, _, err := te.run(backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{te.local("src")}}))
			if err == nil && tc.name != "item error" {
				t.Fatal("expected an error")
			}
			tc.check(t, res, err)
		})
	}
}

// mustNotLeak fails if text holds a secret or a recognizable piece of one.
func mustNotLeak(t *testing.T, text string) {
	t.Helper()
	for _, secret := range []string{testPassword, testSecret} {
		if strings.Contains(text, secret[:6]) {
			t.Errorf("text leaks (part of) a secret: %q", text)
		}
	}
}

type leakyPolicy struct{}

func (leakyPolicy) Resolve(string) (string, error) {
	return "", errors.New("denied for password " + testPassword)
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
