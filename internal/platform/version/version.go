// Package version reports build information for Syncr binaries.
//
// Release builds inject the values with
//
//	-ldflags "-X github.com/Zeptiny/Syncr/internal/platform/version.version=v2.0.0
//	          -X github.com/Zeptiny/Syncr/internal/platform/version.commit=<sha>
//	          -X github.com/Zeptiny/Syncr/internal/platform/version.buildTime=<RFC 3339>"
//
// (see the Makefile). Without them (go run, go install), Get falls back to the
// module version and VCS revision the Go toolchain embeds.
//
// This package is a leaf: it imports only the standard library.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"time"
)

// Written only by the linker (-ldflags -X); never assigned at run time.
var (
	version   string
	commit    string //nolint:gochecknoglobals // set via -ldflags -X
	buildTime string //nolint:gochecknoglobals // set via -ldflags -X
)

// Fallback values for builds without version information.
const (
	DevVersion    = "dev"
	UnknownCommit = "unknown"
)

// Info describes a binary build.
type Info struct {
	// Version is the release version (e.g. "v2.0.0") or DevVersion.
	Version string
	// Commit is the VCS revision, suffixed with "-dirty" for modified trees,
	// or UnknownCommit.
	Commit string
	// BuildTime is when the binary was built; zero if unknown.
	BuildTime time.Time
	// GoVersion is the Go toolchain version the binary was built with.
	GoVersion string
}

// Get returns the build information of the running binary.
func Get() Info {
	return resolve(version, commit, buildTime, debug.ReadBuildInfo)
}

// String formats the info for humans, e.g.
// "v2.0.0 (commit 1a2b3c4, built 2026-01-02T03:04:05Z, go1.27.3)".
func (i Info) String() string {
	built := "unknown"
	if !i.BuildTime.IsZero() {
		built = i.BuildTime.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("%s (commit %s, built %s, %s)", i.Version, i.Commit, built, i.GoVersion)
}

// resolve merges link-time values with the toolchain's build info.
func resolve(ldVersion, ldCommit, ldBuildTime string, readBuildInfo func() (*debug.BuildInfo, bool)) Info {
	info := Info{
		Version:   ldVersion,
		Commit:    ldCommit,
		GoVersion: runtime.Version(),
	}
	if t, err := time.Parse(time.RFC3339, ldBuildTime); err == nil {
		info.BuildTime = t.UTC()
	}

	if bi, ok := readBuildInfo(); ok {
		if info.Version == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			info.Version = bi.Main.Version
		}
		if info.Commit == "" {
			info.Commit = vcsRevision(bi.Settings)
		}
	}

	if info.Version == "" {
		info.Version = DevVersion
	}
	if info.Commit == "" {
		info.Commit = UnknownCommit
	}
	return info
}

func vcsRevision(settings []debug.BuildSetting) string {
	var revision string
	var modified bool
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision != "" && modified {
		revision += "-dirty"
	}
	return revision
}
