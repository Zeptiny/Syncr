// Package config defines the worker's configuration file and environment
// variables: logging, directories, tools, job settings (concurrency,
// progress interval), the hooks jobs may run (see HookConfig) and the link
// to the controller (see LinkConfig).
//
// Paths are validated for the host the worker runs on: they must be
// absolute there (a Windows worker needs "C:\..." paths, a Linux worker
// "/..." paths).
//
// Allowed dependencies: the standard library, internal/link (the names and
// limits of hooks, which the controller checks too) and internal/platform
// packages.
package config

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/link"
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

	// MaxConcurrentJobs is how many jobs the worker runs at once; further
	// jobs wait in a local queue, in order.
	MaxConcurrentJobs int `yaml:"max_concurrent_jobs" env:"SYNCR_MAX_CONCURRENT_JOBS"`

	// ProgressInterval is the minimum time between two progress reports of
	// one job (the last sample is always reported).
	ProgressInterval time.Duration `yaml:"progress_interval" env:"SYNCR_PROGRESS_INTERVAL"`

	// Hooks are the commands jobs may run on this machine around their
	// operation, by name (see HookConfig). The controller only ever names a
	// hook; what runs is what this file says. Empty means jobs can run no
	// hooks here. They have no environment form.
	Hooks map[string]HookConfig `yaml:"hooks"`

	// Link is how the worker talks to the controller. `syncr-worker setup`
	// fills it in; an empty link means the worker is not enrolled.
	Link LinkConfig `yaml:"link"`
}

// HookConfig is one hook: a command of this machine that a job may run
// before or after its operation by naming it (docs/hooks.md). Its key in
// Config.Hooks is its name: 1 to 63 lower-case letters, digits, ".", "_"
// and "-", starting with a letter or digit.
//
// Every user who may run jobs on this worker may run every hook, with the
// worker's rights, but can change nothing about it: no argument and no
// environment value comes from the controller.
type HookConfig struct {
	// Description says what the hook does, in one line of at most 200
	// characters. The controller shows it to users who choose hooks.
	Description string `yaml:"description,omitempty"`

	// Command is the program and its arguments. It is started directly,
	// without a shell: wrap anything that needs one in a script. The
	// program is an absolute path, or a bare name found in the worker's
	// PATH.
	Command []string `yaml:"command"`

	// Timeout is how long one run may take before the worker stops it:
	// MinHookTimeout to MaxHookTimeout. Zero means DefaultHookTimeout.
	Timeout time.Duration `yaml:"timeout,omitempty"`

	// Env adds variables to the command's environment, which otherwise
	// holds only what the worker passes to its tools (PATH, HOME, the
	// locale, proxy settings) and the SYNCR_* variables that describe the
	// job. Names starting with SYNCR_ are reserved for those.
	Env map[string]string `yaml:"env,omitempty"`
}

// EffectiveTimeout is the timeout of one run: Timeout, or
// DefaultHookTimeout when it is zero.
func (h HookConfig) EffectiveTimeout() time.Duration {
	if h.Timeout == 0 {
		return DefaultHookTimeout
	}
	return h.Timeout
}

// Defaults and bounds of a hook's timeout.
const (
	DefaultHookTimeout = 10 * time.Minute
	MinHookTimeout     = time.Second
	MaxHookTimeout     = 24 * time.Hour
)

// hookEnvReserved is the prefix of the environment variables the worker
// sets for a hook; a hook's own env may not use it.
const hookEnvReserved = "SYNCR_"

// Defaults and bounds of the job settings.
const (
	DefaultMaxConcurrentJobs = 2
	MaxMaxConcurrentJobs     = 64
	DefaultProgressInterval  = time.Second
	MinProgressInterval      = 100 * time.Millisecond
	MaxProgressInterval      = time.Minute
)

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
	return Config{
		Log:               logging.DefaultConfig(),
		MaxConcurrentJobs: DefaultMaxConcurrentJobs,
		ProgressInterval:  DefaultProgressInterval,
	}
}

