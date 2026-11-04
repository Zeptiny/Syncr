package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/platform/version"
	workerconfig "github.com/Zeptiny/Syncr/internal/worker/config"
)

func testEnv(vars map[string]string) (*env, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	return &env{
		stdout:    &stdout,
		stderr:    &stderr,
		lookupEnv: func(k string) (string, bool) { v, ok := vars[k]; return v, ok },
		info:      version.Info{Version: "v2.0.0-test", Commit: "abc", GoVersion: "go1.27"},
		host:      interactiveHost{},
	}, &stdout, &stderr
}

func TestDispatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "no args", args: nil, wantCode: exitUsage, wantStderr: "Usage: syncr-worker <command>"},
		{name: "help", args: []string{"help"}, wantCode: exitOK, wantStderr: "setup"},
		{name: "unknown", args: []string{"frobnicate"}, wantCode: exitUsage, wantStderr: `unknown command "frobnicate"`},
		{name: "version", args: []string{"version"}, wantCode: exitOK, wantStdout: "syncr-worker v2.0.0-test (commit abc"},
		{name: "version extra args", args: []string{"version", "x"}, wantCode: exitUsage, wantStderr: "unexpected arguments"},
		{name: "setup without arguments", args: []string{"setup"}, wantCode: exitError, wantStderr: "syncr-worker setup: --controller is required"},
		{name: "service without action", args: []string{"service"}, wantCode: exitUsage, wantStderr: "install"},
		{name: "subcommand help", args: []string{"run", "-h"}, wantCode: exitOK, wantStderr: "-config"},
		{name: "bad flag", args: []string{"run", "-bogus"}, wantCode: exitUsage, wantStderr: "flag provided but not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e, stdout, stderr := testEnv(nil)
			if code := run(t.Context(), tt.args, e); code != tt.wantCode {
				t.Errorf("exit code %d, want %d (stderr: %s)", code, tt.wantCode, stderr)
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout %q does not contain %q", stdout, tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr %q does not contain %q", stderr, tt.wantStderr)
			}
		})
	}
}

// runEnv returns a command environment whose configuration comes only from
// SYNCR_* variables, with fake tools.
func (f *fixture) runEnv(extra map[string]string) (*env, *bytes.Buffer) {
	vars := map[string]string{
		"SYNCR_LOG_FORMAT":  "json",
		"SYNCR_DATA_DIR":    f.cfg.DataDir,
		"SYNCR_LOCAL_ROOTS": strings.Join(f.cfg.LocalRoots, ","),
		"SYNCR_RCLONE_PATH": f.cfg.Tools.RclonePath,
		"SYNCR_RESTIC_PATH": f.cfg.Tools.ResticPath,
	}
	maps.Copy(vars, extra)
	e, _, stderr := testEnv(vars)
	e.tools = f.deps.tools
	return e, stderr
}

// notifyReady makes `run` close the returned channel once the worker is
// ready: it has been built, logged "worker ready" and started its link
// (through e.link, or the real link). A context canceled before then aborts
// startup, which is an error, not a clean stop.
func notifyReady(e *env) <-chan struct{} {
	ready := make(chan struct{})
	start := e.linkStarter()
	var once sync.Once
	e.link = func(ctx context.Context, cfg workerconfig.Config, w *worker, log *slog.Logger) (func(), error) {
		defer once.Do(func() { close(ready) })
		return start(ctx, cfg, w, log)
	}
	return ready
}

// awaitReady waits until ready is closed, failing the test if run returns
// first.
func awaitReady(t *testing.T, ready <-chan struct{}, done <-chan int, stderr fmt.Stringer) {
	t.Helper()
	select {
	case <-ready:
	case code := <-done:
		t.Fatalf("run returned %d before the worker was ready: %s", code, stderr)
	case <-time.After(time.Minute):
		t.Fatal("the worker did not become ready")
	}
}

func TestRunBlocksUntilCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	f := newFixture(t)
	e, stderr := f.runEnv(nil)
	ready := notifyReady(e)

	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"run"}, e) }()

	awaitReady(t, ready, done, stderr)
	select {
	case code := <-done:
		t.Fatalf("run returned %d before cancellation (stderr: %s)", code, stderr)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Errorf("exit code %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop after cancellation")
	}
	for _, want := range []string{`"msg":"worker ready"`, `"job_kinds":["rclone_transfer"`, `"msg":"worker stopped"`} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("logs lack %s:\n%s", want, stderr)
		}
	}
}

func TestRunExitsNonZeroWhenToolsAreMissing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e, stderr := f.runEnv(map[string]string{"SYNCR_RCLONE_PATH": "", "SYNCR_RESTIC_PATH": ""})

	if code := run(t.Context(), []string{"run"}, e); code != exitError {
		t.Fatalf("exit code %d, want %d", code, exitError)
	}
	for _, want := range []string{"syncr-worker run:", "required tools are not available", "tools fetch --dir", "SYNCR_TOOLS_DOWNLOAD"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

func TestRunRejectsInvalidWorkerConfig(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e, stderr := f.runEnv(map[string]string{"SYNCR_LOCAL_ROOTS": "relative", "SYNCR_DATA_DIR": "also-relative"})
	if code := run(t.Context(), []string{"run"}, e); code != exitError {
		t.Fatalf("exit code %d, want %d", code, exitError)
	}
	for _, want := range []string{"local_roots[0]", "data_dir"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q (all problems should be reported):\n%s", want, stderr)
		}
	}
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	e, _, stderr := testEnv(map[string]string{"SYNCR_LOG_FORMAT": "xml"})
	if code := run(t.Context(), []string{"run"}, e); code != exitError {
		t.Errorf("exit code %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr.String(), "format") {
		t.Errorf("stderr: %s", stderr)
	}
}

// interactiveHost is a service.Host for a process not started by a service
// manager, whatever the test runner's parent is.
type interactiveHost struct{}

func (interactiveHost) Managed() bool { return false }

func (interactiveHost) Run(context.Context, string, io.Writer, func(context.Context, io.Writer) error) error {
	panic("interactiveHost.Run must not be called")
}
