//go:build windows

package atomicfile

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func wellKnownName(t *testing.T, kind windows.WELL_KNOWN_SID_TYPE) string {
	t.Helper()
	sid, err := windows.CreateWellKnownSid(kind)
	if err != nil {
		t.Fatal(err)
	}
	return accountName(sid)
}

// setDACL replaces the DACL of path with the one in sddl.
func setDACL(t *testing.T, path, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

func TestMkdirPrivateCreatesProtectedDirectories(t *testing.T) {
	t.Parallel()
	for _, includeUser := range []bool{false, true} {
		base := t.TempDir()
		dir := filepath.Join(base, "a", "b")
		res, err := MkdirPrivate(dir, DirOptions{IncludeCurrentUser: includeUser})
		if err != nil || len(res.Warnings) != 0 {
			t.Fatalf("MkdirPrivate: %+v, %v", res, err)
		}
		// The missing parent is created private too, so nobody can rename
		// it away and put another directory in its place.
		for _, d := range []string{filepath.Join(base, "a"), dir} {
			protected, sids := daclOf(t, d)
			if !protected || !slices.Equal(sids, privateSIDs(t, includeUser)) {
				t.Errorf("%s: protected %v, entries %v", d, protected, sids)
			}
		}
		// Files created inside inherit the private entries.
		file := filepath.Join(dir, "f")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, sids := daclOf(t, file); !slices.Equal(sids, privateSIDs(t, includeUser)) {
			t.Errorf("file inside: entries %v", sids)
		}
		// Calling it again on the now private directory changes nothing.
		if res, err := MkdirPrivate(dir, DirOptions{IncludeCurrentUser: includeUser}); err != nil || len(res.Warnings) != 0 {
			t.Errorf("again: %+v, %v", res, err)
		}
	}
}

// TestMkdirPrivateExistingDirectory: a directory that other accounts can
// change (here BUILTIN\Users may modify it, as under %ProgramData%) is
// refused, or restricted with Reprotect, including what its contents
// inherited.
func TestMkdirPrivateExistingDirectory(t *testing.T) {
	t.Parallel()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	open := "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + user.User.Sid.String() + ")(A;OICI;0x1301bf;;;BU)"
	users := wellKnownName(t, windows.WinBuiltinUsersSid)

	dir := filepath.Join(t.TempDir(), "Syncr")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	setDACL(t, dir, open)
	child := filepath.Join(dir, "tools")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}

	_, err = MkdirPrivate(dir, DirOptions{})
	if err == nil || !strings.Contains(err.Error(), users) || !strings.Contains(err.Error(), "only administrators") {
		t.Fatalf("without Reprotect: err = %v, want a refusal naming %s", err, users)
	}

	res, err := MkdirPrivate(dir, DirOptions{Reprotect: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], users) {
		t.Errorf("warnings = %q", res.Warnings)
	}
	if protected, sids := daclOf(t, dir); !protected || !slices.Equal(sids, privateSIDs(t, false)) {
		t.Errorf("after Reprotect: protected %v, entries %v", protected, sids)
	}
	if _, sids := daclOf(t, child); slices.Contains(sids, "S-1-5-32-545") {
		t.Errorf("the child still grants BUILTIN\\Users: %v", sids)
	}
	if got, err := Exposure(dir); err != nil || len(got) != 0 {
		t.Errorf("Exposure after Reprotect = %q, %v", got, err)
	}
}

func TestMkdirPrivateRefusesLinks(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symbolic link here: %v", err)
	}
	if _, err := MkdirPrivate(link, DirOptions{Reprotect: true}); err == nil || !strings.Contains(err.Error(), "link") {
		t.Errorf("err = %v, want a refusal of the link", err)
	}
}

// TestTrustCheck covers the decisions on descriptors that tests cannot
// create on disk without extra privileges (a foreign owner).
func TestTrustCheck(t *testing.T) {
	t.Parallel()
	tr, err := loadTrust()
	if err != nil {
		t.Fatal(err)
	}
	everyone := wellKnownName(t, windows.WinWorldSid)
	users := wellKnownName(t, windows.WinBuiltinUsersSid)
	tests := []struct {
		name      string
		sddl      string
		wantOwner bool
		writers   []string
	}{
		{"private", "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)", false, nil},
		{"system owner, trusted installer", "O:SYD:(A;;FA;;;S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464)", false, nil},
		{"foreign owner", "O:BUD:P(A;;FA;;;SY)", true, nil},
		{"read only for users", "O:BAD:(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)", false, nil},
		{"users may create files", "O:BAD:(A;OICI;FA;;;BA)(A;CI;0x6;;;BU)", false, []string{users}},
		{"inherit-only modify for everyone", "O:BAD:(A;OICI;FA;;;BA)(A;OICIIO;0x1301bf;;;WD)", false, []string{everyone}},
		{"creator owner and owner rights", "O:BAD:(A;OICIIO;FA;;;CO)(A;;FA;;;OW)", false, nil},
		{"deny entries are not trusted", "O:BAD:(D;;FA;;;WD)(A;;FA;;;WD)", false, []string{everyone}},
		{"no access list", "O:BA", false, []string{"everyone (it has no access control list)"}},
	}
	for _, tt := range tests {
		sd, err := windows.SecurityDescriptorFromString(tt.sddl)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		owner, writers, err := tr.check(sd)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if (owner != "") != tt.wantOwner || !slices.Equal(writers, tt.writers) {
			t.Errorf("%s: owner %q, writers %q; want untrusted owner %v, writers %q", tt.name, owner, writers, tt.wantOwner, tt.writers)
		}
	}
}
