//go:build integration && windows

package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Zeptiny/Syncr/internal/worker/service"
)

// TestIntegrationRealUserTask is TestIntegrationRealService for
// `setup --user` on Windows: the worker becomes a Scheduled Task of the
// current user with a logon trigger (an InteractiveToken task, which runs
// only in a logged-on user's desktop session), started on demand by setup
// and by `service start|restart --user`.
//
// It changes the machine (a task named syncr-worker), so it runs only when
// SYNCR_TEST_USER_TASK=1: CI runs it on the throw-away Windows runner, whose
// job runs as a logged-on user in the console session. It skips in session
// 0 (a Windows service, as on self-hosted runners), where such a task cannot
// start. SYNCR_TEST_WORKER, SYNCR_TEST_RCLONE and SYNCR_TEST_RESTIC are as
// for TestIntegrationRealService. The task is always removed again.
func TestIntegrationRealUserTask(t *testing.T) {
	if os.Getenv("SYNCR_TEST_USER_TASK") != "1" {
		t.Skip("set SYNCR_TEST_USER_TASK=1 to install a real Scheduled Task of the current user")
	}
	var session uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session); err != nil {
		t.Fatalf("find this process's session: %v", err)
	}
	if session == 0 {
		t.Skip("session 0 has no logged-on desktop, so a logon task of this user cannot start here")
	}
	rclone, restic := os.Getenv("SYNCR_TEST_RCLONE"), os.Getenv("SYNCR_TEST_RESTIC")
	if rclone == "" || restic == "" {
		t.Fatal("SYNCR_TEST_RCLONE and SYNCR_TEST_RESTIC must name the pinned tools")
	}
	bin := workerBinary(t)

	base := t.TempDir()
	cfg, data := filepath.Join(base, "worker.yaml"), filepath.Join(base, "data")
	logFile := filepath.Join(data, "logs", "worker.log")
	mgr := service.NewManager()
	spec := service.NewSpec("windows", service.ScopeUser, bin, cfg, data, logFile)
	if st, err := mgr.Status(t.Context(), spec); err != nil || st != service.StateNotInstalled {
		t.Fatalf("a %s task already exists for this user (state %v, err %v); refusing to replace it", spec.Name, st, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = mgr.Uninstall(ctx, spec)
		if st, _ := mgr.Status(ctx, spec); st != service.StateNotInstalled {
			t.Errorf("cleanup: the task is still %v", st)
		}
	})

	run := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = append(os.Environ(), "SYNCR_RCLONE_PATH="+rclone, "SYNCR_RESTIC_PATH="+restic)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		t.Logf("$ syncr-worker %s\n%s", strings.Join(args, " "), out.String())
		return out.String(), err
	}
	mustRun := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("syncr-worker %s: %v", strings.Join(args, " "), err)
		}
		return out
	}
	state := func() service.State {
		t.Helper()
		st, err := mgr.Status(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	waitFor := func(want service.State) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for state() != want {
			if time.Now().After(deadline) {
				t.Fatalf("the task did not reach %v (is %v)", want, state())
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	scope := []string{"--user", "--config", cfg, "--data-dir", data}

	port := freePort(t)
	out := mustRun("setup", "--user", "--passive",
		"--listen", "127.0.0.1:"+port, "--advertise-url", "https://127.0.0.1:"+port,
		"--data-dir", data, "--config", cfg)
	for _, want := range []string{"Setup complete", "Fingerprint:", "Pairing secret:", "service syncr-worker is running",
		"ok    the worker serves the certificate with the printed fingerprint"} {
		if !strings.Contains(out, want) {
			t.Errorf("setup output lacks %q", want)
		}
	}
	if st := state(); st != service.StateRunning {
		t.Fatalf("after setup the task is %v", st)
	}
	// The task's worker hides its console and logs to the file.
	if b, err := os.ReadFile(logFile); err != nil || len(b) == 0 {
		t.Errorf("the task's worker wrote no log to %s: %v", logFile, err)
	}

	mustRun(append([]string{"service", "status"}, scope...)...)
	mustRun(append([]string{"service", "restart"}, scope...)...)
	waitFor(service.StateRunning)

	mustRun(append([]string{"service", "stop"}, scope...)...)
	waitFor(service.StateStopped)
	if _, err := run(append([]string{"service", "status"}, scope...)...); err == nil {
		t.Error("status of a stopped task must exit non-zero")
	}

	mustRun(append([]string{"service", "start"}, scope...)...)
	waitFor(service.StateRunning)

	mustRun(append([]string{"service", "uninstall"}, scope...)...)
	waitFor(service.StateNotInstalled)
	if _, err := os.Stat(cfg); err != nil {
		t.Errorf("uninstall removed the config: %v", err)
	}
}
