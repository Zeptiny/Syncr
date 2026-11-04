// Package paths resolves the worker's default data directory.
//
// Defaults:
//
//	Linux, system   /var/lib/syncr
//	Linux, user     $XDG_DATA_HOME/syncr, else ~/.local/share/syncr
//	Windows, system %ProgramData%\Syncr
//	Windows, user   %LocalAppData%\Syncr
//	macOS, user     ~/Library/Application Support/syncr (development only)
//
// Allowed dependencies: the standard library.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Mode selects between the machine-wide and the per-user location.
type Mode int

// The modes.
const (
	// ModeSystem is for a worker running as a system service.
	ModeSystem Mode = iota
	// ModeUser is for a worker running as an ordinary user, e.g. on a
	// desktop without admin rights.
	ModeUser
)

// String implements fmt.Stringer.
func (m Mode) String() string {
	if m == ModeUser {
		return "user"
	}
	return "system"
}

// Env abstracts the process environment so tests can supply fakes.
type Env struct {
	// GOOS is the operating system, as runtime.GOOS.
	GOOS string
	// LookupEnv has the signature of os.LookupEnv.
	LookupEnv func(key string) (string, bool)
	// UserHomeDir returns the user's home directory.
	UserHomeDir func() (string, error)
	// Geteuid returns the effective user ID, or -1 where there is none.
	Geteuid func() int
}

// OSEnv returns the real process environment.
func OSEnv() Env {
	return Env{GOOS: runtime.GOOS, LookupEnv: os.LookupEnv, UserHomeDir: os.UserHomeDir, Geteuid: os.Geteuid}
}

// DefaultMode picks ModeSystem for root on Unix and always on Windows (where
// a service installer chooses ModeUser explicitly), and ModeUser for
// unprivileged Unix users.
func DefaultMode(env Env) Mode {
	if env.GOOS != "windows" && env.Geteuid != nil && env.Geteuid() != 0 {
		return ModeUser
	}
	return ModeSystem
}

// DataDir returns the worker's data directory for mode.
func DataDir(mode Mode, env Env) (string, error) {
	switch env.GOOS {
	case "windows":
		return windowsDataDir(mode, env)
	case "darwin":
		if mode == ModeSystem {
			return "/Library/Application Support/syncr", nil
		}
		home, err := env.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("paths: find home directory: %w", err)
		}
		return filepath.Join(home, "Library", "Application Support", "syncr"), nil
	default:
		if mode == ModeSystem {
			return "/var/lib/syncr", nil
		}
		if xdg, ok := env.LookupEnv("XDG_DATA_HOME"); ok && filepath.IsAbs(xdg) {
			return filepath.Join(xdg, "syncr"), nil
		}
		home, err := env.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("paths: find home directory: %w", err)
		}
		return filepath.Join(home, ".local", "share", "syncr"), nil
	}
}

func windowsDataDir(mode Mode, env Env) (string, error) {
	variable := "ProgramData"
	if mode == ModeUser {
		variable = "LocalAppData"
	}
	base, ok := env.LookupEnv(variable)
	if !ok || base == "" {
		return "", fmt.Errorf("paths: %%%s%% is not set", variable)
	}
	return filepath.Join(base, "Syncr"), nil
}

// ToolsDir returns the default managed tools directory under dataDir.
func ToolsDir(dataDir string) string { return filepath.Join(dataDir, "tools") }

// ScratchDir returns the directory under dataDir where jobs get private
// temporary directories (rclone config and cache, restic temp files).
func ScratchDir(dataDir string) string { return filepath.Join(dataDir, "scratch") }

// CacheDir returns the directory under dataDir for tool caches that may be
// kept between jobs, such as restic's repository cache.
func CacheDir(dataDir string) string { return filepath.Join(dataDir, "cache") }
