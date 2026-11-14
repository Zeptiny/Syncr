//go:build integration

package restic

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
	"github.com/Zeptiny/Syncr/internal/worker/sourcestate"
)

// Backups of a folder of a remote with the real rclone and restic: the
// mirror method. The remote "src" is a local folder (rclone's alias
// backend over an absolute path), or a bucket on MinIO; the repository is
// on the harness's local remote or on MinIO.

const itHost = "it-host"

// withSources rebuilds the harness's runners with a store of mirrors and
// mount points in base/data/sources and the given support.
func withSources(h *harness, support *syncrv1.RemoteSourceSupport, mounter Mounter) *sourcestate.Store {
	h.t.Helper()
	store, err := sourcestate.New(sourcestate.Config{Root: filepath.Join(filepath.Dir(h.root), "data", "sources")})
	if err != nil {
		h.t.Fatal(err)
	}
	h.cfg.RemoteSources = RemoteSources{Store: store, Mounter: mounter, Support: support, MountTimeout: 30 * time.Second}
	rs, err := New(h.cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, r := range rs {
		h.runners[r.Kind()] = r
	}
	return store
}

// localSource adds the remote "src", a local folder (rclone's alias
// backend), and returns that folder.
func localSource(h *harness) string {
	h.t.Helper()
	dir := filepath.Join(filepath.Dir(h.root), "remote")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatal(err)
	}
	h.defs = append(h.defs, &syncrv1.RemoteDefinition{Alias: "src", BackendType: "alias", Options: map[string]string{"remote": dir}})
	return dir
}

// remoteRun backs up the folder path of remote alias into the harness's
// repository, mirrored under sourceKey and tagged as the controller tags
// it, changed by mutate.
func (h *harness) remoteRun(ctx context.Context, alias, path string, emit runner.EmitFunc,
	mutate ...func(*syncrv1.ResticBackup, *syncrv1.ResticBackupRemoteSource),
) (runner.Result, error) {
	h.t.Helper()
	src := &syncrv1.ResticBackupRemoteSource{Remote: alias, Path: path, Method: mirrorMethod, SourceKey: sourceKey}
	b := &syncrv1.ResticBackup{Repository: h.repo(itPassword), Tags: []string{remoteTag}, Host: itHost, RemoteSource: src}
	for _, m := range mutate {
		m(b, src)
	}
	spec := &syncrv1.JobSpec{Remotes: h.remotes(), Operation: &syncrv1.JobSpec_ResticBackup{ResticBackup: b}}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if emit == nil {
		emit = func(runner.Progress) {}
	}
	return h.runners[kindBackup].Run(ctx, runner.Job{ID: "it-remote", Spec: spec}, emit)
}

func (h *harness) mustRemoteRun(alias, path string, mutate ...func(*syncrv1.ResticBackup, *syncrv1.ResticBackupRemoteSource)) *syncrv1.ResticBackupOutput {
	h.t.Helper()
	res, err := h.remoteRun(h.t.Context(), alias, path, nil, mutate...)
	if err != nil {
		h.t.Fatalf("backup of %s:%s failed: %v", alias, path, err)
	}
	return res.Output.GetResticBackup()
}

