//go:build windows

package mount

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/windows/registry"
)

// windowsChecks are the static checks of Detect on Windows: WinFsp is
// installed, as rclone (through cgofuse) finds it: the InstallDir value of
// its registry key, read as a 32-bit program reads it (WOW6432Node on a
// 64-bit Windows, where WinFsp's installer writes it), and its DLL for
// this architecture in the bin folder there. Each function is a seam for
// tests.
type windowsChecks struct {
	// installDir returns WinFsp's InstallDir.
	installDir func() (string, error)
	// stat stats a file (the DLL).
	stat func(path string) (fs.FileInfo, error)
	// arch is runtime.GOARCH.
	arch string
}

// reasonNoWinFsp is the reason when WinFsp is missing.
const reasonNoWinFsp = "WinFsp is not installed"

func osChecks() windowsChecks {
	return windowsChecks{installDir: winFspInstallDir, stat: os.Stat, arch: runtime.GOARCH}
}

// winFspInstallDir reads WinFsp's InstallDir from the registry.
func winFspInstallDir() (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\WinFsp`, registry.QUERY_VALUE|registry.WOW64_32KEY)
	if err != nil {
		return "", err
	}
	defer func() { _ = k.Close() }()
	dir, _, err := k.GetStringValue("InstallDir")
	return dir, err
}

// dllName is the WinFsp DLL rclone loads on arch.
func dllName(arch string) string {
	switch arch {
	case "arm64":
		return "winfsp-a64.dll"
	case "386":
		return "winfsp-x86.dll"
	}
	return "winfsp-x64.dll"
}

func (c windowsChecks) check() string {
	dir, err := c.installDir()
	if errors.Is(err, registry.ErrNotExist) || errors.Is(err, fs.ErrNotExist) || (err == nil && dir == "") {
		return reasonNoWinFsp
	}
	if err != nil {
		return fmt.Sprintf("WinFsp's installation cannot be read from the registry: %v", err)
	}
	dll := filepath.Join(dir, "bin", dllName(c.arch))
	if _, err := c.stat(dll); err != nil {
		return fmt.Sprintf("WinFsp is not installed completely: %s is missing", dll)
	}
	return ""
}
