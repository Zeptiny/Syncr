//go:build integration

package restic

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

const itProbeID = "0195f0a4-3b7c-7d1e-9f00-0000000000bb"

// treePath is the snapshot tree path of a local path: forward slashes, and
// on Windows the drive letter as the first folder ("/C/Users/...").
func treePath(local string) string {
	vol := filepath.VolumeName(local)
	rest := filepath.ToSlash(local[len(vol):])
	if vol != "" {
		return "/" + strings.TrimSuffix(vol, ":") + rest
	}
	return rest
}

// awkwardName is a file name with characters that are special in restic's
// include patterns, as far as the OS allows them in a file name.
func awkwardName() string {
	if runtime.GOOS == "windows" {
		return "[x] a.txt"
	}
	return "[x]*?.txt"
}

func entryNames(l *syncrv1.ProbeListing) []string {
	names := make([]string, len(l.GetEntries()))
	for i, e := range l.GetEntries() {
		names[i] = e.GetName()
	}
	return names
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// TestIntegrationProbesAndRestore runs the probe operations and the
// Milestone 5 restore options against the real tools: init and config
// probes, a snapshot listing with summaries, browsing a snapshot one folder
// at a time, restoring selected items and one folder's contents, and the
// overwrite behaviors.
func TestIntegrationProbesAndRestore(t *testing.T) {
	h := newHarness(t)
	p := h.prober()
	ctx := t.Context()
	repo := h.repo(itPassword)

	// Probes on a location without a repository.
	_, err := p.Config(ctx, itProbeID, h.remotes(), &syncrv1.ProbeResticConfig{Repository: repo})
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_NOT_FOUND)

	created, err := p.Init(ctx, itProbeID, h.remotes(), &syncrv1.ProbeResticInit{Repository: repo})
	if err != nil {
		t.Fatalf("init probe: %v", err)
	}
	if !resticIDPattern.MatchString(created.GetId()) || created.GetVersion() != 2 {
		t.Fatalf("config after init = %v", created)
	}
	_, err = p.Init(ctx, itProbeID, h.remotes(), &syncrv1.ProbeResticInit{Repository: repo})
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_ALREADY_EXISTS)

	cfg, err := p.Config(ctx, itProbeID, h.remotes(), &syncrv1.ProbeResticConfig{Repository: repo})
	if err != nil || cfg.GetId() != created.GetId() {
		t.Fatalf("config probe = %v, %v; want ID %s", cfg, err, created.GetId())
	}
	wrong := "definitely-not-the-password"
	_, err = p.Config(ctx, itProbeID, h.remotes(), &syncrv1.ProbeResticConfig{Repository: h.repo(wrong)})
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_WRONG_PASSWORD)
	if strings.Contains(err.Error(), wrong) {
		t.Errorf("error leaks the password: %v", err)
	}

	empty, err := p.Snapshots(ctx, itProbeID, h.remotes(), &syncrv1.ProbeResticSnapshots{Repository: repo})
	if err != nil || len(empty.GetSnapshots()) != 0 {
		t.Fatalf("snapshots of a new repository = %v, %v", empty, err)
	}

	// Two backups of a tree with awkward names.
	src := h.local("data")
	writeFile(t, filepath.Join(src, "a b.txt"), []byte("one\n"))
	writeFile(t, filepath.Join(src, awkwardName()), []byte("awkward\n"))
	writeFile(t, filepath.Join(src, "ü.txt"), []byte("umlaut\n"))
	writeFile(t, filepath.Join(src, "docs", "d.txt"), []byte("doc\n"))
	writeFile(t, filepath.Join(src, "docs", "deep", "e.txt"), []byte("deep\n"))
	res, _ := h.backup(&syncrv1.ResticBackup{Paths: []string{src}, Host: "it-host", Tags: []string{"m5"}})
	first := res.Output.GetResticBackup()
	if first.GetFilesNew() != 5 || first.GetDirsNew() == 0 || first.GetTotalFilesProcessed() != 5 ||
		first.GetTotalBytesProcessed() == 0 || first.GetDataAddedPackedBytes() == 0 {
		t.Errorf("first backup output = %v", first)
	}
	writeFile(t, filepath.Join(src, "a b.txt"), []byte("one, changed\n"))
	res, _ = h.backup(&syncrv1.ResticBackup{Paths: []string{src}, Host: "it-host"})
	second := res.Output.GetResticBackup()
	if second.GetFilesChanged() != 1 || second.GetFilesUnmodified() != 4 {
		t.Errorf("second backup output = %v", second)
	}

	// The snapshot listing carries parents, summaries and the user.
	list, err := p.Snapshots(ctx, itProbeID, h.remotes(), &syncrv1.ProbeResticSnapshots{Repository: repo})
	if err != nil || len(list.GetSnapshots()) != 2 || list.GetTruncated() {
		t.Fatalf("snapshots = %v, %v", list, err)
	}
	old, latest := list.GetSnapshots()[0], list.GetSnapshots()[1]
	if latest.GetId() != second.GetSnapshotId() || latest.GetParent() != old.GetId() || latest.GetHost() != "it-host" ||
		latest.GetSummary().GetFilesChanged() != 1 || latest.GetSummary().GetTotalFilesProcessed() != 5 ||
		latest.GetProgramVersion() == "" || !slices.Equal(old.GetTags(), []string{"m5"}) {
		t.Errorf("snapshots = %v", list)
	}
	newest, err := p.Snapshots(ctx, itProbeID, h.remotes(), &syncrv1.ProbeResticSnapshots{Repository: repo, MaxSnapshots: 1})
	if err != nil || len(newest.GetSnapshots()) != 1 || !newest.GetTruncated() || newest.GetSnapshots()[0].GetId() != latest.GetId() {
		t.Errorf("newest snapshot only = %v, %v", newest, err)
	}

	// Browse from the root down to the source folder.
	tree := treePath(src)
	ls := func(path string, limit int32) (*syncrv1.ProbeListing, error) {
		return p.Ls(ctx, itProbeID, h.remotes(), &syncrv1.ProbeResticLs{Repository: repo, SnapshotId: latest.GetId(), Path: path, MaxEntries: limit})
	}
	at := "/"
	for _, seg := range strings.Split(strings.Trim(tree, "/"), "/") {
		l, err := ls(at, 0)
		if err != nil {
			t.Fatalf("ls %s: %v", at, err)
		}
		if !slices.Contains(entryNames(l), seg) {
			t.Fatalf("ls %s = %v, want it to list %q", at, entryNames(l), seg)
		}
		at = strings.TrimSuffix(at, "/") + "/" + seg
	}
	l, err := ls(tree, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Directories first, then by name.
	if want := []string{"docs", awkwardName(), "a b.txt", "ü.txt"}; !slices.Equal(entryNames(l), want) || l.GetPath() != tree {
		t.Errorf("ls %s = %v (path %q), want %v", tree, entryNames(l), l.GetPath(), want)
	}
	if e := l.GetEntries()[2]; e.GetPath() != tree+"/a b.txt" || e.GetSize() != int64(len("one, changed\n")) ||
		e.GetType() != syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_FILE || e.GetModTime() == nil {
		t.Errorf("entry = %v", e)
	}
	if cut, err := ls(tree, 2); err != nil || len(cut.GetEntries()) != 2 || !cut.GetTruncated() {
		t.Errorf("limited listing = %v, %v", cut, err)
	}
	_, err = ls(tree+"/nope", 0)
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND)
	_, err = p.Ls(ctx, itProbeID, h.remotes(), &syncrv1.ProbeResticLs{Repository: repo, SnapshotId: "deadbeef", Path: "/"})
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND)

	restore := func(op *syncrv1.ResticRestore) *syncrv1.ResticRestoreOutput {
		t.Helper()
		op.Repository = repo
		op.SnapshotId = latest.GetId()
		res, _ := h.mustRun(func(s *syncrv1.JobSpec) { s.Operation = &syncrv1.JobSpec_ResticRestore{ResticRestore: op} })
		out := res.Output.GetResticRestore()
		if out == nil {
			t.Fatal("restore has no output")
		}
		return out
	}

	// Selected items: a folder with its contents and a file with an awkward
	// name, given as literal tree paths.
	r2 := h.local("r2")
	out := restore(&syncrv1.ResticRestore{Target: r2, Paths: []string{tree + "/docs", tree + "/" + awkwardName()}})
	got2 := restoredPath(r2, src)
	for _, f := range []string{filepath.Join("docs", "d.txt"), filepath.Join("docs", "deep", "e.txt"), awkwardName()} {
		if !exists(filepath.Join(got2, f)) {
			t.Errorf("selected item %s was not restored", f)
		}
	}
	for _, f := range []string{"a b.txt", "ü.txt"} {
		if exists(filepath.Join(got2, f)) {
			t.Errorf("%s was restored but not selected", f)
		}
	}
	// restic counts folders among the files; the bytes are those of the
	// three selected files.
	if want := int64(len("doc\n") + len("deep\n") + len("awkward\n")); out.GetFilesRestored() == 0 || out.GetTotalBytes() != want || out.GetBytesRestored() != want {
		t.Errorf("selected restore output = %v, want %d bytes in the selection", out, want)
	}

	// One folder's contents directly into the target; existing files are
	// kept by NEVER and replaced by IF_CHANGED.
	r3 := h.local("r3")
	writeFile(t, filepath.Join(r3, "a b.txt"), []byte("local edit\n"))
	out = restore(&syncrv1.ResticRestore{Target: r3, Subfolder: tree, Overwrite: syncrv1.ResticRestoreOverwrite_RESTIC_RESTORE_OVERWRITE_NEVER})
	if got := mustRead(t, filepath.Join(r3, "a b.txt")); got != "local edit\n" {
		t.Errorf("overwrite=never replaced an existing file: %q", got)
	}
	if got := mustRead(t, filepath.Join(r3, "docs", "deep", "e.txt")); got != "deep\n" {
		t.Errorf("subfolder restore: docs/deep/e.txt = %q", got)
	}
	if out.GetFilesSkipped() != 1 || out.GetBytesSkipped() == 0 {
		t.Errorf("overwrite=never output = %v, want 1 skipped file", out)
	}
	restore(&syncrv1.ResticRestore{Target: r3, Subfolder: tree, Overwrite: syncrv1.ResticRestoreOverwrite_RESTIC_RESTORE_OVERWRITE_IF_CHANGED})
	if got := mustRead(t, filepath.Join(r3, "a b.txt")); got != "one, changed\n" {
		t.Errorf("overwrite=if-changed kept a changed file: %q", got)
	}
	_, _, err = h.run(ctx, func(s *syncrv1.JobSpec) {
		s.Operation = &syncrv1.JobSpec_ResticRestore{ResticRestore: &syncrv1.ResticRestore{
			Repository: repo, SnapshotId: latest.GetId(), Target: h.local("r4"), Subfolder: "/nope",
		}}
	})
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND)

	// Retention with keep_within keeps both snapshots and counts them.
	res, _ = h.mustRun(func(s *syncrv1.JobSpec) {
		s.Operation = &syncrv1.JobSpec_ResticForget{ResticForget: &syncrv1.ResticForget{
			Repository: repo, Policy: &syncrv1.ResticRetentionPolicy{KeepWithin: "1y"},
		}}
	})
	if f := res.Output.GetResticForget(); f.GetKeptCount() != 2 || len(f.GetRemovedSnapshotIds()) != 0 {
		t.Errorf("forget output = %v, want 2 kept", f)
	}

	// A full check reports its summary.
	res, _ = h.mustRun(func(s *syncrv1.JobSpec) {
		s.Operation = &syncrv1.JobSpec_ResticCheck{ResticCheck: &syncrv1.ResticCheck{Repository: repo, ReadDataPercent: 100}}
	})
	if c := res.Output.GetResticCheck(); c == nil || c.GetNumErrors() != 0 || c.GetReadDataPercent() != 100 {
		t.Errorf("check output = %v", c)
	}
}

