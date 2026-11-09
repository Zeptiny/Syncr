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
func (*lifetime) startChild(cmd *exec.Cmd, _ *slog.Logger) (<-chan error, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return done, nil
}