// Validate reports every invalid setting.
func (c *Config) Validate() error {
	errs := []error{c.Log.Validate()}
	errs = append(errs, checkAbs("data_dir", c.DataDir, true))
	errs = append(errs, checkAbs("tools.dir", c.Tools.Dir, true))
	errs = append(errs, checkAbs("tools.rclone_path", c.Tools.RclonePath, true))
	errs = append(errs, checkAbs("tools.restic_path", c.Tools.ResticPath, true))
	if c.MaxConcurrentJobs < 1 || c.MaxConcurrentJobs > MaxMaxConcurrentJobs {
		errs = append(errs, fmt.Errorf("max_concurrent_jobs: %d is outside 1..%d", c.MaxConcurrentJobs, MaxMaxConcurrentJobs))
	}
	if c.ProgressInterval < MinProgressInterval || c.ProgressInterval > MaxProgressInterval {
		errs = append(errs, fmt.Errorf("progress_interval: %s is outside %s..%s", c.ProgressInterval, MinProgressInterval, MaxProgressInterval))
	}
	errs = append(errs, c.Link.Validate())
	errs = append(errs, validateHooks(c.Hooks))
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

// validateHooks reports every invalid hook, in name order.
func validateHooks(hooks map[string]HookConfig) error {
	var errs []error
	if len(hooks) > link.MaxWorkerHooks {
		errs = append(errs, fmt.Errorf("hooks: %d hooks are defined, at most %d are allowed", len(hooks), link.MaxWorkerHooks))
	}
	for _, name := range slices.Sorted(maps.Keys(hooks)) {
		field := fmt.Sprintf("hooks.%s", name)
		if !link.ValidHookName(name) {
			errs = append(errs, fmt.Errorf("hooks: the name %q is not valid: use 1 to %d lower-case letters, digits, '.', '_' and '-', starting with a letter or digit",
				name, link.MaxHookNameLen))
			continue
		}
		errs = append(errs, hooks[name].validate(field))
	}
	return errors.Join(errs...)
}

// validate reports every invalid setting of h, which is the hook at field.
// Messages never quote env values: they may be secrets.
func (h HookConfig) validate(field string) error {
	var errs []error
	switch {
	case len(h.Command) == 0:
		errs = append(errs, fmt.Errorf("%s.command: is required: the program and its arguments, as a list", field))
	case strings.TrimSpace(h.Command[0]) == "":
		errs = append(errs, fmt.Errorf("%s.command: the program (the first entry) is empty", field))
	case !filepath.IsAbs(h.Command[0]) && filepath.Base(h.Command[0]) != h.Command[0]:
		errs = append(errs, fmt.Errorf("%s.command: %q must be an absolute path, or a bare program name that is found in PATH", field, h.Command[0]))
	}
	for i, arg := range h.Command {
		if strings.ContainsRune(arg, 0) {
			errs = append(errs, fmt.Errorf("%s.command[%d]: contains a NUL byte", field, i))
		}
	}
	if h.Timeout != 0 && (h.Timeout < MinHookTimeout || h.Timeout > MaxHookTimeout) {
		errs = append(errs, fmt.Errorf("%s.timeout: %s is outside %s..%s", field, h.Timeout, MinHookTimeout, MaxHookTimeout))
	}
	switch {
	case !utf8.ValidString(h.Description):
		errs = append(errs, fmt.Errorf("%s.description: is not valid UTF-8", field))
	case utf8.RuneCountInString(h.Description) > link.MaxHookDescriptionLen:
		errs = append(errs, fmt.Errorf("%s.description: is longer than %d characters", field, link.MaxHookDescriptionLen))
	case strings.ContainsFunc(h.Description, unicode.IsControl):
		errs = append(errs, fmt.Errorf("%s.description: must be one line without control characters", field))
	}
	for _, name := range slices.Sorted(maps.Keys(h.Env)) {
		switch {
		case name == "" || strings.ContainsAny(name, "=\x00"):
			errs = append(errs, fmt.Errorf("%s.env: the variable name %q is empty or contains '=' or NUL", field, name))
		case strings.HasPrefix(strings.ToUpper(name), hookEnvReserved):
			errs = append(errs, fmt.Errorf("%s.env: %s: names starting with %s are reserved for the variables the worker sets", field, name, hookEnvReserved))
		case strings.ContainsRune(h.Env[name], 0):
			errs = append(errs, fmt.Errorf("%s.env: %s: the value contains a NUL byte", field, name))
		}
	}
	return errors.Join(errs...)
}
