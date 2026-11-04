//go:build unix

package proc

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"syscall"
	"testing"
	"time"
)

func TestRunGracefulStop(t *testing.T) {
	t.Parallel()
	ctx, spec, lines := cancelOnReady(t, "trap", context.Canceled)

	start := time.Now()
	res, err := New(Options{GracePeriod: 10 * time.Second}).Run(ctx, spec)
	elapsed := time.Since(start)

	if _, ok := errors.AsType[*CanceledError](err); !ok {
		t.Fatalf("Run() error = %v, want *CanceledError", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3 (the child's own exit after handling SIGINT)", res.ExitCode)
	}
	if !slices.Equal(*lines, []string{"ready", "stopping"}) {
		t.Errorf("stdout = %q; output after the stop request must still be delivered", *lines)
	}
	if elapsed > 5*time.Second {
		t.Errorf("graceful stop took %v", elapsed)
	}
}

func TestRunKillsAfterGracePeriod(t *testing.T) {
	t.Parallel()
	ctx, spec, _ := cancelOnReady(t, "ignore", context.Canceled)
	const grace = 300 * time.Millisecond
	spec.GracePeriod = grace

	start := time.Now()
	res, err := New(Options{}).Run(ctx, spec)
	elapsed := time.Since(start)

	if _, ok := errors.AsType[*CanceledError](err); !ok {
		t.Fatalf("Run() error = %v, want *CanceledError", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 (killed by signal)", res.ExitCode)
	}
	if elapsed < grace || elapsed > grace+5*time.Second {
		t.Errorf("Run took %v, want just over the %v grace period", elapsed, grace)
	}
}

func TestRunChildHasOwnProcessGroup(t *testing.T) {
	t.Parallel()
	spec := helperSpec(t, "pgid")
	var pid, pgid int
	spec.Stdout = func(l Line) { _, _ = fmt.Sscanf(string(l.Data), "%d %d", &pid, &pgid) }

	if _, err := New(Options{}).Run(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if pid == 0 || pgid != pid {
		t.Errorf("child pid %d, pgid %d; want the child to lead its own group", pid, pgid)
	}
	if pgid == syscall.Getpgrp() {
		t.Error("child shares the worker's process group")
	}
}

func TestRunPanicKillsChild(t *testing.T) {
	t.Parallel()
	spec := helperSpec(t, "pid")
	var pid int
	spec.Stdout = func(l Line) {
		_, _ = fmt.Sscanf(string(l.Data), "pid=%d", &pid)
		panic("boom")
	}

	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Errorf("recovered %v, want boom", r)
			}
		}()
		_, _ = New(Options{}).Run(t.Context(), spec)
	}()

	if pid == 0 {
		t.Fatal("child PID not received")
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("child %d still running after the callback panicked", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
