package web

import (
	"io/fs"
	"strings"
	"testing"
)

func TestFSHasIndex(t *testing.T) {
	t.Parallel()
	b, err := fs.ReadFile(FS(), "index.html")
	if err != nil {
		t.Fatalf("index.html: %v", err)
	}
	if !strings.Contains(strings.ToLower(string(b)), "<html") {
		t.Errorf("index.html is not an HTML page: %q", b)
	}
}
