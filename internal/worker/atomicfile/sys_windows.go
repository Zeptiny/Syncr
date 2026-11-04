//go:build windows

package atomicfile

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func isWindows() bool { return true }

// syncDir does nothing: Windows cannot sync a directory handle.
func syncDir(string) {}

// restrictACL replaces the DACL of path with one that grants full control to
// SYSTEM and the Administrators group (and the current user if asked) and
// does not inherit from the parent.
func restrictACL(path string, includeUser bool) error {
	var entries []windows.EXPLICIT_ACCESS
	for _, kind := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		sid, err := windows.CreateWellKnownSid(kind)
		if err != nil {
			return fmt.Errorf("restrict access to %s: %w", path, err)
		}
		entries = append(entries, grant(sid, windows.TRUSTEE_IS_WELL_KNOWN_GROUP))
	}
	if includeUser {
		user, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			return fmt.Errorf("restrict access to %s: find the current user: %w", path, err)
		}
		entries = append(entries, grant(user.User.Sid, windows.TRUSTEE_IS_USER))
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("restrict access to %s: %w", path, err)
	}
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil)
	if err != nil {
		return fmt.Errorf("restrict access to %s: %w", path, err)
	}
	return nil
}

func grant(sid *windows.SID, trusteeType windows.TRUSTEE_TYPE) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  trusteeType,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}
