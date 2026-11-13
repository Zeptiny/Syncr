//go:build integration

package restic

import (
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// TestIntegrationRestoreItemsOfSubfolder restores selected items of a
// subfolder with the real restic (Milestone 7, "restore to the original
// location"): "<id>:<subfolder>" with the items as includes relative to
// it puts exactly the items into the target as they were in the
// subfolder: nested items at several depths, awkward names, and neighbors
// whose names a careless pattern would also match stay out. The root as
// the subfolder restores the items by their whole tree path.
func TestIntegrationRestoreItemsOfSubfolder(t *testing.T) {
	h := newHarness(t)
	repo := h.repo(itPassword)
	h.newRepository(repo)

	src := h.local("data")
	awkward := awkwardName()
	for _, f := range []string{
		"f1.txt", "f2.txt", "dirA/x", "dirA/other", "dirA/sub/y", "dirA/sub/z",
		"dirB/z", awkward, "x.txt", "st.txt", "deep/er/est/leaf.txt", "deep/er/sibling.txt",
	} {
		writeFile(t, filepath.Join(src, filepath.FromSlash(f)), []byte(f+"\n"))
	}
	res, _ := h.backup(&syncrv1.ResticBackup{Paths: []string{src}, Host: "it-host"})
	id := res.Output.GetResticBackup().GetSnapshotId()
	if id == "" {
		t.Fatal("backup reported no snapshot ID")
	}
	tree := treePath(src)

	restore := func(target, sub string, paths ...string) []string {
		t.Helper()
		h.mustRun(func(s *syncrv1.JobSpec) {
			s.Operation = &syncrv1.JobSpec_ResticRestore{ResticRestore: &syncrv1.ResticRestore{
				Repository: repo, SnapshotId: id, Target: target, Subfolder: sub, Paths: paths,
				Overwrite: syncrv1.ResticRestoreOverwrite_RESTIC_RESTORE_OVERWRITE_NEVER,
			}}
		})
		return filesBelow(t, target)
	}

	// Items of the backed-up folder, back into a folder that stands for it.
	r1 := h.local("r1")
	got := restore(r1, tree,
		tree+"/dirA/sub/y", tree+"/dirA/x", tree+"/f2.txt", tree+"/"+awkward, tree+"/deep/er/est",
	)
	want := []string{"dirA/sub/y", "dirA/x", "deep/er/est/leaf.txt", "f2.txt", awkward}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("items of the subfolder restored %q, want %q", got, want)
	}
	if got := mustRead(t, filepath.Join(r1, "dirA", "sub", "y")); got != "dirA/sub/y\n" {
		t.Errorf("dirA/sub/y = %q", got)
	}

	// The root as the subfolder: the items keep their whole tree path.
	r2 := h.local("r2")
	got = restore(r2, "/", tree+"/dirB", tree+"/f1.txt")
	base := filepath.ToSlash(restoredPath("", src))
	if runtime.GOOS != "windows" {
		base = base[1:] // restoredPath("") keeps the leading slash of a POSIX path
	}
	want = []string{base + "/dirB/z", base + "/f1.txt"}
	if !slices.Equal(got, want) {
		t.Errorf("items of the root restored %q, want %q", got, want)
	}
}