// deleteData runs restic_delete_data on repo.
func (h *harness) deleteData(repo *syncrv1.ResticRepository, expectedID string, removeDir bool) (runner.Result, []runner.Progress, error) {
	h.t.Helper()
	return h.run(h.t.Context(), func(s *syncrv1.JobSpec) {
		s.Operation = &syncrv1.JobSpec_ResticDeleteData{ResticDeleteData: &syncrv1.ResticDeleteData{
			Repository: repo, ExpectedResticId: expectedID, RemoveDirectory: removeDir,
		}}
	})
}

// newRepository initializes a repository with one backup and returns its
// restic ID.
func (h *harness) newRepository(repo *syncrv1.ResticRepository) string {
	h.t.Helper()
	cfg, err := h.prober().Init(h.t.Context(), itProbeID, h.remotes(), &syncrv1.ProbeResticInit{Repository: repo})
	if err != nil {
		h.t.Fatalf("init %v: %v", repo.GetLocation(), err)
	}
	writeFile(h.t, h.local("data", "a.txt"), []byte("a"))
	h.mustRun(func(s *syncrv1.JobSpec) {
		s.Operation = &syncrv1.JobSpec_ResticBackup{ResticBackup: &syncrv1.ResticBackup{Repository: repo, Paths: []string{h.local("data")}}}
	})
	return cfg.GetId()
}

