//go:build !linux && !windows

package proc

import (
	"log/slog"
	"os/exec"
)

// lifetime ties children to the worker's lifetime; see startChild.
type lifetime struct{}

// startChild starts cmd and returns a channel that receives the result of
// cmd.Wait. These systems have no parent death signal, so a child keeps
// running if the worker is killed.
func (*lifetime) startChild(cmd *exec.Cmd, t *tree, log *slog.Logger) (<-chan error, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- t.wait(cmd, log) }()
	return done, nil
}

// tree is the process tree of one run. These systems offer no way to wait
// for the child's exit without reaping it that works with os/exec (waitid
// reports stopped processes on macOS as exited), so only the child is
// stopped: the processes it started may outlive the run.
type tree struct{}

func newTree() *tree { return &tree{} }

// wait waits for the child (cmd.Wait).
func (*tree) wait(cmd *exec.Cmd, _ *slog.Logger) error { return cmd.Wait() }
