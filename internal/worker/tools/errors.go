package tools

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors. Use errors.Is to test for them.
var (
	// ErrNotFound is returned by a Provider that has no candidate binary for
	// the tool. The Resolver treats it as "try the next provider".
	ErrNotFound = errors.New("tool not found")
	// ErrUnknownTool means the manifest does not pin the requested tool.
	ErrUnknownTool = errors.New("unknown tool")
	// ErrUnsupportedPlatform means the manifest has no release of a tool for
	// the requested OS and architecture.
	ErrUnsupportedPlatform = errors.New("unsupported platform")
	// ErrNotInstalled means the managed install of a tool is absent.
	ErrNotInstalled = errors.New("tool not installed")
	// ErrInstallInvalid means a managed install exists but does not match its
	// recorded or pinned digest and version.
	ErrInstallInvalid = errors.New("installed tool failed verification")
)

// ChecksumError reports that a downloaded archive does not match the SHA-256
// pinned in the manifest. Nothing was extracted.
type ChecksumError struct {
	URL  string
	Want string
	Got  string
}

// Error implements error.
func (e *ChecksumError) Error() string {
	return fmt.Sprintf("tools: checksum mismatch for %s: got sha256 %s, pinned %s", e.URL, e.Got, e.Want)
}

// VersionMismatchError reports a binary whose version differs from the pin.
type VersionMismatchError struct {
	Tool string
	Path string
	Got  string
	Want string
}

// Error implements error.
func (e *VersionMismatchError) Error() string {
	return fmt.Sprintf("tools: %s at %s is version %s, want pinned %s", e.Tool, e.Path, e.Got, e.Want)
}

// Rejection records why a provider's candidate was not used.
type Rejection struct {
	Source Source
	Err    error
}

// ResolveError reports that no provider produced a verified binary.
type ResolveError struct {
	Tool     string
	Rejected []Rejection
}

// Error implements error.
func (e *ResolveError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "tools: no verified %s binary", e.Tool)
	for _, r := range e.Rejected {
		fmt.Fprintf(&b, "; %s: %v", r.Source, r.Err)
	}
	return b.String()
}

// Unwrap exposes every provider's error to errors.Is and errors.As.
func (e *ResolveError) Unwrap() []error {
	errs := make([]error, len(e.Rejected))
	for i, r := range e.Rejected {
		errs[i] = r.Err
	}
	return errs
}