// resticLayout are the entries of a restic repository, the config first.
func resticLayout() []string {
	return []string{"config", "keys", "data", "index", "snapshots", "locks"}
}

// runDeleteDataOnDisk runs the deletion scenarios on repositories whose
// files are in directories of the test: repoAt returns the repository for a
// directory.
func runDeleteDataOnDisk(t *testing.T, h *harness, base string, repoAt func(dir string) *syncrv1.ResticRepository) {
	t.Helper()

	t.Run("shared folder", func(t *testing.T) {
		dir := filepath.Join(base, "shared", "repo")
		repo := repoAt(dir)
		id := h.newRepository(repo)
		writeFile(t, filepath.Join(dir, "README.txt"), []byte("not restic's"))
		writeFile(t, filepath.Join(base, "shared", "notes.txt"), []byte("next to it"))

		res, snaps, err := h.deleteData(repo, id, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, part := range resticLayout() {
			if exists(filepath.Join(dir, part)) {
				t.Errorf("%s survived the deletion", part)
			}
		}
		if !exists(filepath.Join(dir, "README.txt")) || !exists(filepath.Join(base, "shared", "notes.txt")) {
			t.Error("an unrelated file was deleted")
		}
		out := res.Output.GetResticDeleteData()
		if out.GetDirectoryRemoved() || out.GetAlreadyGone() || out.GetDeletedObjects() < 4 ||
			!slices.Equal(out.GetRemovedParts(), []string{"locks", "snapshots", "index", "data", "keys", "config"}) {
			t.Errorf("output = %v", out)
		}
		if res.Stats.Deletes != out.GetDeletedObjects() || len(snaps) == 0 {
			t.Errorf("stats = %+v with %d progress snapshots", res.Stats, len(snaps))
		}

		// Running again finds nothing left.
		res, _, err = h.deleteData(repo, id, true)
		if err != nil || !res.Output.GetResticDeleteData().GetAlreadyGone() {
			t.Errorf("second run = %v, %v; want already_gone", res.Output, err)
		}
		if !exists(filepath.Join(dir, "README.txt")) {
			t.Error("the second run deleted an unrelated file")
		}
	})

	t.Run("own folder", func(t *testing.T) {
		dir := filepath.Join(base, "solo", "repo")
		repo := repoAt(dir)
		id := h.newRepository(repo)
		res, _, err := h.deleteData(repo, id, true)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Output.GetResticDeleteData().GetDirectoryRemoved() || exists(dir) {
			t.Errorf("output = %v; the emptied folder still exists: %v", res.Output, exists(dir))
		}
		if !exists(filepath.Join(base, "solo")) {
			t.Error("the parent folder was removed")
		}
	})

	t.Run("kept folder", func(t *testing.T) {
		dir := filepath.Join(base, "kept", "repo")
		repo := repoAt(dir)
		id := h.newRepository(repo)
		res, _, err := h.deleteData(repo, id, false)
		if err != nil {
			t.Fatal(err)
		}
		if res.Output.GetResticDeleteData().GetDirectoryRemoved() || !exists(dir) {
			t.Errorf("the folder was removed without remove_directory: %v", res.Output)
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		dirA, dirB := filepath.Join(base, "mismatch", "a"), filepath.Join(base, "mismatch", "b")
		repoA, repoB := repoAt(dirA), repoAt(dirB)
		h.newRepository(repoA)
		idB := h.newRepository(repoB)
		_, _, err := h.deleteData(repoA, idB, true)
		mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_MISMATCH)
		for _, part := range resticLayout()[:5] {
			if !exists(filepath.Join(dirA, part)) {
				t.Errorf("%s was deleted from a mismatching repository", part)
			}
		}
	})

	t.Run("wrong password", func(t *testing.T) {
		dir := filepath.Join(base, "wrongpw", "repo")
		id := h.newRepository(repoAt(dir))
		bad := repoAt(dir)
		bad.Password = "definitely-not-the-password"
		_, _, err := h.deleteData(bad, id, true)
		mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_WRONG_PASSWORD)
		if !exists(filepath.Join(dir, "config")) || !exists(filepath.Join(dir, "data")) {
			t.Error("files were deleted with a wrong password")
		}
	})

	t.Run("resume", func(t *testing.T) {
		// An earlier attempt stopped between keys/ and config.
		dir := filepath.Join(base, "resume", "repo")
		repo := repoAt(dir)
		id := h.newRepository(repo)
		for _, part := range resticLayout()[1:] {
			if err := os.RemoveAll(filepath.Join(dir, part)); err != nil {
				t.Fatal(err)
			}
		}
		res, _, err := h.deleteData(repo, id, true)
		if err != nil {
			t.Fatal(err)
		}
		out := res.Output.GetResticDeleteData()
		if !slices.Equal(out.GetRemovedParts(), []string{"config"}) || !out.GetDirectoryRemoved() || exists(dir) {
			t.Errorf("output = %v (folder exists: %v), want only the config deleted and the folder removed", out, exists(dir))
		}
	})

	t.Run("config missing but data remains", func(t *testing.T) {
		dir := filepath.Join(base, "broken", "repo")
		repo := repoAt(dir)
		id := h.newRepository(repo)
		if err := os.Remove(filepath.Join(dir, "config")); err != nil {
			t.Fatal(err)
		}
		_, _, err := h.deleteData(repo, id, true)
		mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_NOT_FOUND)
		if !exists(filepath.Join(dir, "data")) {
			t.Error("data was deleted although the repository could not be verified")
		}
	})
}