// tree lists a snapshot's folder (a tree path) by name, folders with a
// trailing "/".
func (h *harness) tree(snapshot, path string) []string {
	h.t.Helper()
	l, err := h.prober().Ls(h.t.Context(), "it-ls", h.remotes(), &syncrv1.ProbeResticLs{Repository: h.repo(itPassword), SnapshotId: snapshot, Path: path})
	if err != nil {
		h.t.Fatalf("ls %s %s: %v", snapshot, path, err)
	}
	var names []string
	for _, e := range l.GetEntries() {
		n := e.GetName()
		if e.GetType() == syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR {
			n += "/"
		}
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

func excludeRule(pattern string) *syncrv1.FilterRule {
	return &syncrv1.FilterRule{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: pattern}
}

// mirrored lists the files of the mirror's copy, relative to it.
func mirrored(t *testing.T, store *sourcestate.Store) []string {
	t.Helper()
	data := filepath.Join(store.MirrorsDir(), sourceKey, sourcestate.DataDirName)
	var out []string
	err := filepath.WalkDir(data, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(data, path)
		out = append(out, filepath.ToSlash(rel))
		return err
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// TestIntegrationRemoteSourceMirror backs up a local folder through a
// mirror: the snapshot's tree starts at the folder's contents and carries
// the tag; unchanged files are unmodified the next time; deletions and
// filter changes reach the mirror (--delete-excluded); an empty folder
// leaves the mirror alone; a missing one is NOT_FOUND.
func TestIntegrationRemoteSourceMirror(t *testing.T) {
	h := newHarness(t)
	remote := localSource(h)
	store := withSources(h, &syncrv1.RemoteSourceSupport{Mirror: true}, nil)
	h.init(itPassword)
	photos := filepath.Join(remote, "photos")
	writeFile(t, filepath.Join(photos, "a.jpg"), randomBytes(t, 3000))
	writeFile(t, filepath.Join(photos, "2024", "b.jpg"), randomBytes(t, 2000))
	writeFile(t, filepath.Join(photos, "c.tmp"), []byte("scratch"))
	writeFile(t, filepath.Join(photos, "d.log"), []byte("log"))
	if err := os.Mkdir(filepath.Join(photos, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	skipTmp := func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) {
		s.Filters = []*syncrv1.FilterRule{excludeRule("*.tmp")}
	}

	// 1. The first run mirrors everything the filters keep.
	var phases []string
	res, err := h.remoteRun(t.Context(), "src", "photos", func(p runner.Progress) {
		if len(phases) == 0 || phases[len(phases)-1] != p.Phase {
			phases = append(phases, p.Phase)
		}
	}, skipTmp)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(phases, []string{"mirror", "backup"}) {
		t.Errorf("phases = %q", phases)
	}
	out := res.Output.GetResticBackup()
	rs := out.GetRemoteSource()
	if out.GetSnapshotId() == "" || out.GetFilesNew() != 3 || rs.GetMirroredFiles() != 3 || rs.GetMirroredBytes() != 5003 || rs.GetDeletedFiles() != 0 {
		t.Errorf("first run = %v", out)
	}
	if got, want := mirrored(t, store), []string{"2024/b.jpg", "a.jpg", "d.log"}; !slices.Equal(got, want) {
		t.Errorf("mirror = %q, want %q", got, want)
	}
	if got, want := h.tree(out.GetSnapshotId(), "/"), []string{"2024/", "a.jpg", "d.log", "empty/"}; !slices.Equal(got, want) {
		t.Errorf("snapshot tree = %q, want %q (the folder's contents, the empty folder too)", got, want)
	}
	snaps := h.snapshots(&syncrv1.ResticSnapshots{Tags: []string{remoteTag}})
	if len(snaps) != 1 || snaps[0].GetHost() != itHost || !slices.Contains(snaps[0].GetTags(), remoteTag) ||
		len(snaps[0].GetPaths()) != 1 || snaps[0].GetPaths()[0] != filepath.Join(store.MirrorsDir(), sourceKey, "data") {
		t.Errorf("snapshots = %v", snaps)
	}

	// 2. Nothing changed: nothing downloaded, every file unmodified.
	out = h.mustRemoteRun("src", "photos", skipTmp)
	if out.GetFilesUnmodified() != 3 || out.GetFilesNew() != 0 || out.GetFilesChanged() != 0 || out.GetRemoteSource().GetMirroredFiles() != 0 {
		t.Errorf("second run = %v", out)
	}

	// 3. A deletion, and a filter that now leaves the logs out.
	if err := os.Remove(filepath.Join(photos, "a.jpg")); err != nil {
		t.Fatal(err)
	}
	out = h.mustRemoteRun("src", "photos", func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) {
		s.Filters = []*syncrv1.FilterRule{excludeRule("*.tmp"), excludeRule("*.log")}
	})
	if out.GetRemoteSource().GetDeletedFiles() != 2 || out.GetFilesUnmodified() != 1 {
		t.Errorf("third run = %v", out)
	}
	if got := mirrored(t, store); !slices.Equal(got, []string{"2024/b.jpg"}) {
		t.Errorf("mirror after a deletion and a new filter = %q", got)
	}
	if got := h.tree(out.GetSnapshotId(), "/"); !slices.Equal(got, []string{"2024/", "empty/"}) {
		t.Errorf("snapshot tree = %q", got)
	}

	// 4. An emptied folder is refused and the mirror kept as it was.
	for _, name := range []string{"2024", "c.tmp", "d.log", "empty"} {
		if err := os.RemoveAll(filepath.Join(photos, name)); err != nil {
			t.Fatal(err)
		}
	}
	_, err = h.remoteRun(t.Context(), "src", "photos", nil, skipTmp)
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_SOURCE_EMPTY)
	if got := mirrored(t, store); !slices.Equal(got, []string{"2024/b.jpg"}) {
		t.Errorf("mirror after an empty folder = %q", got)
	}
	// allow_empty lets the run go on: the mirror follows the folder, but
	// restic 0.19.1 makes no snapshot of an empty folder ("snapshot is
	// empty"), so the job succeeds without one and says so.
	res, err = h.remoteRun(t.Context(), "src", "photos", nil, func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) { s.AllowEmpty = true })
	if err != nil {
		t.Fatal(err)
	}
	out = res.Output.GetResticBackup()
	if out.GetSnapshotId() != "" || out.GetRemoteSource().GetDeletedFiles() != 1 || len(mirrored(t, store)) != 0 ||
		len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "no snapshot was made") {
		t.Errorf("allowed empty run = %v, warnings %q", out, res.Warnings)
	}
	// A folder whose files the filters all leave out has nothing to back
	// up either.
	writeFile(t, filepath.Join(photos, "only.tmp"), []byte("x"))
	_, err = h.remoteRun(t.Context(), "src", "photos", nil, skipTmp)
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED)
	if !strings.Contains(err.Error(), "the filters leave out everything") {
		t.Errorf("error = %v", err)
	}

	// 5. A folder that does not exist.
	_, err = h.remoteRun(t.Context(), "src", "nope", nil)
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND)
	t.Logf("missing folder: %v", err)
}

