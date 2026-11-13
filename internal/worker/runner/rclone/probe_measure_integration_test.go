//go:build integration

package rclone

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// TestIntegrationMeasure measures a local folder tree with the real rclone,
// through a union remote of two folders below the local root (rclone
// refuses a union of one, and the local backend is not allowed as a
// remote).
func TestIntegrationMeasure(t *testing.T) {
	it := newITest(t)
	a, b := it.dir(t, "a"), it.dir(t, "b")
	writeFiles(t, a, map[string]int{"top.txt": 10, "photos/2025/img1.jpg": 3000, "photos/x.jpg": 1000, "docs/readme.md": 5})
	writeFiles(t, b, map[string]int{"docs/deep/e.txt": 7, "other/o.bin": 100})
	if err := os.MkdirAll(filepath.Join(a, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		// rclone does not list links without --links: not counted.
		if err := os.Symlink(filepath.Join(a, "top.txt"), filepath.Join(a, "link")); err != nil {
			t.Fatal(err)
		}
	}
	union := &syncrv1.RemoteDefinition{
		Alias: "both", BackendType: "union",
		UnionUpstreams: []*syncrv1.UnionUpstream{{Location: local(a)}, {Location: local(b)}},
	}
	p := it.prober(t)
	measure := func(ctx context.Context, path string, children int) (*syncrv1.ProbeMeasurement, error) {
		return p.Measure(ctx, "0195f0a4-3b7c-7d1e-9f00-0000000000e1", []*syncrv1.RemoteDefinition{union},
			&syncrv1.ProbeMeasureRemote{Location: &syncrv1.Location{Remote: "both", Path: path}}, children)
	}
	m, err := measure(t.Context(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if m.GetBytes() != 4122 || m.GetFiles() != 6 || m.GetDirs() != 6 || !m.GetComplete() || m.GetUnreadable() != 0 || m.GetChildrenTruncated() {
		t.Errorf("measurement = %v", m)
	}
	var children []string
	for _, c := range m.GetChildren() {
		children = append(children, fmt.Sprintf("%s %d %d %d", c.GetName(), c.GetBytes(), c.GetFiles(), c.GetDirs()))
	}
	if want := []string{"photos 4000 2 1", "other 100 1 0", "docs 12 2 1", "empty 0 0 0"}; !slices.Equal(children, want) {
		t.Errorf("children = %q, want %q", children, want)
	}
	if m, err := measure(t.Context(), "photos", 1); err != nil || m.GetBytes() != 4000 || len(m.GetChildren()) != 1 || m.GetChildrenTruncated() || m.GetPath() != "photos" {
		t.Errorf("photos = %v, %v", m, err)
	}
	_, err = measure(t.Context(), "nope", 10)
	mustFailCode(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND)

	// The soft deadline: rclone is stopped, and what it listed is returned
	// without an error.
	short, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	m, err = measure(short, "", 10)
	if err != nil || m.GetComplete() {
		t.Errorf("measurement at a deadline = %v, %v", m, err)
	}
	if took := time.Since(start); took > 2*scanGrace+time.Second {
		t.Errorf("stopping rclone took %v", took)
	}
	if left, _ := os.ReadDir(it.scratch); len(left) != 0 {
		t.Errorf("scratch entries left: %v", left)
	}
}
