package restic

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
)

// TestWaitDelayAfterSuccess: restic exited 0, but a process it started kept
// the output pipes open past the grace period. The run is judged by
// restic's own output, so the backup succeeds.
func TestWaitDelayAfterSuccess(t *testing.T) {
	t.Parallel()
	waitDelay := fmt.Errorf("proc: restic: %w", exec.ErrWaitDelay)

	t.Run("exit 0", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		te.sup.result = proc.Result{ExitCode: 0}
		te.sup.err = waitDelay
		te.sup.stdout = fixture(t, "backup.out")
		res, _, err := te.run(backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{te.local("src")}}))
		if err != nil {
			t.Fatalf("err = %v, want nil: restic succeeded", err)
		}
		if res.ExitCode != 0 || res.Output.GetResticBackup().GetSnapshotId() == "" {
			t.Errorf("result = %+v, want the snapshot", res)
		}
	})
	t.Run("exit 0 without a summary", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		te.sup.result = proc.Result{ExitCode: 0}
		te.sup.err = waitDelay
		_, _, err := te.run(backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{te.local("src")}}))
		mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL)
	})
	t.Run("killed", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		te.sup.result = proc.Result{ExitCode: -1}
		te.sup.err = waitDelay
		te.sup.stdout = fixture(t, "backup.out")
		_, _, err := te.run(backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{te.local("src")}}))
		mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL)
	})
}

// TestWaitDelayWithRealProcess runs a fake restic that prints a backup
// summary and exits 0 while a background process keeps its stdout open.
func TestWaitDelayWithRealProcess(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the fake restic is a shell script")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "backup.out")
	if err := os.WriteFile(out, []byte(strings.Join(fixture(t, "backup.out"), "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "restic")
	// The background sleep inherits stdout and outlives the script.
	body := "#!/bin/sh\ncat '" + out + "'\nsleep 5 &\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	te := newTestEnv(t, func(c *Config) {
		c.ResticPath = script
		c.Supervisor = proc.New(proc.Options{GracePeriod: 200 * time.Millisecond})
	})
	start := time.Now()
	res, _, err := te.run(backupSpec(localRepo(te.local("repo")), &syncrv1.ResticBackup{Paths: []string{te.local("src")}}))
	if err != nil {
		t.Fatalf("err = %v, want nil: restic exited 0", err)
	}
	if res.Output.GetResticBackup().GetSnapshotId() == "" {
		t.Errorf("output = %v, want the snapshot", res.Output)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("run took %v; the held pipes were not closed after the grace period", elapsed)
	}
}
