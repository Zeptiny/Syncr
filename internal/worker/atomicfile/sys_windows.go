//go:build windows

package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
)

func isWindows() bool { return true }

// syncDir does nothing: Windows cannot sync a directory handle.
func syncDir(string) {}

// createTemp creates Write's temporary file. With RestrictACL the file is
// created with a protected DACL that grants full control to SYSTEM and the
// Administrators group (and the current user if asked) and nothing to anyone
// else, so no other account can open it, not even in the moment before a
// DACL set afterwards would apply. It is also opened without sharing, so no
// other handle exists while it is written.
func createTemp(dir, base string, opts Options) (f *os.File, aclWarning, err error) {
	if !opts.RestrictACL {
		f, err = os.CreateTemp(dir, tempPattern(base))
		return f, nil, err
	}
	sd, sdErr := privateDescriptor(false, opts.IncludeCurrentUser)
	if sdErr != nil {
		// Best effort, as documented: the file gets the permissions it
		// inherits from dir, and the caller warns.
		f, err = os.CreateTemp(dir, tempPattern(base))
		return f, fmt.Errorf("restrict access to %s: %w", filepath.Join(dir, base), sdErr), err
	}
	sa := securityAttributes(sd)
	for range 10000 {
		name := filepath.Join(dir, "."+base+"."+strconv.FormatUint(uint64(rand.Uint32()), 10)+".tmp") //nolint:gosec // a unique name, not a secret
		p, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return nil, nil, &fs.PathError{Op: "open", Path: name, Err: err}
		}
		h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, sa,
			windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			continue
		}
		if err != nil {
			return nil, nil, &fs.PathError{Op: "open", Path: name, Err: err}
		}
		return os.NewFile(uintptr(h), name), nil, nil
	}
	return nil, nil, &fs.PathError{Op: "createtemp", Path: filepath.Join(dir, tempPattern(base)), Err: fs.ErrExist}
}

// privateDescriptor returns a security descriptor whose protected DACL
// grants full control to SYSTEM and the Administrators group, plus the
// current user if includeUser, and to nobody else; it inherits nothing from
// the parent directory. For a directory (inherit) the entries are inherited
// by everything created inside it.
func privateDescriptor(inherit, includeUser bool) (*windows.SECURITY_DESCRIPTOR, error) {
	flags := ""
	if inherit {
		flags = "OICI"
	}
	ace := func(sid string) string { return "(A;" + flags + ";FA;;;" + sid + ")" }
	sddl := "D:P" + ace("SY") + ace("BA")
	if includeUser {
		user, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			return nil, fmt.Errorf("find the current user: %w", err)
		}
		sddl += ace(user.User.Sid.String())
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, fmt.Errorf("build the security descriptor: %w", err)
	}
	return sd, nil
}

func securityAttributes(sd *windows.SECURITY_DESCRIPTOR) *windows.SecurityAttributes {
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	return sa
}
