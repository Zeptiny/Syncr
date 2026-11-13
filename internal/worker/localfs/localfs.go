// Package localfs enforces the worker's local_roots: the directories the
// worker's admin allows jobs to read and write on the local filesystem.
//
// A Policy accepts a path only if it is absolute and, after resolving
// symbolic links (and, on Windows, junctions and mount points), lies inside
// one of the roots. Paths that do not exist yet (restore targets, new
// destination directories) are resolved through their deepest existing
// ancestor, so they are accepted when that ancestor is inside a root.
//
// Comparisons are component-wise, so "/data2" is never inside "/data". On
// Windows, volume names and path components compare case-insensitively,
// drive-relative ("C:foo") and rooted-but-driveless ("\foo") paths are
// rejected, UNC shares are supported, and device or verbatim paths
// ("\\.\", "\\?\") as well as reserved names (NUL, CON...) and alternate data
// streams ("file:stream") are rejected.
//
// Checks are made at the time of Resolve; a symlink created inside a root
// afterwards can still redirect the tool. Roots should therefore not be
// writable by untrusted local users.
//
// Allowed dependencies: the standard library and golang.org/x/sys.
package localfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Sentinel errors, matched with errors.Is on a *PathError.
var (
	// ErrInvalidPath reports a path that is not an acceptable absolute path
	// (empty, relative, drive-relative, device path, NUL byte...).
	ErrInvalidPath = errors.New("not an absolute local path")

	// ErrNotAllowed reports a path that resolves outside every root (or a
	// policy without roots).
	ErrNotAllowed = errors.New("path is outside the worker's local_roots")

	// ErrUnresolvable reports a path whose existing part cannot be resolved:
	// a dangling symlink, a non-directory in the middle of the path, or an
	// I/O or permission error.
	ErrUnresolvable = errors.New("cannot resolve path")
)

// PathError describes a rejected path. Its message names the path, which is
// user input and never secret.
type PathError struct {
	Path string
	// Err is one of the sentinel errors, possibly joined with the
	// underlying cause.
	Err error
}

// Error implements error.
func (e *PathError) Error() string { return fmt.Sprintf("local path %q: %v", e.Path, e.Err) }

// Unwrap returns the underlying error.
func (e *PathError) Unwrap() error { return e.Err }

// root is an allowed root: as configured (cleaned) and resolved.
type root struct {
	clean    string
	resolved string
}

// Policy checks local paths against a fixed set of roots. It is immutable and
// safe for concurrent use.
type Policy struct {
	roots []root
}

