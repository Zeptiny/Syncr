package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Zeptiny/Syncr/internal/worker/proc"
)

const (
	// versionTimeout bounds one `<tool> version` run.
	versionTimeout = 30 * time.Second
	// versionMaxOutput bounds how much version output is parsed.
	versionMaxOutput = 8 << 10
)

// Runner runs a child process to completion. *proc.Supervisor implements it.
type Runner interface {
	Run(ctx context.Context, spec proc.Spec) (proc.Result, error)
}

// VersionDetector reports the version of the tool binary at path.
type VersionDetector interface {
	// Detect returns the version, without a leading "v", that the binary at
	// path reports.
	Detect(ctx context.Context, spec Spec, path string) (string, error)
}

// ProcDetector detects versions by running `<tool> version` through a
// Runner.
type ProcDetector struct {
	runner Runner
}

// NewProcDetector returns a detector that runs binaries with runner.
func NewProcDetector(runner Runner) *ProcDetector { return &ProcDetector{runner: runner} }

// Detect implements VersionDetector.
func (d *ProcDetector) Detect(ctx context.Context, spec Spec, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()

	var out strings.Builder
	collect := func(l proc.Line) {
		if out.Len() < versionMaxOutput {
			out.Write(l.Data)
			out.WriteByte('\n')
		}
	}
	_, err := d.runner.Run(ctx, proc.Spec{
		Path:   path,
		Args:   spec.VersionArgs,
		Stdout: collect,
		Stderr: collect,
	})
	if err != nil {
		return "", fmt.Errorf("tools: run %s %s: %w", path, strings.Join(spec.VersionArgs, " "), err)
	}
	return ParseVersion(spec, out.String())
}

// ParseVersion extracts the version from the output of the tool's version
// command using spec.VersionPattern.
func ParseVersion(spec Spec, output string) (string, error) {
	re, err := compileVersionPattern(spec.VersionPattern)
	if err != nil {
		return "", err
	}
	m := re.FindStringSubmatch(output)
	if len(m) != 2 || m[1] == "" {
		return "", fmt.Errorf("tools: cannot find a %s version in output %q", spec.Name, firstLine(output))
	}
	return strings.TrimPrefix(m[1], "v"), nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if len(line) > 120 {
		line = line[:120] + "..."
	}
	return line
}

// checkVersion runs det on path and requires the pinned version.
func checkVersion(ctx context.Context, det VersionDetector, spec Spec, path string) (string, error) {
	got, err := det.Detect(ctx, spec, path)
	if err != nil {
		return "", err
	}
	if got != spec.Version {
		return got, &VersionMismatchError{Tool: spec.Name, Path: path, Got: got, Want: spec.Version}
	}
	return got, nil
}

// isContextErr reports whether err means the caller's context ended.
func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
