//go:build windows

package atomicfile

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// daclOf returns whether the DACL of path is protected and the SIDs of its
// entries, as strings.
func daclOf(t *testing.T, path string) (protected bool, sids []string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	for i := range uint32(acl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			t.Fatal(err)
		}
		// The SID starts at SidStart and runs past the struct.
		sids = append(sids, (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String())
	}
	return control&windows.SE_DACL_PROTECTED != 0, sids
}

func privateSIDs(t *testing.T, includeUser bool) []string {
	t.Helper()
	want := []string{"S-1-5-18", "S-1-5-32-544"}
	if includeUser {
		user, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, user.User.Sid.String())
	}
	return want
}

func TestWriteRestrictsACL(t *testing.T) {
	t.Parallel()
	for _, includeUser := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "secret.yaml")
		res, err := Write(path, []byte("x"), Options{RestrictACL: true, IncludeCurrentUser: includeUser})
		if err != nil {
			t.Fatal(err)
		}
		if res.ACLWarning != nil {
			t.Fatalf("ACL warning: %v", res.ACLWarning)
		}
		protected, sids := daclOf(t, path)
		if !protected {
			t.Error("DACL still inherits from the parent directory")
		}
		if want := privateSIDs(t, includeUser); !slices.Equal(sids, want) {
			t.Errorf("includeUser=%v: DACL entries %v, want %v", includeUser, sids, want)
		}
	}
}

// TestTempFileIsPrivateFromCreation: the secret must never sit in a file
// that other accounts could open, not even before Write restricts it.
func TestTempFileIsPrivateFromCreation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f, aclWarning, err := createTemp(dir, "worker.yaml", Options{RestrictACL: true})
	if err != nil || aclWarning != nil {
		t.Fatalf("createTemp: %v, %v", err, aclWarning)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	defer func() { _ = f.Close() }()

	protected, sids := daclOf(t, f.Name())
	if !protected || !slices.Equal(sids, privateSIDs(t, false)) {
		t.Errorf("temporary file DACL: protected %v, entries %v", protected, sids)
	}
	// Opened without sharing: nobody else gets a handle while it is written.
	if g, err := os.Open(f.Name()); err == nil {
		_ = g.Close()
		t.Error("a second handle to the temporary file could be opened")
	}
	if _, err := f.WriteString("secret"); err != nil {
		t.Errorf("the creating handle cannot write: %v", err)
	}
}
