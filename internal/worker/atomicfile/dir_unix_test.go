//go:build !windows

package atomicfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMkdirPrivateUnix(t *testing.T) {
	t.Parallel()
	base := t.TempDir()

	created := filepath.Join(base, "a", "b")
	res, err := MkdirPrivate(created, DirOptions{})
	if err != nil || len(res.Warnings) != 0 {
		t.Fatalf("new directory: %+v, %v", res, err)
	}
	if info, err := os.Stat(created); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Errorf("new directory: %v, %v; want it private to the owner", info, err)
	}

	tests := []struct {
		name    string
		mode    os.FileMode
		wantErr string
		warn    string
	}{
		{name: "private", mode: 0o700},
		{name: "readable", mode: 0o755},
		{name: "world-writable", mode: 0o777, wantErr: "chmod o-w"},
		{name: "sticky world-writable", mode: 0o777 | os.ModeSticky, warn: "every user can write to it"},
		{name: "group-writable", mode: 0o770, warn: "can write to it"},
	}
	for _, tt := range tests {
		dir := filepath.Join(base, strings.ReplaceAll(tt.name, " ", "-"))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, tt.mode); err != nil {
			t.Fatal(err)
		}
		res, err := MkdirPrivate(dir, DirOptions{Reprotect: true})
		switch {
		case tt.wantErr != "":
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("%s: err = %v, want %q", tt.name, err, tt.wantErr)
			}
		case err != nil:
			t.Errorf("%s: %v", tt.name, err)
		case tt.warn == "" && len(res.Warnings) != 0:
			t.Errorf("%s: unexpected warnings %q", tt.name, res.Warnings)
		case tt.warn != "" && (len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], tt.warn) || !strings.Contains(res.Warnings[0], dir)):
			t.Errorf("%s: warnings %q, want one about %q", tt.name, res.Warnings, tt.warn)
		}
		// Unix permissions of an existing directory are never changed.
		if info, err := os.Stat(dir); err != nil || info.Mode()&(os.ModePerm|os.ModeSticky) != tt.mode {
			t.Errorf("%s: mode is now %v, want %v unchanged", tt.name, info.Mode(), tt.mode)
		}
	}

	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MkdirPrivate(blocker, DirOptions{}); err == nil {
		t.Error("a file was accepted as a directory")
	}
}

func TestExposureUnix(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin := filepath.Join(dir, "syncr-worker")
	if err := os.WriteFile(bin, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := Exposure(bin); err != nil || len(got) != 0 {
		t.Errorf("0755 file of the current user: %q, %v", got, err)
	}
	if err := os.Chmod(bin, 0o777); err != nil {
		t.Fatal(err)
	}
	got, err := Exposure(bin)
	if err != nil || len(got) != 2 || !strings.Contains(strings.Join(got, "; "), "every user can write to it") {
		t.Errorf("0777 file: %q, %v", got, err)
	}
	if _, err := Exposure(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
		t.Errorf("missing file: err = %v", err)
	}
}
