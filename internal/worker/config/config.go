// Package config defines the worker's configuration file and environment
// variables. Milestone 2 adds the link settings (mode, controller URL,
// listen address, credentials).
//
// Paths are validated for the host the worker runs on: they must be
// absolute there (a Windows worker needs "C:\..." paths, a Linux worker
// "/..." paths).
//
// Allowed dependencies: the standard library and internal/platform packages.
package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Zeptiny/Syncr/internal/platform/logging"
)

// Config is the worker configuration.
type Config struct {
	Log logging.Config `yaml:"log"`

	// DataDir is the worker's data directory: job scratch space, the restic
	// cache and the managed tools. Empty selects the platform default (see
	// internal/worker/paths).
	DataDir string `yaml:"data_dir" env:"SYNCR_DATA_DIR"`

	// LocalRoots are the directories jobs may read and write on this
	// machine: backup sources, restore targets, local repositories and local
	// transfer endpoints. An empty list denies all local access, so a worker
	// that only moves data between remotes needs none. Environment form:
	// comma-separated.
	LocalRoots []string `yaml:"local_roots" env:"SYNCR_LOCAL_ROOTS"`

	// Tools says where rclone and restic come from.
	Tools ToolsConfig `yaml:"tools"`
}

// ToolsConfig configures provisioning of the pinned rclone and restic
// binaries (see internal/worker/tools). The resolution order is: explicit
// path, binary next to the worker executable, managed install in Dir, PATH.
// Whatever is found must report exactly the pinned version.
type ToolsConfig struct {
	// RclonePath and ResticPath are explicit binaries, tried first. A path
	// that is set but unusable is an error rather than skipped.
	RclonePath string `yaml:"rclone_path" env:"SYNCR_RCLONE_PATH"`
	ResticPath string `yaml:"restic_path" env:"SYNCR_RESTIC_PATH"`

	// Dir is the managed tools directory. Empty means <data_dir>/tools.
	// "syncr-worker tools fetch" installs into it.
	Dir string `yaml:"dir" env:"SYNCR_TOOLS_DIR"`

	// Download lets the worker fetch a missing pinned tool into Dir at
	// startup (checksum-verified). Off by default: a running worker does not
	// reach out to the internet unless the operator allows it.
	Download bool `yaml:"download" env:"SYNCR_TOOLS_DOWNLOAD"`
}

// Default returns the configuration used when nothing is overridden.
func Default() Config {
	return Config{Log: logging.DefaultConfig()}
}

// Validate reports every invalid setting.
func (c *Config) Validate() error {
	errs := []error{c.Log.Validate()}
	errs = append(errs, checkAbs("data_dir", c.DataDir, true))
	errs = append(errs, checkAbs("tools.dir", c.Tools.Dir, true))
	errs = append(errs, checkAbs("tools.rclone_path", c.Tools.RclonePath, true))
	errs = append(errs, checkAbs("tools.restic_path", c.Tools.ResticPath, true))
	seen := make(map[string]bool, len(c.LocalRoots))
	for i, root := range c.LocalRoots {
		field := fmt.Sprintf("local_roots[%d]", i)
		if err := checkAbs(field, root, false); err != nil {
			errs = append(errs, err)
			continue
		}
		key := filepath.Clean(root)
		if seen[key] {
			errs = append(errs, fmt.Errorf("%s: %q is listed more than once", field, root))
		}
		seen[key] = true
	}
	return errors.Join(errs...)
}

// checkAbs requires value to be an absolute path of this host. An empty value
// is valid when optional.
func checkAbs(field, value string, optional bool) error {
	switch {
	case value == "" && optional:
		return nil
	case value == "":
		return fmt.Errorf("%s: path is empty", field)
	case strings.ContainsRune(value, 0):
		return fmt.Errorf("%s: path contains a NUL byte", field)
	case !filepath.IsAbs(value):
		return fmt.Errorf("%s: %q is not an absolute path", field, value)
	}
	return nil
}
