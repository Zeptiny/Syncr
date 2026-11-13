package tally

import (
	"fmt"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

func names(m *syncrv1.ProbeMeasurement) []string {
	out := make([]string, len(m.GetChildren()))
	for i, c := range m.GetChildren() {
		out[i] = c.GetName()
	}
	return out
}

func TestTally(t *testing.T) {
	t.Parallel()
	tl := New("bucket/data", 10)
	// rclone lists a folder's entries before its subfolders' and, on a
	// bucket, the folders it synthesizes last; the order does not matter.
	for _, e := range []struct {
		rel  string
		dir  bool
		size int64
	}{
		{"a.txt", false, 4},
		{"photos/2025/img1.jpg", false, 3000},
		{"empty", true, -1},
		{"docs", true, 80}, // a folder's own size is ignored
		{"docs/readme.md", false, 5},
		{"/docs/deep/", true, 0},
		{"docs/deep/e.txt", false, 5},
		{"photos", true, -1},
		{"photos/2025", true, -1},
		{"photos/2025/IMG2.JPG", false, 5000},
		{"unknown-size", false, -1},
		{"", true, 0}, // the folder itself
	} {
		tl.Add(e.rel, e.dir, e.size)
	}
	tl.Unreadable()
	m := tl.Result(true)
	if m.GetPath() != "bucket/data" || m.GetBytes() != 8014 || m.GetFiles() != 6 || m.GetDirs() != 5 ||
		!m.GetComplete() || m.GetUnreadable() != 1 || m.GetChildrenTruncated() {
		t.Errorf("measurement = %v", m)
	}
	// Largest first; an empty folder is a child too.
	if want := []string{"photos", "docs", "empty"}; !slices.Equal(names(m), want) {
		t.Fatalf("children = %v, want %v", names(m), want)
	}
	want := []*syncrv1.ProbeChildMeasurement{
		{Name: "photos", Bytes: 8000, Files: 2, Dirs: 1},
		{Name: "docs", Bytes: 10, Files: 2, Dirs: 1},
		{Name: "empty"},
	}
	for i, c := range m.GetChildren() {
		if !proto.Equal(c, want[i]) {
			t.Errorf("child %d = %v, want %v", i, c, want[i])
		}
	}
}

func TestTallyKeepsTheLargestChildren(t *testing.T) {
	t.Parallel()
	tl := New("/", 2)
	for i, size := range []int64{5, 50, 1, 500} {
		tl.Add(fmt.Sprintf("d%d/f", i), false, size)
	}
	tl.Add("tie-b/f", false, 50)
	m := tl.Result(false)
	if !slices.Equal(names(m), []string{"d3", "d1"}) || !m.GetChildrenTruncated() || m.GetComplete() || m.GetBytes() != 606 {
		t.Errorf("measurement = %v", m)
	}
	// Equal sizes: more files first, then by name.
	tl = New("/", 3)
	tl.Add("b/x", false, 10)
	tl.Add("a/x", false, 10)
	tl.Add("c/x", false, 5)
	tl.Add("c/y", false, 5)
	if got := names(tl.Result(true)); !slices.Equal(got, []string{"c", "a", "b"}) {
		t.Errorf("order = %v", got)
	}
	// No children wanted.
	if m := New("/", 0); len(m.Result(true).GetChildren()) != 0 {
		t.Error("children returned")
	}
}

func TestTallyBoundsTrackedChildren(t *testing.T) {
	t.Parallel()
	tl := New("/", MaxTracked+10)
	for i := range MaxTracked + 5 {
		tl.Add(fmt.Sprintf("d%06d/f", i), false, 1)
	}
	m := tl.Result(true)
	if len(m.GetChildren()) != MaxTracked || !m.GetChildrenTruncated() || m.GetFiles() != MaxTracked+5 || m.GetBytes() != MaxTracked+5 {
		t.Errorf("%d children, truncated=%v, %d files, %d bytes", len(m.GetChildren()), m.GetChildrenTruncated(), m.GetFiles(), m.GetBytes())
	}
}

func TestTallyMakesNamesValid(t *testing.T) {
	t.Parallel()
	tl := New("bad\xffpath", 5)
	tl.Add("bad\xffname/x", false, 1)
	m := tl.Result(true)
	if m.GetPath() != "bad�path" || m.GetChildren()[0].GetName() != "bad�name" {
		t.Errorf("measurement = %q, %q", m.GetPath(), m.GetChildren()[0].GetName())
	}
	if _, err := proto.Marshal(m); err != nil {
		t.Errorf("marshal: %v", err)
	}
}
