package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// verifyExecutable checks that path is a regular file reporting the pinned
// version, and returns it as a Resolved (Source and Tool are filled by the
// Resolver).
func verifyExecutable(ctx context.Context, det VersionDetector, spec Spec, path string) (Resolved, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Resolved{}, fmt.Errorf("tools: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Resolved{}, fmt.Errorf("tools: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Resolved{}, fmt.Errorf("tools: %s is not a regular file", abs)
	}
	version, err := checkVersion(ctx, det, spec, abs)
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{Path: abs, Version: version}, nil
}

// ConfiguredProvider resolves tools from explicit, operator-chosen paths. A
// path that is set but unusable is an error, not absence, so a typo is
// reported rather than silently skipped.
type ConfiguredProvider struct {
	paths    map[string]string
	detector VersionDetector
}

// NewConfiguredProvider returns a provider for paths keyed by tool name.
// Empty values are ignored.
func NewConfiguredProvider(paths map[string]string, det VersionDetector) *ConfiguredProvider {
	cp := make(map[string]string, len(paths))
	for k, v := range paths {
		if v != "" {
			cp[k] = v
		}
	}
	return &ConfiguredProvider{paths: cp, detector: det}
}

// Source implements Provider.
func (p *ConfiguredProvider) Source() Source { return SourceConfigured }

// Resolve implements Provider.
func (p *ConfiguredProvider) Resolve(ctx context.Context, spec Spec) (Resolved, error) {
	path, ok := p.paths[spec.Name]
	if !ok {
		return Resolved{}, fmt.Errorf("%w: no path configured for %s", ErrNotFound, spec.Name)
	}
	return verifyExecutable(ctx, p.detector, spec, path)
}

// BundledProvider resolves tools shipped in a directory, normally the one
// holding the worker executable.
type BundledProvider struct {
	dir      string
	goos     string
	detector VersionDetector
}

// NewBundledProvider returns a provider that looks for <dir>/<tool>[.exe].
func NewBundledProvider(dir string, det VersionDetector) *BundledProvider {
	return &BundledProvider{dir: dir, goos: runtime.GOOS, detector: det}
}

// Source implements Provider.
func (p *BundledProvider) Source() Source { return SourceBundled }

// Resolve implements Provider.
func (p *BundledProvider) Resolve(ctx context.Context, spec Spec) (Resolved, error) {
	path := filepath.Join(p.dir, spec.Binary(p.goos))
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return Resolved{}, fmt.Errorf("%w: %s does not exist", ErrNotFound, path)
	}
	return verifyExecutable(ctx, p.detector, spec, path)
}

// PathProvider resolves tools from PATH. A binary is accepted only if it
// reports the pinned version.
type PathProvider struct {
	lookPath func(file string) (string, error)
	detector VersionDetector
}

// NewPathProvider returns a provider that searches PATH with lookPath
// (exec.LookPath if nil).
func NewPathProvider(lookPath func(string) (string, error), det VersionDetector) *PathProvider {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	return &PathProvider{lookPath: lookPath, detector: det}
}

// Source implements Provider.
func (p *PathProvider) Source() Source { return SourcePath }

// Resolve implements Provider.
func (p *PathProvider) Resolve(ctx context.Context, spec Spec) (Resolved, error) {
	path, err := p.lookPath(spec.Name)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return Resolved{}, fmt.Errorf("%w: %s is not on PATH", ErrNotFound, spec.Name)
		}
		// exec.ErrDot and friends: the match is unusable, say so.
		return Resolved{}, fmt.Errorf("tools: look up %s on PATH: %w", spec.Name, err)
	}
	return verifyExecutable(ctx, p.detector, spec, path)
}

// ManagedProvider resolves tools installed in the worker's tools directory,
// installing them on demand when it has an Installer that may download.
type ManagedProvider struct {
	installer *Installer
	platform  Platform
	download  bool
}

// NewManagedProvider returns a provider backed by installer for platform.
// If download is false it only reports existing installs and never touches
// the network.
func NewManagedProvider(installer *Installer, platform Platform, download bool) *ManagedProvider {
	return &ManagedProvider{installer: installer, platform: platform, download: download}
}

// Source implements Provider.
func (p *ManagedProvider) Source() Source { return SourceManaged }

// Resolve implements Provider.
func (p *ManagedProvider) Resolve(ctx context.Context, spec Spec) (Resolved, error) {
	inst, err := p.installer.Installed(ctx, spec, p.platform)
	if err != nil && p.download && (errors.Is(err, ErrNotInstalled) || errors.Is(err, ErrInstallInvalid)) {
		inst, err = p.installer.Install(ctx, spec, p.platform)
	}
	switch {
	case errors.Is(err, ErrNotInstalled):
		return Resolved{}, fmt.Errorf("%w: %w", ErrNotFound, err)
	case err != nil:
		return Resolved{}, err
	}
	return Resolved{Path: inst.Path, Version: inst.Version}, nil
}
