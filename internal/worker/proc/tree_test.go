//go:build linux || windows

package proc

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRunStopsProcessTree runs a child that starts a grandchild (which keeps
// the child's stdout open and survives a stop signal) and checks that no
// process of the tree is left once Run returns: after a normal exit, after
// a cancel, after a time limit (the agent cancels the run's context with a
// cause of its own, which takes the same path) and after the child was
// killed at the end of the grace period.
func TestRunStopsProcessTree(t *testing.T) {
	t.Parallel()
	errCancelled := errors.New("job cancelled by the controller")
	errTimeLimit := errors.New("stopped after 1m0s: the job's time limit was reached")
	const longGrace = 10 * time.Second

	tests := []struct {
		name string
		// mode is the child's behavior (runTree).
		mode string
		// cause cancels the run once the tree is ready; nil lets the child
		// exit on its own.
		cause error
		grace time.Duration
		// unixExit is the child's exit code on Unix; on Windows a stopped
		// child is terminated with exit code 1.
		unixExit int
	}{
		{name: "normal exit", mode: "exit", grace: longGrace, unixExit: 0},
		{name: "cancel", mode: "trap", cause: errCancelled, grace: longGrace, unixExit: 3},
		{name: "time limit", mode: "trap", cause: errTimeLimit, grace: longGrace, unixExit: 3},
		{name: "killed after the grace period", mode: "ignore", cause: errCancelled, grace: 300 * time.Millisecond, unixExit: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			spec := helperSpec(t, "tree", tt.mode)
			spec.GracePeriod = tt.grace
			var lines []string
			var grandchild *watched
			spec.Stdout = func(l Line) {
				s := string(l.Data)
				lines = append(lines, s)
				if pid, ok := strings.CutPrefix(s, "grandchild="); ok {
					n, err := strconv.Atoi(pid)
					if err != nil {
						t.Errorf("bad line %q", s)
						return
					}
					grandchild = watch(t, n)
				}
				if s == "ready" && tt.cause != nil {
					cancel(tt.cause)
				}
			}

			res, err := New(Options{}).Run(ctx, spec)

			if tt.cause == nil {
				// nil, not exec.ErrWaitDelay: the pipes the grandchild held
				// were closed by stopping it, not by the grace period.
				if err != nil || res.ExitCode != 0 {
					t.Errorf("Run() = (%+v, %v), want a clean exit", res, err)
				}
			} else {
				if _, ok := errors.AsType[*CanceledError](err); !ok || !errors.Is(err, tt.cause) {
					t.Errorf("Run() error = %v, want a *CanceledError with the cause", err)
				}
				want := tt.unixExit
				if runtime.GOOS == "windows" {
					want = 1
				}
				if res.ExitCode != want {
					t.Errorf("ExitCode = %d, want %d", res.ExitCode, want)
				}
			}
			if grandchild == nil {
				t.Fatalf("no grandchild PID in the output %q", lines)
			}
			if !grandchild.gone(10 * time.Second) {
				t.Errorf("grandchild %d is still running after Run returned", grandchild.pid)
			}
			// The stop request goes to the child alone; the rest of the tree
			// is killed only once the child has exited.
			if slices.Contains(lines, "grandchild interrupted") {
				t.Error("the stop request reached the grandchild")
			}
		})
	}
}
