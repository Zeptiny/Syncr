//go:build windows

package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// fileDeleteChild is FILE_DELETE_CHILD, which x/sys/windows lacks.
	fileDeleteChild = 0x40
	// accessAllowedCallbackACEType is ACCESS_ALLOWED_CALLBACK_ACE_TYPE (a
	// conditional ACE), laid out like ACCESS_ALLOWED_ACE up to the SID.
	accessAllowedCallbackACEType = 9
	// trustedInstallerSID is NT SERVICE\TrustedInstaller, the owner of most
	// of Windows and Program Files.
	trustedInstallerSID = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
	// writeRights let an account change a file or directory, what it
	// contains, or who may access it.
	writeRights = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA | fileDeleteChild |
		windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_WRITE | windows.GENERIC_ALL
)

func mkdirPrivate(path string, opts DirOptions) (DirResult, error) {
	path = filepath.Clean(path)
	sd, err := privateDescriptor(true, opts.IncludeCurrentUser)
	if err != nil {
		return DirResult{}, fmt.Errorf("restrict %s: %w", path, err)
	}
	created, err := createDirs(path, securityAttributes(sd))
	if err != nil || created {
		return DirResult{}, err
	}

	// An existing directory: someone may have prepared it.
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return DirResult{}, &fs.PathError{Op: "mkdir", Path: path, Err: err}
	}
	attrs, err := windows.GetFileAttributes(name)
	switch {
	case err != nil:
		return DirResult{}, &fs.PathError{Op: "mkdir", Path: path, Err: err}
	case attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0:
		return DirResult{}, fmt.Errorf("%s is a link or junction; it must be a real directory (remove it, or name the directory it points to)", path)
	case attrs&windows.FILE_ATTRIBUTE_DIRECTORY == 0:
		return DirResult{}, &fs.PathError{Op: "mkdir", Path: path, Err: syscall.ENOTDIR}
	}
	t, err := loadTrust()
	if err != nil {
		return DirResult{}, fmt.Errorf("check %s: %w", path, err)
	}
	current, err := readSecurity(path, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return DirResult{}, err
	}
	owner, writers, err := t.check(current)
	if err != nil {
		return DirResult{}, fmt.Errorf("check %s: %w", path, err)
	}
	if owner != "" {
		return DirResult{}, untrustedOwnerError(path, path, owner)
	}
	if len(writers) == 0 {
		return DirResult{}, nil
	}
	if !opts.Reprotect {
		return DirResult{}, fmt.Errorf("%s can be changed by %s, who could then tamper with the worker's files; "+
			"choose a directory that only administrators can change", path, strings.Join(writers, ", "))
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return DirResult{}, fmt.Errorf("restrict %s: %w", path, err)
	}
	// SetNamedSecurityInfo also propagates the new inheritable entries to
	// everything below, replacing what it inherited from the old DACL.
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return DirResult{}, fmt.Errorf("restrict %s: %w", path, err)
	}
	// What another account created while it could still keeps its owner,
	// who can change it at will: refuse the directory if there is any.
	if err := t.checkOwners(path); err != nil {
		return DirResult{}, err
	}
	return DirResult{Warnings: []string{fmt.Sprintf("%s could be changed by %s; it is now restricted to SYSTEM and Administrators", path, strings.Join(writers, ", "))}}, nil
}

// createDirs creates path and its missing parents with sa and reports
// whether it created path. A directory that appears meanwhile is left to
// the caller's checks.
func createDirs(path string, sa *windows.SecurityAttributes) (bool, error) {
	var missing []string
	for p := path; ; {
		_, err := os.Lstat(p)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		missing = append(missing, p)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	for i := len(missing) - 1; i >= 0; i-- {
		name, err := windows.UTF16PtrFromString(missing[i])
		if err != nil {
			return false, &fs.PathError{Op: "mkdir", Path: missing[i], Err: err}
		}
		err = windows.CreateDirectory(name, sa)
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			if i == 0 {
				return false, nil
			}
			continue
		}
		if err != nil {
			return false, &fs.PathError{Op: "mkdir", Path: missing[i], Err: err}
		}
	}
	return len(missing) > 0, nil
}

