package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// commandRunner runs an external program and returns its combined output and
// exit code. A program that ran and failed is reported through the exit code
// with a nil error; err is only set when it could not run at all.
type commandRunner interface {
	Run(ctx context.Context, name string, args ...string) (output string, exitCode int, err error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // callers pass fixed program names with arguments built from a validated Spec
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	text := strings.TrimSpace(out.String())
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return text, exitErr.ExitCode(), nil
	}
	if err != nil {
		return text, -1, fmt.Errorf("run %s: %w", name, err)
	}
	return text, 0, nil
}