// TestIntegrationRemoteSourceDiscardAndPrune: a discarded mirror is
// downloaded again, yet restic finds the files unmodified (--ignore-inode);
// an unused mirror is pruned.
func TestIntegrationRemoteSourceDiscardAndPrune(t *testing.T) {
	h := newHarness(t)
	remote := localSource(h)
	store := withSources(h, &syncrv1.RemoteSourceSupport{Mirror: true}, nil)
	h.init(itPassword)
	writeFile(t, filepath.Join(remote, "docs", "a.txt"), randomBytes(t, 1000))
	writeFile(t, filepath.Join(remote, "docs", "b", "c.txt"), randomBytes(t, 1000))
	discard := func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) { s.DiscardMirror = true }

	out := h.mustRemoteRun("src", "docs", discard)
	if !out.GetRemoteSource().GetMirrorDiscarded() || out.GetFilesNew() != 2 || len(mirrored(t, store)) != 0 {
		t.Errorf("first run = %v", out)
	}
	out = h.mustRemoteRun("src", "docs", discard)
	if out.GetRemoteSource().GetMirroredFiles() != 2 || out.GetFilesUnmodified() != 2 || out.GetFilesChanged() != 0 {
		t.Errorf("second run of a discarded mirror = %v", out)
	}

	// A kept mirror, unused long enough, is pruned.
	h.mustRemoteRun("src", "docs")
	if len(mirrored(t, store)) != 2 {
		t.Fatal("the mirror was not kept")
	}
	future, err := sourcestate.New(sourcestate.Config{Root: store.Root(), Now: func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	removed, err := future.Prune(t.Context(), 30*24*time.Hour)
	if err != nil || !slices.Equal(removed, []string{sourceKey}) {
		t.Errorf("Prune = %q, %v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(store.MirrorsDir(), sourceKey)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the mirror was kept: %v", err)
	}
	// The next run downloads it again.
	out = h.mustRemoteRun("src", "docs")
	if out.GetRemoteSource().GetMirroredFiles() != 2 || out.GetFilesUnmodified() != 2 {
		t.Errorf("run after pruning = %v", out)
	}
}

// TestIntegrationRemoteSourceCancelMidSync cancels a slow sync: the job
// ends as cancelled, no snapshot is made, and the mirror's lock is free.
func TestIntegrationRemoteSourceCancelMidSync(t *testing.T) {
	h := newHarness(t)
	remote := localSource(h)
	store := withSources(h, &syncrv1.RemoteSourceSupport{Mirror: true}, nil)
	h.init(itPassword)
	writeFile(t, filepath.Join(remote, "big", "file.bin"), randomBytes(t, 4<<20))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	start := time.Now()
	_, err := h.remoteRun(ctx, "src", "big", func(p runner.Progress) {
		if p.Phase == "mirror" && p.Bytes > 0 {
			cancel()
		}
	}, func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) {
		s.BandwidthLimitBytesPerSecond = ptr[int64](256 << 10) // 16 s for the file
	})
	if err == nil || !runner.Canceled(err) {
		t.Fatalf("Run = %v, want cancelled", err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("the cancel took %v", d)
	}
	if snaps := h.snapshots(&syncrv1.ResticSnapshots{}); len(snaps) != 0 {
		t.Errorf("snapshots after a cancelled sync = %v", snaps)
	}
	m, err := store.OpenMirror(sourceKey)
	if err != nil {
		t.Fatalf("the mirror stayed locked: %v", err)
	}
	_ = m.Close()
}

// TestIntegrationRemoteSourceMinIO backs up a folder of a bucket into a
// repository in another bucket of the same MinIO, twice, and checks what a
// missing bucket and a missing prefix are on S3.
func TestIntegrationRemoteSourceMinIO(t *testing.T) {
	h := newMinIOHarness(t)
	h.loc = &syncrv1.Location{Remote: "minio", Path: "syncr-rs-repo/repo"}
	store := withSources(h, &syncrv1.RemoteSourceSupport{Mirror: true}, nil)
	h.init(itPassword)
	src := h.local("upload")
	writeFile(t, filepath.Join(src, "a.jpg"), randomBytes(t, 4000))
	writeFile(t, filepath.Join(src, "2024", "b.jpg"), randomBytes(t, 3000))
	h.rclone("mkdir", "minio:syncr-rs-src")
	h.rclone("copy", src, "minio:syncr-rs-src/photos")

	out := h.mustRemoteRun("minio", "syncr-rs-src/photos")
	if out.GetFilesNew() != 2 || out.GetRemoteSource().GetMirroredFiles() != 2 || out.GetRemoteSource().GetMirroredBytes() != 7000 {
		t.Errorf("first run = %v", out)
	}
	if got := h.tree(out.GetSnapshotId(), "/"); !slices.Equal(got, []string{"2024/", "a.jpg"}) {
		t.Errorf("snapshot tree = %q", got)
	}
	out = h.mustRemoteRun("minio", "syncr-rs-src/photos")
	if out.GetFilesUnmodified() != 2 || out.GetRemoteSource().GetMirroredFiles() != 0 {
		t.Errorf("second run = %v", out)
	}
	if got := mirrored(t, store); !slices.Equal(got, []string{"2024/b.jpg", "a.jpg"}) {
		t.Errorf("mirror = %q", got)
	}

	// On S3 a bucket that does not exist is missing; a prefix without
	// objects in a bucket that exists is an empty folder.
	_, err := h.remoteRun(t.Context(), "minio", "syncr-rs-nobucket/photos", nil)
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND)
	_, err = h.remoteRun(t.Context(), "minio", "syncr-rs-src/nothing-here", nil)
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_SOURCE_EMPTY)
	if !strings.Contains(err.Error(), `"syncr-rs-src/nothing-here" on the remote is empty`) {
		t.Errorf("error = %v", err)
	}
}
