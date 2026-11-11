package upgrade

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/tools"
)

// Binary runs the downloaded worker binary before it is installed.
// *ExecBinary is the implementation; tests use a fake.
type Binary interface {
	// Version runs "<path> version" and returns the version it reports
	// (e.g. "v2.1.0").
	Version(ctx context.Context, path string) (string, error)
	// FetchTools runs "<path> tools fetch --dir <toolsDir>", which installs
	// the tool versions that binary pins (verified against the checksums
	// compiled into it), and returns those versions. An empty list means
	// they could not be read from its output; the tools are installed all
	// the same.
	FetchTools(ctx context.Context, path, toolsDir string) ([]tools.ToolVersion, error)
	// CheckTools runs "<path> tools status --dir <toolsDir>" with the
	// worker's configured tool paths (configured, by tool name) and fails
	// unless that binary resolves every tool it pins.
	CheckTools(ctx context.Context, path, toolsDir string, configured map[string]string) error
}

// Supervisor runs a child process; *proc.Supervisor implements it.
type Supervisor interface {
	Run(ctx context.Context, spec proc.Spec) (proc.Result, error)
}

// Timeouts of the commands ExecBinary runs.
const (
	versionTimeout = 30 * time.Second
	fetchTimeout   = 30 * time.Minute // downloads two archives
	statusTimeout  = 2 * time.Minute
)

// maxDetailLines is how many of a failed command's last output lines its
// error quotes.
const maxDetailLines = 6

// ExecBinary runs the binary as a child process through a Supervisor, with
// the supervisor's minimal environment (proxy and CA settings pass, the
// worker's own secrets do not).
type ExecBinary struct {
	sup Supervisor
}

// NewExecBinary returns an ExecBinary that starts processes with sup.
func NewExecBinary(sup Supervisor) *ExecBinary { return &ExecBinary{sup: sup} }

// output runs path with args and env and returns its output lines (stdout
// then stderr, each in order).
func (b *ExecBinary) output(ctx context.Context, timeout time.Duration, path string, env map[string]string, args ...string) (stdout, stderr []string, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err = b.sup.Run(ctx, proc.Spec{
		Path:   path,
		Args:   args,
		Env:    env,
		Stdout: func(l proc.Line) { stdout = append(stdout, string(l.Data)) },
		Stderr: func(l proc.Line) { stderr = append(stderr, string(l.Data)) },
	})
	if err != nil {
		return stdout, stderr, fmt.Errorf("%s %s: %w%s", BinaryName, strings.Join(args, " "), err, detail(stdout, stderr))
	}
	return stdout, stderr, nil
}

// detail quotes the last lines of a command's output for an error message.
func detail(stdout, stderr []string) string {
	lines := append(append([]string(nil), stdout...), stderr...)
	var kept []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			kept = append(kept, strings.TrimSpace(l))
		}
	}
	if len(kept) == 0 {
		return ""
	}
	if len(kept) > maxDetailLines {
		kept = kept[len(kept)-maxDetailLines:]
	}
	return ": " + strings.Join(kept, "; ")
}

// Version implements Binary. The version command prints
// "syncr-worker <version> (commit ..., built ..., go...) <os>/<arch>".
func (b *ExecBinary) Version(ctx context.Context, path string) (string, error) {
	stdout, _, err := b.output(ctx, versionTimeout, path, nil, "version")
	if err != nil {
		return "", err
	}
	for _, l := range stdout {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == BinaryName {
			return f[1], nil
		}
	}
	return "", fmt.Errorf("%s version printed no version: %q", BinaryName, stdout)
}

// FetchTools implements Binary. tools fetch prints one line per tool:
// "<tool> <version> <os>/<arch> <installed|up-to-date> <path>".
func (b *ExecBinary) FetchTools(ctx context.Context, path, toolsDir string) ([]tools.ToolVersion, error) {
	stdout, _, err := b.output(ctx, fetchTimeout, path, nil, "tools", "fetch", "--dir", toolsDir)
	if err != nil {
		return nil, err
	}
	var pins []tools.ToolVersion
	seen := map[string]bool{}
	for _, l := range stdout {
		f := strings.Fields(l)
		if len(f) < 5 || seen[f[0]] || (f[3] != "installed" && f[3] != "up-to-date") {
			continue
		}
		seen[f[0]] = true
		pins = append(pins, tools.ToolVersion{Tool: f[0], Version: f[1]})
	}
	return pins, nil
}

// CheckTools implements Binary. tools status exits non-zero unless every
// pinned tool resolves; the configured paths reach it as SYNCR_<TOOL>_PATH,
// as they would reach the worker.
func (b *ExecBinary) CheckTools(ctx context.Context, path, toolsDir string, configured map[string]string) error {
	env := map[string]string{}
	names := make([]string, 0, len(configured))
	for name := range configured {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if p := configured[name]; p != "" {
			env["SYNCR_"+strings.ToUpper(name)+"_PATH"] = p
		}
	}
	if _, _, err := b.output(ctx, statusTimeout, path, env, "tools", "status", "--dir", toolsDir); err != nil {
		return errors.Join(errors.New("the new version does not find its pinned tools with this worker's configuration"), err)
	}
	return nil
}
