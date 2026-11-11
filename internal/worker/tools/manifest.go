package tools

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"regexp"
	"runtime"
	"slices"
	"strings"
)

// Platform identifies an operating system and CPU architecture using Go's
// GOOS/GOARCH names.
type Platform struct {
	OS   string
	Arch string
}

// HostPlatform returns the platform the process runs on.
func HostPlatform() Platform { return Platform{OS: runtime.GOOS, Arch: runtime.GOARCH} }

// String formats the platform as "os/arch".
func (p Platform) String() string { return p.OS + "/" + p.Arch }

// Format is the archive format of an Artifact.
type Format string

// Supported archive formats.
const (
	// FormatZip is a zip archive; Artifact.Entry names the member holding
	// the binary.
	FormatZip Format = "zip"
	// FormatBzip2 is a single bzip2-compressed binary (restic's Linux
	// releases); Artifact.Entry must be empty.
	FormatBzip2 Format = "bz2"
	// FormatTarGz is a gzip-compressed tar archive (the worker's own Linux
	// releases); Artifact.Entry names the member holding the binary.
	FormatTarGz Format = "tar.gz"
)

// Artifact is one downloadable release archive of a tool.
type Artifact struct {
	// URL is the absolute https download URL.
	URL string
	// SHA256 is the lower-case hex SHA-256 of the archive as published.
	SHA256 string
	// Format is the archive format.
	Format Format
	// Entry is the slash-separated path of the binary inside a zip or tar.gz
	// archive; empty for FormatBzip2.
	Entry string
}

// Spec pins one tool: its version, how to detect it and where to get it.
type Spec struct {
	// Name is the tool and its executable name without extension.
	Name string
	// Version is the pinned version without a leading "v".
	Version string
	// VersionArgs are the arguments that make the tool print its version.
	VersionArgs []string
	// VersionPattern is a regular expression, matched in multi-line mode
	// against the version output, with exactly one capture group yielding
	// the version without a leading "v".
	VersionPattern string
	// Artifacts maps each supported platform to its release archive.
	Artifacts map[Platform]Artifact
}

// Binary returns the executable file name of the tool on the given OS.
func (s Spec) Binary(goos string) string {
	if goos == "windows" {
		return s.Name + ".exe"
	}
	return s.Name
}

// Artifact returns the archive for p, or an error wrapping
// ErrUnsupportedPlatform.
func (s Spec) Artifact(p Platform) (Artifact, error) {
	a, ok := s.Artifacts[p]
	if !ok {
		return Artifact{}, fmt.Errorf("%w: %s %s has no release for %s (supported: %s)",
			ErrUnsupportedPlatform, s.Name, s.Version, p, strings.Join(s.platformNames(), ", "))
	}
	return a, nil
}

func (s Spec) platformNames() []string {
	names := make([]string, 0, len(s.Artifacts))
	for p := range s.Artifacts {
		names = append(names, p.String())
	}
	slices.Sort(names)
	return names
}

// Manifest is the set of tools a worker needs.
type Manifest struct {
	Tools []Spec
}

// Lookup returns the Spec named name.
func (m Manifest) Lookup(name string) (Spec, bool) {
	for _, s := range m.Tools {
		if s.Name == name {
			return s, true
		}
	}
	return Spec{}, false
}

// Platforms returns every platform supported by at least one tool, sorted.
func (m Manifest) Platforms() []Platform {
	var out []Platform
	for _, s := range m.Tools {
		for p := range s.Artifacts {
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	slices.SortFunc(out, func(a, b Platform) int { return strings.Compare(a.String(), b.String()) })
	return out
}

var (
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	namePattern   = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
)

// Validate checks the manifest for internal consistency. It does not touch
// the network.
func (m Manifest) Validate() error {
	if len(m.Tools) == 0 {
		return errors.New("tools: manifest has no tools")
	}
	seen := make(map[string]bool, len(m.Tools))
	var errs []error
	for _, s := range m.Tools {
		if seen[s.Name] {
			errs = append(errs, fmt.Errorf("duplicate tool %q", s.Name))
		}
		seen[s.Name] = true
		if err := s.validate(); err != nil {
			errs = append(errs, fmt.Errorf("tool %q: %w", s.Name, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("tools: invalid manifest: %w", err)
	}
	return nil
}

func (s Spec) validate() error {
	var errs []error
	if !namePattern.MatchString(s.Name) {
		errs = append(errs, errors.New("name must be a lower-case identifier"))
	}
	if s.Version == "" || strings.HasPrefix(s.Version, "v") {
		errs = append(errs, errors.New("version must be set and have no leading v"))
	}
	if len(s.VersionArgs) == 0 {
		errs = append(errs, errors.New("version args must not be empty"))
	}
	if re, err := compileVersionPattern(s.VersionPattern); err != nil {
		errs = append(errs, err)
	} else if re.NumSubexp() != 1 {
		errs = append(errs, errors.New("version pattern needs exactly one capture group"))
	}
	if len(s.Artifacts) == 0 {
		errs = append(errs, errors.New("no artifacts"))
	}
	for p, a := range s.Artifacts {
		if err := a.validate(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
		}
	}
	return errors.Join(errs...)
}

func (a Artifact) validate() error {
	var errs []error
	if u, err := url.Parse(a.URL); err != nil || u.Scheme != "https" || u.Host == "" {
		errs = append(errs, fmt.Errorf("url %q must be an absolute https URL", a.URL))
	}
	if !sha256Pattern.MatchString(a.SHA256) {
		errs = append(errs, errors.New("sha256 must be 64 lower-case hex digits"))
	}
	switch a.Format {
	case FormatZip, FormatTarGz:
		if !fs.ValidPath(a.Entry) || a.Entry == "." {
			errs = append(errs, fmt.Errorf("%s entry %q is not a clean relative path", a.Format, a.Entry))
		}
	case FormatBzip2:
		if a.Entry != "" {
			errs = append(errs, errors.New("bz2 artifacts have no entry"))
		}
	default:
		errs = append(errs, fmt.Errorf("unknown format %q", a.Format))
	}
	return errors.Join(errs...)
}

func compileVersionPattern(p string) (*regexp.Regexp, error) {
	re, err := regexp.Compile("(?m)" + p)
	if err != nil {
		return nil, fmt.Errorf("version pattern: %w", err)
	}
	return re, nil
}
