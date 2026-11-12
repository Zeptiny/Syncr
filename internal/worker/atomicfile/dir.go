package atomicfile

import "io/fs"

// DirOptions controls MkdirPrivate.
type DirOptions struct {
	// Perm is the mode of the directories created on Unix. Zero means 0700.
	Perm fs.FileMode
	// IncludeCurrentUser also grants the current user access on Windows,
	// for per-user installs that run without admin rights.
	IncludeCurrentUser bool
	// Reprotect lets MkdirPrivate restrict an existing Windows directory
	// that other accounts can change, instead of refusing it: its DACL is
	// replaced by the private one, which also takes away what its contents
	// inherited, and everything in it must then have a trusted owner. Set it
	// only for a directory that belongs to the caller alone, never for one
	// that other programs or users share. It has no effect on Unix.
	Reprotect bool
}

// DirResult reports what MkdirPrivate found but did not refuse.
type DirResult struct {
	// Warnings are sentences for the operator: on Unix an existing
	// directory with a foreign owner or write access for its group, on
	// Windows an existing directory that had to be restricted.
	Warnings []string
}

// MkdirPrivate makes path a directory that only trusted accounts can
// change, creating it and any missing parents. Trusted are root and the
// current user on Unix; SYSTEM, the Administrators group, TrustedInstaller
// and the current user on Windows.
//
// On Windows every directory it creates gets a protected DACL from the
// start: full control for SYSTEM and Administrators (and the current user
// with IncludeCurrentUser), inherited by everything created inside, and
// nothing for anyone else. An existing directory must be a real directory
// (not a link or junction) with a trusted owner, and no other account may be
// able to change it; with Reprotect it is restricted instead (see
// DirOptions). Otherwise MkdirPrivate fails, saying what is wrong.
//
// On Unix directories are created with Perm, as os.MkdirAll does. An
// existing directory that every user may write to (without the sticky bit)
// is refused; other findings are only reported in DirResult.
func MkdirPrivate(path string, opts DirOptions) (DirResult, error) {
	return mkdirPrivate(path, opts)
}

// Exposure describes how accounts other than the trusted ones (see
// MkdirPrivate) can change path, for example an untrusted owner or a
// permission that lets others write, delete or re-permission it. An empty
// result means that only trusted accounts can change it.
func Exposure(path string) ([]string, error) { return exposure(path) }
