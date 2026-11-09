//go:build windows

package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// openToUsers gives dir the DACL an admin-created folder under
// %ProgramData% inherits: BUILTIN\Users may add and change files.
func openToUsers(t *testing.T, dir string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + user.User.Sid.String() + ")(A;OICI;0x1301bf;;;BU)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

// TestExistingDataDirOpenToUsers: a data directory other accounts could
// change is restricted when it holds only the worker's files (an earlier
// setup, or `tools fetch` before setup), and refused before enrolling when
// it holds anything else, since restricting it would lock others out.
func TestExistingDataDirOpenToUsers(t *testing.T) {
	t.Parallel()
	t.Run("worker files only", func(t *testing.T) {
		t.Parallel()
		c := newFakeController(t)
		r := newRig(t)
		if err := os.MkdirAll(filepath.Join(r.data, "tools"), 0o700); err != nil {
			t.Fatal(err)
		}
		openToUsers(t, r.data)
		r.mustRun(r.activeOptions(c))
		mustContain(t, "output", r.out.String(), "warning: "+r.data+" could be changed by", "now restricted")
	})
	t.Run("other content", func(t *testing.T) {
		t.Parallel()
		c := newFakeController(t)
		r := newRig(t)
		if err := os.MkdirAll(filepath.Join(r.data, "Documents"), 0o700); err != nil {
			t.Fatal(err)
		}
		openToUsers(t, r.data)
		_, err := r.run(r.activeOptions(c))
		if err == nil || !strings.Contains(err.Error(), "only administrators can change") || !strings.Contains(err.Error(), "--data-dir") {
			t.Fatalf("err = %v, want a refusal that suggests --data-dir", err)
		}
		if len(c.seen()) != 0 {
			t.Error("the token was spent")
		}
	})
}
