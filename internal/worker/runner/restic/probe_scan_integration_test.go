//go:build integration

package restic

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// escapeGlob escapes a literal name for a restic pattern with character
// classes, which work on Windows, where "\" separates folders.
func escapeGlob(s string) string {
	r := strings.NewReplacer("[", "[[]", "*", "[*]", "?", "[?]")
	return r.Replace(s)
}

// TestIntegrationScans runs the Milestone 7 probe operations against the
// real restic: two backups with changes in between, then a folder
// measurement, searches across both snapshots (two versions of a changed
// file) and comparisons of the whole tree and of one folder.
func TestIntegrationScans(t *testing.T) {
	h := newHarness(t)
	p := h.prober()
	ctx := t.Context()
	repo := h.repo(itPassword)
	h.init(itPassword)

	src := h.local("data")
	writeFile(t, filepath.Join(src, "a.txt"), []byte("one\n"))
	writeFile(t, filepath.Join(src, "b.txt"), []byte("bee\n"))
	writeFile(t, filepath.Join(src, awkwardName()), []byte("awkward\n"))
	writeFile(t, filepath.Join(src, "docs", "d.txt"), []byte("doc\n"))
	writeFile(t, filepath.Join(src, "docs", "deep", "e.txt"), []byte("deep\n"))
	writeFile(t, filepath.Join(src, "photos", "p.jpg"), randomBytes(t, 3000))
	writeFile(t, filepath.Join(src, "kind"), []byte("a file\n"))
	res, _ := h.backup(&syncrv1.ResticBackup{Paths: []string{src}, Host: "it-host"})
	first := res.Output.GetResticBackup().GetSnapshotId()
	writeFile(t, filepath.Join(src, "a.txt"), []byte("one, changed\n"))
	writeFile(t, filepath.Join(src, "docs", "new.txt"), []byte("new\n"))
	if err := os.Remove(filepath.Join(src, "b.txt")); err != nil {
		t.Fatal(err)
	}
	// A file that becomes a folder of the same name: a type change.
	if err := os.Remove(filepath.Join(src, "kind")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(src, "kind", "inner.txt"), []byte("inner\n"))
	res, _ = h.backup(&syncrv1.ResticBackup{Paths: []string{src}, Host: "it-host"})
	second := res.Output.GetResticBackup().GetSnapshotId()
	tree := treePath(src)

	// Measure the source folder of the first snapshot, and the root.
	m, err := p.Measure(ctx, itProbeID, h.remotes(), &syncrv1.ProbeResticMeasure{Repository: repo, SnapshotId: first, Path: tree}, 10)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	wantBytes := int64(len("one\n") + len("bee\n") + len("awkward\n") + len("doc\n") + len("deep\n") + 3000 + len("a file\n"))
	if m.GetPath() != tree || m.GetBytes() != wantBytes || m.GetFiles() != 7 || m.GetDirs() != 3 || !m.GetComplete() {
		t.Errorf("measurement = %v", m)
	}
	var children []string
	for _, c := range m.GetChildren() {
		children = append(children, fmt.Sprintf("%s %d %d %d", c.GetName(), c.GetBytes(), c.GetFiles(), c.GetDirs()))
	}
	if want := []string{"photos 3000 1 0", "docs 9 2 1"}; !slices.Equal(children, want) {
		t.Errorf("children = %q, want %q", children, want)
	}
	root, err := p.Measure(ctx, itProbeID, h.remotes(), &syncrv1.ProbeResticMeasure{Repository: repo, SnapshotId: first, Path: "/"}, 10)
	if err != nil || root.GetBytes() != wantBytes || root.GetFiles() != 7 || len(root.GetChildren()) != 1 {
		t.Errorf("root measurement = %v, %v", root, err)
	}
	_, err = p.Measure(ctx, itProbeID, h.remotes(), &syncrv1.ProbeResticMeasure{Repository: repo, SnapshotId: first, Path: tree + "/nope"}, 10)
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND)

	// Search: two versions of a.txt, newest first.
	find := func(op *syncrv1.ProbeResticFind) *syncrv1.ResticFindOutput {
		t.Helper()
		op.Repository = repo
		f, err := p.Find(ctx, itProbeID, h.remotes(), op)
		if err != nil {
			t.Fatalf("find %q: %v", op.GetPatterns(), err)
		}
		return f
	}
	f := find(&syncrv1.ProbeResticFind{Patterns: []string{"a.txt"}})
	if len(f.GetMatches()) != 2 || !f.GetComplete() || f.GetTruncated() || f.GetSnapshotsSearched() != 2 {
		t.Fatalf("find a.txt = %v", f)
	}
	newer, older := f.GetMatches()[0], f.GetMatches()[1]
	if newer.GetSnapshotId() != second || older.GetSnapshotId() != first ||
		newer.GetEntry().GetSize() != int64(len("one, changed\n")) || older.GetEntry().GetSize() != int64(len("one\n")) ||
		newer.GetEntry().GetPath() != tree+"/a.txt" || newer.GetEntry().GetName() != "a.txt" ||
		newer.GetEntry().GetType() != syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_FILE || newer.GetEntry().GetModTime() == nil {
		t.Errorf("versions = %v, %v", newer, older)
	}
	// An anchored path; a pattern with "/" matched against trailing
	// folders; a literal name escaped with character classes.
	for _, pat := range []string{tree + "/a.txt", "data/a.txt", escapeGlob(tree + "/" + awkwardName())} {
		if f := find(&syncrv1.ProbeResticFind{Patterns: []string{pat}}); len(f.GetMatches()) != 2 {
			t.Errorf("find %q = %d matches, want 2", pat, len(f.GetMatches()))
		}
	}
	if f := find(&syncrv1.ProbeResticFind{Patterns: []string{"[[]x]*.txt"}}); len(f.GetMatches()) != 2 {
		t.Errorf("escaped bracket: %d matches", len(f.GetMatches()))
	}
	// Case: only with ignore_case.
	if f := find(&syncrv1.ProbeResticFind{Patterns: []string{"A.TXT"}}); len(f.GetMatches()) != 0 {
		t.Errorf("case-sensitive search = %v", matchLines(f))
	}
	if f := find(&syncrv1.ProbeResticFind{Patterns: []string{"A.TXT"}, IgnoreCase: true}); len(f.GetMatches()) != 2 {
		t.Errorf("case-insensitive search = %v", matchLines(f))
	}
	// A folder matches with everything below it.
	f = find(&syncrv1.ProbeResticFind{Patterns: []string{"docs"}, SnapshotIds: []string{first[:8]}})
	var paths []string
	for _, mt := range f.GetMatches() {
		paths = append(paths, strings.TrimPrefix(mt.GetEntry().GetPath(), tree))
	}
	if want := []string{"/docs", "/docs/d.txt", "/docs/deep", "/docs/deep/e.txt"}; !slices.Equal(paths, want) || f.GetSnapshotsSearched() != 1 ||
		f.GetMatches()[0].GetSnapshotId() != first || f.GetMatches()[0].GetEntry().GetType() != syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR {
		t.Errorf("find docs in the first snapshot = %q (searched %d)", paths, f.GetSnapshotsSearched())
	}
	// Removed in the second snapshot.
	if f := find(&syncrv1.ProbeResticFind{Patterns: []string{"b.txt"}}); len(f.GetMatches()) != 1 || f.GetMatches()[0].GetSnapshotId() != first || f.GetSnapshotsSearched() != 2 {
		t.Errorf("find b.txt = %v", f)
	}
	// The bound (restic may finish before it is stopped: complete or not).
	if f := find(&syncrv1.ProbeResticFind{Patterns: []string{"*.txt"}, MaxMatches: 2}); len(f.GetMatches()) != 2 || !f.GetTruncated() ||
		f.GetMatches()[1].GetSnapshotId() != second {
		t.Errorf("bounded find = %v", matchLines(f))
	}
	_, err = p.Find(ctx, itProbeID, h.remotes(), &syncrv1.ProbeResticFind{Repository: repo, Patterns: []string{"x"}, SnapshotIds: []string{"deadbeef"}})
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND)

	// Compare the snapshots, whole and one folder.
	d, err := p.Diff(ctx, itProbeID, h.remotes(), &syncrv1.ProbeResticDiff{Repository: repo, FromSnapshotId: first, ToSnapshotId: second})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	// Changes of metadata only (the folders above the changed files) are
	// not asked for; the file that became a folder is a type change.
	changes := changeLines(d)
	if want := []string{"CHANGED " + tree + "/a.txt", "REMOVED " + tree + "/b.txt", "ADDED " + tree + "/docs/new.txt", "TYPE " + tree + "/kind/"}; !slices.Equal(changes, want) {
		t.Errorf("diff = %q, want %q", changes, want)
	}
	if s := d.GetStats(); !d.GetComplete() || s.GetChangedFiles() != 1 || s.GetAddedFiles() != 1 || s.GetRemovedFiles() != 1 {
		t.Errorf("diff stats = %v, complete %v", s, d.GetComplete())
	}
	d, err = p.Diff(ctx, itProbeID, h.remotes(), &syncrv1.ProbeResticDiff{Repository: repo, FromSnapshotId: first, ToSnapshotId: second, Path: tree + "/docs"})
	if err != nil || !slices.Equal(changeLines(d), []string{"ADDED " + tree + "/docs/new.txt"}) || d.GetStats().GetAddedFiles() != 1 {
		t.Errorf("folder diff = %q, %v, %v", changeLines(d), d.GetStats(), err)
	}
	_, err = p.Diff(ctx, itProbeID, h.remotes(), &syncrv1.ProbeResticDiff{Repository: repo, FromSnapshotId: first, ToSnapshotId: second, Path: tree + "/nope"})
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND)
	_, err = p.Diff(ctx, itProbeID, h.remotes(), &syncrv1.ProbeResticDiff{Repository: repo, FromSnapshotId: first, ToSnapshotId: second, Path: tree + "/a.txt"})
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND)

	// A soft deadline that passes while restic runs (it is still opening
	// the repository after 50 ms): restic is stopped, and what was found
	// so far is returned without an error.
	for name, scan := range map[string]func(context.Context) (bool, error){
		"measure": func(c context.Context) (bool, error) {
			m, err := p.Measure(c, itProbeID, h.remotes(), &syncrv1.ProbeResticMeasure{Repository: repo, SnapshotId: first, Path: "/"}, 10)
			return m.GetComplete(), err
		},
		"find": func(c context.Context) (bool, error) {
			f, err := p.Find(c, itProbeID, h.remotes(), &syncrv1.ProbeResticFind{Repository: repo, Patterns: []string{"*"}})
			return f.GetComplete(), err
		},
		"diff": func(c context.Context) (bool, error) {
			d, err := p.Diff(c, itProbeID, h.remotes(), &syncrv1.ProbeResticDiff{Repository: repo, FromSnapshotId: first, ToSnapshotId: second})
			return d.GetComplete(), err
		},
	} {
		short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		start := time.Now()
		complete, err := scan(short)
		cancel()
		if err != nil || complete {
			t.Errorf("%s at a deadline: complete %v, %v", name, complete, err)
		}
		if took := time.Since(start); took > 50*time.Millisecond+2*scanGrace+time.Second {
			t.Errorf("%s: stopping restic took %v", name, took)
		}
	}
}
