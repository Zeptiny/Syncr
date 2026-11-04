package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"

	"github.com/Zeptiny/Syncr/internal/worker/paths"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/tools"
)

// Environment variables read by the tools commands.
const (
	toolsDirEnv = "SYNCR_TOOLS_DIR"
	// A tool's explicit binary path is SYNCR_<TOOL>_PATH, e.g. SYNCR_RCLONE_PATH.
)

// toolsOverrides lets tests replace the manifest and the network; the zero
// value means the pinned tools and the real network.
type toolsOverrides struct {
	manifest   *tools.Manifest
	httpClient *http.Client
	baseURL    string
}

func (o toolsOverrides) pinned() tools.Manifest {
	if o.manifest != nil {
		return *o.manifest
	}
	return tools.DefaultManifest()
}

// toolsCmd dispatches `tools fetch` and `tools status`.
func toolsCmd(ctx context.Context, e *env, args []string) error {
	subs := []struct {
		name, summary string
		run           func(context.Context, *env, []string) error
	}{
		{"fetch", "download and verify the pinned rclone and restic", toolsFetchCmd},
		{"status", "show what each provider resolves for every tool", toolsStatusCmd},
	}
	if len(args) == 0 || isHelp(args[0]) {
		fmt.Fprintf(e.stderr, "Usage: %s tools <fetch|status> [flags]\n\n", name)
		for _, s := range subs {
			fmt.Fprintf(e.stderr, "  %-8s %s\n", s.name, s.summary)
		}
		fmt.Fprintf(e.stderr, "\nRun '%s tools <subcommand> -h' for flags.\n", name)
		if len(args) == 0 {
			return errUsage
		}
		return nil
	}
	for _, s := range subs {
		if s.name == args[0] {
			return s.run(ctx, e, args[1:])
		}
	}
	fmt.Fprintf(e.stderr, "unknown tools subcommand %q\n", args[0])
	return errUsage
}

// toolsDir resolves the tools directory: the flag, else $SYNCR_TOOLS_DIR,
// else <worker data dir>/tools.
func toolsDir(e *env, flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if v, ok := e.lookupEnv(toolsDirEnv); ok && v != "" {
		return v, nil
	}
	pe := paths.Env{GOOS: runtime.GOOS, LookupEnv: e.lookupEnv, UserHomeDir: os.UserHomeDir, Geteuid: os.Geteuid}
	data, err := paths.DataDir(paths.DefaultMode(pe), pe)
	if err != nil {
		return "", fmt.Errorf("find the worker data directory (use --dir): %w", err)
	}
	return paths.ToolsDir(data), nil
}

// toolsFetchCmd installs every pinned tool into the tools directory.
func toolsFetchCmd(ctx context.Context, e *env, args []string) error {
	fs := newFlagSet(e, "tools fetch")
	dirFlag := fs.String("dir", "", "tools directory (default $"+toolsDirEnv+", else <worker data dir>/tools)")
	osFlag := fs.String("os", runtime.GOOS, "target operating system (GOOS)")
	archFlag := fs.String("arch", runtime.GOARCH, "target architecture (GOARCH)")
	if helped, err := parseFlags(fs, args); helped || err != nil {
		return err
	}
	dir, err := toolsDir(e, *dirFlag)
	if err != nil {
		return err
	}
	platform := tools.Platform{OS: *osFlag, Arch: *archFlag}
	manifest := e.tools.pinned()
	// Fail before downloading anything if some tool has no release.
	for _, spec := range manifest.Tools {
		if _, err := spec.Artifact(platform); err != nil {
			return err
		}
	}

	inst, err := tools.NewInstaller(tools.InstallerOptions{
		Dir:        dir,
		HTTPClient: e.tools.httpClient,
		BaseURL:    e.tools.baseURL,
		Detector:   tools.NewProcDetector(proc.New(proc.Options{})),
	})
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
	for _, spec := range manifest.Tools {
		got, err := inst.Install(ctx, spec, platform)
		if err != nil {
			_ = tw.Flush()
			return fmt.Errorf("%s: %w", spec.Name, err)
		}
		state := "installed"
		if got.Reused {
			state = "up-to-date"
		}
		// The path is last so scripts can take the final field.
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", got.Tool, got.Version, got.Platform, state, got.Path)
	}
	return tw.Flush()
}

// toolsStatusCmd shows what each provider resolves, without downloading.
func toolsStatusCmd(ctx context.Context, e *env, args []string) error {
	fs := newFlagSet(e, "tools status")
	dirFlag := fs.String("dir", "", "tools directory (default $"+toolsDirEnv+", else <worker data dir>/tools)")
	if helped, err := parseFlags(fs, args); helped || err != nil {
		return err
	}
	dir, err := toolsDir(e, *dirFlag)
	if err != nil {
		return err
	}
	manifest := e.tools.pinned()

	configured := make(map[string]string)
	for _, spec := range manifest.Tools {
		if v, ok := e.lookupEnv("SYNCR_" + strings.ToUpper(spec.Name) + "_PATH"); ok {
			configured[spec.Name] = v
		}
	}
	resolver, err := tools.New(tools.Config{
		Manifest:   manifest,
		Configured: configured,
		BundledDir: executableDir(),
		ToolsDir:   dir,
		Download:   false,
		Runner:     proc.New(proc.Options{}),
		HTTPClient: e.tools.httpClient,
		BaseURL:    e.tools.baseURL,
	})
	if err != nil {
		return err
	}

	var missing []string
	for _, spec := range manifest.Tools {
		attempts, err := resolver.Trace(ctx, spec.Name)
		if err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "%s (pinned %s)\n", spec.Name, spec.Version)
		var used *tools.Attempt
		for i, a := range attempts {
			writeAttempt(e.stdout, a)
			if a.Err == nil && used == nil {
				used = &attempts[i]
			}
		}
		if used == nil {
			fmt.Fprintf(e.stdout, "  => none; run '%s tools fetch'\n\n", name)
			missing = append(missing, spec.Name)
			continue
		}
		fmt.Fprintf(e.stdout, "  => %s: %s\n\n", used.Source, used.Resolved.Path)
	}
	if len(missing) > 0 {
		return fmt.Errorf("no verified binary for: %s", strings.Join(missing, ", "))
	}
	return nil
}

func writeAttempt(w io.Writer, a tools.Attempt) {
	if a.Err == nil {
		fmt.Fprintf(w, "  %-11s ok      %s (%s)\n", a.Source, a.Resolved.Path, a.Resolved.Version)
		return
	}
	state := "failed"
	if errors.Is(a.Err, tools.ErrNotFound) {
		state = "absent"
	}
	fmt.Fprintf(w, "  %-11s %-7s %v\n", a.Source, state, a.Err)
}

// executableDir returns the directory of the running executable, or "" if it
// cannot be determined (which disables bundled tools).
func executableDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}
