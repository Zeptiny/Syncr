//go:build windows

package atomicfile

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWriteRestrictsACL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		includeUser bool
		wantACEs    uint16
	}{
		{name: "system and administrators", wantACEs: 2},
		{name: "plus current user", includeUser: true, wantACEs: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "secret.yaml")
			res, err := Write(path, []byte("x"), Options{RestrictACL: true, IncludeCurrentUser: tt.includeUser})
			if err != nil {
				t.Fatal(err)
			}
			if res.ACLWarning != nil {
				t.Fatalf("ACL warning: %v", res.ACLWarning)
			}
			sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
			if err != nil {
				t.Fatal(err)
			}
			control, _, err := sd.Control()
			if err != nil {
				t.Fatal(err)
			}
			if control&windows.SE_DACL_PROTECTED == 0 {
				t.Error("DACL still inherits from the parent directory")
			}
			acl, _, err := sd.DACL()
			if err != nil {
				t.Fatal(err)
			}
			if acl.AceCount != tt.wantACEs {
				t.Errorf("DACL has %d entries, want %d", acl.AceCount, tt.wantACEs)
			}
		})
	}
}
