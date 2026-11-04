package tools

import (
	"errors"
	"fmt"
	"net/http"
)

// Config describes how a worker finds its tools. The composition root fills
// it from the worker's configuration and environment.
type Config struct {
	// Manifest pins the tools; the zero value means DefaultManifest().
	Manifest Manifest
	// Configured maps tool names to explicit binary paths (first provider).
	Configured map[string]string
	// BundledDir is the directory holding binaries shipped with the worker,
	// normally the directory of the worker executable. Empty disables the
	// bundled provider.
	BundledDir string
	// ToolsDir is the managed tools directory. Empty disables the managed
	// provider.
	ToolsDir string
	// Download lets the managed provider fetch missing tools. When false it
	// only reports what is already installed.
	Download bool
	// Platform selects the managed artifacts; the zero value means the host.
	// Binaries of other platforms are installed but not executed.
	Platform Platform
	// Runner runs `<tool> version`; usually a *proc.Supervisor. Required.
	Runner Runner
	// HTTPClient and BaseURL are passed to the Installer.
	HTTPClient *http.Client
	BaseURL    string
}

// New builds the standard provider chain (configured, bundled, managed,
// PATH) described by cfg.
func New(cfg Config) (*Resolver, error) {
	if cfg.Runner == nil {
		return nil, errors.New("tools: a Runner is required")
	}
	if len(cfg.Manifest.Tools) == 0 {
		cfg.Manifest = DefaultManifest()
	}
	platform := cfg.Platform
	if platform == (Platform{}) {
		platform = HostPlatform()
	}
	det := NewProcDetector(cfg.Runner)

	providers := []Provider{NewConfiguredProvider(cfg.Configured, det)}
	if cfg.BundledDir != "" {
		providers = append(providers, NewBundledProvider(cfg.BundledDir, det))
	}
	if cfg.ToolsDir != "" {
		inst, err := NewInstaller(InstallerOptions{
			Dir: cfg.ToolsDir, HTTPClient: cfg.HTTPClient, BaseURL: cfg.BaseURL,
			Detector: det,
		})
		if err != nil {
			return nil, err
		}
		providers = append(providers, NewManagedProvider(inst, platform, cfg.Download))
	}
	providers = append(providers, NewPathProvider(nil, det))

	r, err := NewResolver(cfg.Manifest, providers...)
	if err != nil {
		return nil, fmt.Errorf("tools: %w", err)
	}
	return r, nil
}