func exposure(path string) ([]string, error) {
	t, err := loadTrust()
	if err != nil {
		return nil, err
	}
	sd, err := readSecurity(path, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	owner, writers, err := t.check(sd)
	if err != nil {
		return nil, fmt.Errorf("check %s: %w", path, err)
	}
	var out []string
	if owner != "" {
		out = append(out, "it is owned by "+owner)
	}
	if len(writers) > 0 {
		out = append(out, strings.Join(writers, ", ")+" can change it")
	}
	return out, nil
}

// trust holds the accounts that may own and change the worker's files.
type trust struct {
	trusted []*windows.SID
	// ignored are placeholders (CREATOR OWNER, CREATOR GROUP, OWNER RIGHTS)
	// that grant nothing by themselves; the owner is checked on its own.
	ignored []*windows.SID
}

func loadTrust() (*trust, error) {
	t := &trust{}
	for _, kind := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		sid, err := windows.CreateWellKnownSid(kind)
		if err != nil {
			return nil, err
		}
		t.trusted = append(t.trusted, sid)
	}
	ti, err := windows.StringToSid(trustedInstallerSID)
	if err != nil {
		return nil, err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("find the current user: %w", err)
	}
	t.trusted = append(t.trusted, ti, user.User.Sid)
	for _, kind := range []windows.WELL_KNOWN_SID_TYPE{windows.WinCreatorOwnerSid, windows.WinCreatorGroupSid, windows.WinCreatorOwnerRightsSid} {
		sid, err := windows.CreateWellKnownSid(kind)
		if err != nil {
			return nil, err
		}
		t.ignored = append(t.ignored, sid)
	}
	return t, nil
}

func contains(list []*windows.SID, sid *windows.SID) bool {
	return sid != nil && slices.ContainsFunc(list, sid.Equals)
}

// check returns the owner of sd if it is not trusted, and the other
// accounts its DACL lets change the object (including through entries that
// only apply to what is created inside a directory). Deny entries are
// ignored, so the answer errs on the side of reporting.
func (t *trust) check(sd *windows.SECURITY_DESCRIPTOR) (badOwner string, writers []string, err error) {
	owner, _, err := sd.Owner()
	if err != nil {
		return "", nil, err
	}
	if !contains(t.trusted, owner) {
		badOwner = accountName(owner)
	}
	dacl, _, err := sd.DACL()
	if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) || (err == nil && dacl == nil) {
		return badOwner, []string{"everyone (it has no access control list)"}, nil
	}
	if err != nil {
		return "", nil, err
	}
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return "", nil, err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE && ace.Header.AceType != accessAllowedCallbackACEType {
			continue
		}
		if ace.Mask&writeRights == 0 {
			continue
		}
		// The SID starts at SidStart and runs past the struct.
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)) //nolint:gosec // the layout of an ACCESS_ALLOWED_ACE
		if contains(t.trusted, sid) || contains(t.ignored, sid) {
			continue
		}
		if name := accountName(sid); !slices.Contains(writers, name) {
			writers = append(writers, name)
		}
	}
	return badOwner, writers, nil
}

// checkOwners requires everything below root to have a trusted owner.
func (t *trust) checkOwners(root string) error {
	return filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("check the contents of %s: %w", root, err)
		}
		if p == root {
			return nil
		}
		sd, err := readSecurity(p, windows.OWNER_SECURITY_INFORMATION)
		if err != nil {
			return fmt.Errorf("check who owns %s: %w", p, err)
		}
		owner, _, err := sd.Owner()
		if err != nil {
			return fmt.Errorf("check who owns %s: %w", p, err)
		}
		if !contains(t.trusted, owner) {
			return untrustedOwnerError(root, p, accountName(owner))
		}
		return nil
	})
}

func untrustedOwnerError(root, path, owner string) error {
	what := path
	if path != root {
		what = path + " (in " + root + ")"
	}
	return fmt.Errorf("%s is owned by %s, not by SYSTEM, Administrators or the account running this: "+
		"another account may have created it to tamper with the worker. Check what it contains, delete it and try again "+
		`(or take it over as an administrator: takeown /F "%s" /A /R, then icacls "%s" /reset /T)`, what, owner, path, path)
}

// readSecurity reads parts of the security descriptor of path itself (a
// link is not followed).
func readSecurity(path string, info windows.SECURITY_INFORMATION) (*windows.SECURITY_DESCRIPTOR, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(name, windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	defer func() { _ = windows.CloseHandle(h) }()
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, info)
	if err != nil {
		return nil, fmt.Errorf("read the permissions of %s: %w", path, err)
	}
	if sd == nil {
		return nil, fmt.Errorf("%s is on a file system without permissions", path)
	}
	return sd, nil
}

func accountName(sid *windows.SID) string {
	if sid == nil {
		return "nobody"
	}
	account, domain, _, err := sid.LookupAccount("")
	switch {
	case err != nil:
		return sid.String()
	case domain != "":
		return domain + `\` + account
	default:
		return account
	}
}
