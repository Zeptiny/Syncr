package localfs

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestClaims: jobs share folders, but a folder claimed exclusively (a
// restore's target, a local move's sides) overlaps no other job's folder,
// in either order, as written or through a link; a failed Add changes
// nothing, and Release gives the folders back.
func TestClaims(t *testing.T) {
	t.Parallel()
	root := realTempDir(t)
	data, docs := filepath.Join(root, "data"), filepath.Join(root, "docs")
	mkdirs(t, filepath.Join(data, "a"), docs)
	link := filepath.Join(docs, "to-a")
	symlink(t, filepath.Join(data, "a"), link)

	var c Claims
	backup := c.Take("restic_backup")
	defer backup.Release()
	if err := backup.Add(data, false); err != nil {
		t.Fatal(err)
	}
	copyJob := c.Take("rclone_transfer")
	if err := copyJob.Add(filepath.Join(data, "a"), false); err != nil {
		t.Errorf("shared folders: %v", err)
	}
	copyJob.Release()
	copyJob.Release() // twice is fine

	restore := c.Take("restic_restore")
	for _, path := range []string{filepath.Join(data, "a", "new"), root, link} {
		err := restore.Add(path, true)
		var ie *InUseError
		if !errors.Is(err, ErrInUse) || !errors.As(err, &ie) || ie.Other != data || ie.OtherKind != "restic_backup" || !ie.Exclusive {
			t.Errorf("Add(%s, exclusive) = %v, want in use by the backup", path, err)
		}
	}
	if err := restore.Add(docs, true); err != nil {
		t.Errorf("a folder beside it: %v", err)
	}
	// The backup now meets the restore's folder.
	err := backup.Add(filepath.Join(docs, "x"), false)
	if !errors.Is(err, ErrInUse) || !strings.Contains(err.Error(), "restic_restore") || !strings.Contains(err.Error(), "it restores into or moves that folder") {
		t.Errorf("Add beside an exclusive claim = %v", err)
	}
	restore.Release()
	if err := backup.Add(filepath.Join(docs, "x"), false); err != nil {
		t.Errorf("after the release: %v", err)
	}
	if err := restore.Add(docs, false); err == nil {
		t.Error("Add after Release succeeded")
	}
	if err := backup.Add("relative", false); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("Add(relative) = %v", err)
	}
}