// NewPolicy returns a policy for roots. Every root must be an absolute path;
// roots that do not exist yet are allowed (they are resolved through their
// deepest existing ancestor). An empty list yields a policy that denies every
// path.
func NewPolicy(roots []string) (*Policy, error) {
	p := &Policy{roots: make([]root, 0, len(roots))}
	var errs []error
	for _, r := range roots {
		clean, err := cleanAbs(r)
		if err != nil {
			errs = append(errs, fmt.Errorf("local root: %w", err))
			continue
		}
		resolved, err := resolve(clean)
		if err != nil {
			errs = append(errs, fmt.Errorf("local root: %w", err))
			continue
		}
		p.roots = append(p.roots, root{clean: clean, resolved: resolved})
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return p, nil
}

// Roots returns the configured roots, cleaned, in configuration order.
func (p *Policy) Roots() []string {
	out := make([]string, len(p.roots))
	for i, r := range p.roots {
		out[i] = r.clean
	}
	return out
}

// Resolve checks path and returns it cleaned (absolute, with "." and ".."
// removed lexically), ready to hand to a tool. The path is accepted only if
// its resolved form (symlinks, junctions) is inside a root. Errors are
// *PathError values wrapping ErrInvalidPath, ErrNotAllowed or
// ErrUnresolvable.
//
// The cleaned path, not the resolved one, is returned so that tools record
// the paths users chose (e.g. in restic snapshots). Because ".." is removed
// before resolution, the OS resolves the returned path to the location that
// was checked.
func (p *Policy) Resolve(path string) (string, error) {
	clean, err := cleanAbs(path)
	if err != nil {
		return "", err
	}
	if len(p.roots) == 0 {
		return "", &PathError{Path: path, Err: fmt.Errorf("%w (no local_roots configured)", ErrNotAllowed)}
	}
	resolved, err := resolve(clean)
	if err != nil {
		return "", err
	}
	for _, r := range p.roots {
		if within(r.resolved, resolved) {
			return clean, nil
		}
	}
	return "", &PathError{Path: path, Err: ErrNotAllowed}
}

// Overlap reports whether the absolute paths a and b are the same location
// or one is inside the other, compared component-wise as Resolve compares
// (case-insensitively on Windows), both as written and resolved as Resolve
// resolves them (symbolic links, junctions, through the deepest existing
// ancestor). Errors are *PathError values, as from Resolve.
func Overlap(a, b string) (bool, error) {
	ca, err := cleanAbs(a)
	if err != nil {
		return false, err
	}
	cb, err := cleanAbs(b)
	if err != nil {
		return false, err
	}
	if within(ca, cb) || within(cb, ca) {
		return true, nil
	}
	ra, err := resolve(ca)
	if err != nil {
		return false, err
	}
	rb, err := resolve(cb)
	if err != nil {
		return false, err
	}
	return within(ra, rb) || within(rb, ra), nil
}

// cleanAbs validates path and returns it cleaned.
func cleanAbs(path string) (string, error) {
	switch {
	case path == "":
		return "", &PathError{Path: path, Err: fmt.Errorf("%w: empty", ErrInvalidPath)}
	case strings.ContainsRune(path, 0):
		return "", &PathError{Path: path, Err: fmt.Errorf("%w: contains NUL", ErrInvalidPath)}
	case !filepath.IsAbs(path):
		return "", &PathError{Path: path, Err: fmt.Errorf("%w: relative", ErrInvalidPath)}
	}
	clean := filepath.Clean(path)
	if err := checkPlatform(clean); err != nil {
		return "", &PathError{Path: path, Err: fmt.Errorf("%w: %w", ErrInvalidPath, err)}
	}
	return clean, nil
}

// resolve resolves the deepest existing ancestor of the clean absolute path
// clean and appends the remaining (non-existent) components.
func resolve(clean string) (string, error) {
	cur := clean
	var rest []string
	for {
		resolved, err := resolveExisting(cur)
		if err == nil {
			parts := append([]string{resolved}, rest...)
			return filepath.Join(parts...), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", &PathError{Path: clean, Err: errors.Join(ErrUnresolvable, err)}
		}
		// cur does not resolve. If it exists as a link, the link dangles: a
		// tool creating cur would follow it to an unchecked location.
		if _, lerr := os.Lstat(cur); lerr == nil {
			return "", &PathError{Path: clean, Err: fmt.Errorf("%w: %s is a dangling link", ErrUnresolvable, cur)}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Even the volume root does not exist (e.g. an unmounted drive).
			return "", &PathError{Path: clean, Err: errors.Join(ErrUnresolvable, err)}
		}
		rest = append([]string{filepath.Base(cur)}, rest...)
		cur = parent
	}
}

// within reports whether path is rootPath or inside it, comparing volume
// names and components with the platform's name comparison. Both must be
// clean absolute paths.
func within(rootPath, path string) bool {
	rv, pv := filepath.VolumeName(rootPath), filepath.VolumeName(path)
	if !nameEqual(rv, pv) {
		return false
	}
	rc, pc := components(rootPath[len(rv):]), components(path[len(pv):])
	if len(pc) < len(rc) {
		return false
	}
	for i := range rc {
		if !nameEqual(rc[i], pc[i]) {
			return false
		}
	}
	return true
}

// components splits a volume-less path into its non-empty components.
func components(p string) []string {
	return strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == filepath.Separator })
}
