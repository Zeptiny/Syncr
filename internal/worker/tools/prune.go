package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Zeptiny/Syncr/internal/worker/proc"
)

// ToolVersion names one version of one tool.
type ToolVersion struct {
	Tool    string
	Version string
}

// Pins returns the version each tool of m is pinned to, in m's order.
func (m Manifest) Pins() []ToolVersion {
	pins := make([]ToolVersion, 0, len(m.Tools))
	for _, s := range m.Tools {
		pins = append(pins, ToolVersion{Tool: s.Name, Version: s.Version})
	}
	return pins
}

// Prune removes the managed installs of keep's tools in versions other than
// the one keep names: the directories <dir>/<tool>/<version>, for every
// platform. It runs under the install lock, so it never races with an
// install, and returns the directories it removed.
//
// Only directories of the tools keep names are removed; the lock file,
// other tools, files and symbolic links in the tools directory stay, and
// configured, bundled and PATH binaries live elsewhere and are never
// touched. Callers prune only once the new pins have resolved: setup after
// it provisioned the tools, an upgrade after the upgraded worker stayed up.
// A running worker never prunes, so a worker rolled back to its previous
// version still finds that version's tools.
//
// It tries every directory and returns the failures joined (on Windows a
// tool that still runs cannot be removed); the directories in removed are
// gone either way.
func (i *Installer) Prune(ctx context.Context, keep []ToolVersion) (removed []string, err error) {
	if len(keep) == 0 {
		return nil, errors.New("tools: prune: no versions to keep")
	}
	seen := make(map[string]bool, len(keep))
	for _, k := range keep {
		if !namePattern.MatchString(k.Tool) || seen[k.Tool] {
			return nil, fmt.Errorf("tools: prune: invalid or repeated tool name %q", k.Tool)
		}
		if k.Version == "" || strings.ContainsAny(k.Version, `/\`) || k.Version == "." || k.Version == ".." {
			return nil, fmt.Errorf("tools: prune: invalid version %q of %s", k.Version, k.Tool)
		}
		seen[k.Tool] = true
	}
	release, err := i.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = release() }()

	var errs []error
	for _, k := range keep {
		toolDir := filepath.Join(i.dir, k.Tool)
		entries, err := os.ReadDir(toolDir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("tools: prune %s: %w", k.Tool, err))
			continue
		}
		for _, e := range entries {
			// e.IsDir is false for a symbolic link, which is never followed.
			if !e.IsDir() || e.Name() == k.Version {
				continue
			}
			if err := ctx.Err(); err != nil {
				return removed, errors.Join(append(errs, fmt.Errorf("tools: prune: %w", context.Cause(ctx)))...)
			}
			dir := filepath.Join(toolDir, e.Name())
			if err := proc.RemoveAll(dir); err != nil {
				errs = append(errs, fmt.Errorf("tools: prune %s %s: %w", k.Tool, e.Name(), err))
				continue
			}
			removed = append(removed, dir)
		}
	}
	return removed, errors.Join(errs...)
}
