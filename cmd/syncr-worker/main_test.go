package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/platform/version"
)

func testEnv(vars map[string]string) (*env, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	return &env{
		stdout:    &stdout,
		stderr:    &stderr,
		lookupEnv: func(k string) (string, bool) { v, ok := vars[k]; return v, ok },
		info:      version.Info{Version: "v2.0.0-test", Commit: "abc", GoVersion: "go1.27"},
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
		{name: "setup", args: []string{"setup"}, wantCode: exitError, wantStderr: "syncr-worker setup: not implemented yet"},
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

func TestRunBlocksUntilCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	e, _, stderr := testEnv(map[string]string{"SYNCR_LOG_FORMAT": "json"})

	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"run"}, e) }()

	select {
	case code := <-done:
		t.Fatalf("run returned %d before cancellation", code)
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
	if !strings.Contains(stderr.String(), `"msg":"worker stopped"`) {
		t.Errorf("logs: %s", stderr)
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
