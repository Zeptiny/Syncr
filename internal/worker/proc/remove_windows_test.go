//go:build windows

package proc

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRemoveAllWaitsForAnOpenHandle holds a file open (Go opens files
// without FILE_SHARE_DELETE, as another process would) and releases it
// shortly after RemoveAll has started: the first attempt fails with a
// sharing violation, and a retry succeeds.
func TestRemoveAllWaitsForAnOpenHandle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "job")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "held"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); !transientRemoveError(err) {
		_ = f.Close()
		t.Fatalf("removing a directory with an open file: %v, want an error RemoveAll retries", err)
	}

	released := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = f.Close()
		close(released)
	}()
	err = RemoveAll(dir)
	<-released
	if err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat after RemoveAll: %v", err)
	}
}
