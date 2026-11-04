//go:build integration

package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/worker/service"
)

// TestIntegrationRealService installs, starts, checks, stops and removes the
// real worker service of this operating system, through the built binary:
//
//	syncr-worker setup --passive ...   (config, certificate, service, self-check)
//	syncr-worker service status|restart|stop|uninstall
//
// It changes the machine (a systemd unit, or a Windows Service), so it runs
// only when SYNCR_TEST_SERVICE=1 and needs root (Linux) or an elevated
// terminal (Windows): CI runs it on throw-away runners. SYNCR_TEST_WORKER is
// the path of a built syncr-worker (default: it builds one); SYNCR_TEST_RCLONE
// and SYNCR_TEST_RESTIC are the pinned tools, as for the other integration
// tests. The service is always removed again, even when the test fails.
func TestIntegrationRealService(t *testing.T) {
	if os.Getenv("SYNCR_TEST_SERVICE") != "1" {
		t.Skip("set SYNCR_TEST_SERVICE=1 to install a real service (needs root or Administrator)")
	}
	if !service.Elevated() {
		t.Fatal("SYNCR_TEST_SERVICE=1 needs root (Linux) or an elevated terminal (Windows)")
	}
	rclone, restic := os.Getenv("SYNCR_TEST_RCLONE"), os.Getenv("SYNCR_TEST_RESTIC")
	if rclone == "" || restic == "" {
		t.Fatal("SYNCR_TEST_RCLONE and SYNCR_TEST_RESTIC must name the pinned tools")
	}
	bin := workerBinary(t)

	// Never touch a worker service that is really in use.
	mgr := service.NewManager()
	spec := service.NewSpec(runtime.GOOS, service.ScopeSystem, bin, "unused", "unused", "")
	if st, err := mgr.Status(t.Context(), spec); err != nil || st != service.StateNotInstalled {
		t.Fatalf("a %s service already exists on this machine (state %v, err %v); refusing to replace it", spec.Name, st, err)
	}

	base := t.TempDir()
	cfg, data := filepath.Join(base, "worker.yaml"), filepath.Join(base, "data")
	t.Cleanup(func() {
		// Whatever happened, leave no service behind.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = mgr.Uninstall(ctx, spec)
		if st, _ := mgr.Status(ctx, spec); st != service.StateNotInstalled {
			t.Errorf("cleanup: the service is still %v", st)
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
				t.Fatalf("service did not reach %v (is %v)", want, state())
			}
			time.Sleep(250 * time.Millisecond)
		}
	}

	port := freePort(t)
	out := mustRun("setup", "--passive",
		"--listen", "127.0.0.1:"+port, "--advertise-url", "https://127.0.0.1:"+port,
		"--data-dir", data, "--config", cfg)
	for _, want := range []string{"Setup complete", "Fingerprint:", "Pairing secret:", "service syncr-worker is running"} {
		if !strings.Contains(out, want) {
			t.Errorf("setup output lacks %q", want)
		}
	}
	if st := state(); st != service.StateRunning {
		t.Fatalf("after setup the service is %v", st)
	}
	if runtime.GOOS == "linux" {
		unit, err := os.ReadFile("/etc/systemd/system/syncr-worker.service")
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"Restart=on-failure", "RestartSec=5", "NoNewPrivileges=yes"} {
			if !strings.Contains(string(unit), want) {
				t.Errorf("unit lacks %q:\n%s", want, unit)
			}
		}
	}

	// A second setup is refused, and does not disturb the running service.
	if out, err := run("setup", "--passive", "--listen", "127.0.0.1:"+port, "--advertise-url", "https://127.0.0.1:"+port, "--data-dir", data, "--config", cfg); err == nil || !strings.Contains(out, "already set up") {
		t.Errorf("second setup: err %v\n%s", err, out)
	}
	if st := state(); st != service.StateRunning {
		t.Fatalf("after the refused setup the service is %v", st)
	}

	mustRun("service", "status", "--config", cfg, "--data-dir", data)
	mustRun("service", "restart", "--config", cfg, "--data-dir", data)
	waitFor(service.StateRunning)

	mustRun("service", "stop", "--config", cfg, "--data-dir", data)
	waitFor(service.StateStopped)
	if _, err := run("service", "status", "--config", cfg, "--data-dir", data); err == nil {
		t.Error("status of a stopped service must exit non-zero")
	}

	mustRun("service", "start", "--config", cfg, "--data-dir", data)
	waitFor(service.StateRunning)

	mustRun("service", "uninstall", "--config", cfg, "--data-dir", data)
	waitFor(service.StateNotInstalled)
	if _, err := os.Stat(cfg); err != nil {
		t.Errorf("uninstall removed the config: %v", err)
	}
}

// workerBinary returns SYNCR_TEST_WORKER or builds the worker into a
// temporary directory.
func workerBinary(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("SYNCR_TEST_WORKER"); p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			t.Fatal(err)
		}
		return abs
	}
	name := "syncr-worker"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", out, ".")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port) //nolint:errcheck,forcetypeassert // a TCP listener
}