// TestIntegrationDeleteData deletes repositories with the real restic and
// rclone, on a local rclone remote and on the worker's own filesystem (the
// variant that also runs on Windows CI).
func TestIntegrationDeleteData(t *testing.T) {
	t.Run("rclone remote", func(t *testing.T) {
		h := newHarness(t)
		runDeleteDataOnDisk(t, h, h.repoDir, func(dir string) *syncrv1.ResticRepository {
			return &syncrv1.ResticRepository{Location: &syncrv1.Location{Remote: "repo", Path: dir}, Password: itPassword}
		})
	})
	t.Run("local path", func(t *testing.T) {
		h := newHarness(t)
		h.defs = nil
		runDeleteDataOnDisk(t, h, h.local("repos"), func(dir string) *syncrv1.ResticRepository {
			return &syncrv1.ResticRepository{Location: &syncrv1.Location{Path: dir}, Password: itPassword}
		})
	})
}

// TestIntegrationDeleteDataRefusesALockedRepository: a lock another restic
// client holds (here a running backup) stops the deletion before anything
// is deleted.
func TestIntegrationDeleteDataRefusesALockedRepository(t *testing.T) {
	h := newHarness(t, "--limit-upload=500") // KiB/s: the backup below takes seconds
	repo := h.repo(itPassword)
	id := h.newRepository(repo)
	src := h.local("big")
	writeFile(t, filepath.Join(src, "big.bin"), randomBytes(t, 4_000_000))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		spec := &syncrv1.JobSpec{Remotes: h.remotes(), Operation: &syncrv1.JobSpec_ResticBackup{ResticBackup: &syncrv1.ResticBackup{
			Repository: repo, Paths: []string{src},
		}}}
		var once sync.Once
		_, err := h.runners[kindBackup].Run(ctx, runner.Job{ID: "it-lock-holder", Spec: spec}, func(runner.Progress) {
			once.Do(func() { close(started) })
		})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("backup did not start")
	}

	_, _, err := h.deleteData(repo, id, true)
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_LOCKED)
	if !strings.Contains(err.Error(), "nothing was deleted") {
		t.Errorf("message = %q", err)
	}
	if !exists(filepath.Join(h.repoDir, "config")) || !exists(filepath.Join(h.repoDir, "data")) {
		t.Error("files were deleted from a locked repository")
	}
	cancel()
	if err := <-done; !runner.Canceled(err) {
		t.Errorf("backup err = %v, want cancellation", err)
	}
}
