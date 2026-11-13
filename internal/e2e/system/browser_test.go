//go:build integration

package system

import (
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
)

// minioRemote stores an S3 remote on the MinIO at endpoint.
func minioRemote(c *controller, endpoint string) string {
	return c.createRemote(&syncrv1.RemoteInput{
		Name: "MinIO", BackendType: "s3",
		Options: map[string]string{
			"provider": "Minio", "endpoint": endpoint, "region": "us-east-1",
			"access_key_id": minioUser, "bucket": "syncr-e2e", "force_path_style": "true",
		},
		Secrets: map[string]string{"secret_access_key": minioPassword},
	})
}

// sized is content of n bytes.
func sized(n int) string { return strings.Repeat("x", n) }

// TestSystemBrowserScans runs the snapshot browser's scans with the real
// restic against MinIO: two backups with changes in between, then the
// folder sizes of a snapshot folder (and of the worker's folder and the
// repository's folder on the remote), a search that finds both versions of
// the changed file (by name, and by the anchored, escaped exact path the
// versions sheet sends), a comparison of the two snapshots (the whole tree
// and one folder), and a restore of selected items of a subfolder into
// their original place.
func TestSystemBrowserScans(t *testing.T) {
	t.Parallel()
	e := newResticEnv(t, false)
	w, ctx := e.w, t.Context()
	minio := minioRemote(e.c, startMinIO(t))
	repo, err := e.create("Vault", minio, "restic/browser", resticPassword, syncrv1.RepositoryCreateMode_REPOSITORY_CREATE_MODE_INIT)
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}

	// Two backups: the draft changes size, gone.txt goes and new.txt comes.
	const draft = "notes [draft]*.txt"
	src := filepath.Join(w.root, "src")
	for name, size := range map[string]int{
		"docs/one.txt": 100, "docs/deep/two.txt": 200, "photos/a.jpg": 1000, "photos/b.jpg": 2000, draft: 10, "gone.txt": 5,
	} {
		write(t, filepath.Join(src, name), sized(size))
	}
	e.run(backupJob(repo.GetId(), []string{src}, nil, nil))
	write(t, filepath.Join(src, draft), sized(25))
	if err := os.Remove(filepath.Join(src, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(src, "new.txt"), sized(7))
	e.run(backupJob(repo.GetId(), []string{src}, nil, nil))
	list := e.snapshots(repo.GetId()).GetSnapshots()
	if len(list) != 2 {
		t.Fatalf("snapshots = %v, want 2", list)
	}
	// The list is newest first.
	newer, older := list[0].GetId(), list[1].GetId()
	tree := filepath.ToSlash(src) // on Linux the tree path is the path

	// 1. Folder sizes of the newer snapshot's folder: totals, and the child
	// folders largest first.
	wantChildren := []string{"photos 3000 2 0", "docs 300 2 1"}
	children := func(m *syncrv1.ProbeMeasurement) []string {
		var out []string
		for _, ch := range m.GetChildren() {
			out = append(out, ch.GetName()+" "+strconv.FormatInt(ch.GetBytes(), 10)+" "+
				strconv.FormatInt(ch.GetFiles(), 10)+" "+strconv.FormatInt(ch.GetDirs(), 10))
		}
		return out
	}
	measured, err := e.restic.MeasureSnapshotFolder(ctx, connect.NewRequest(&syncrv1.MeasureSnapshotFolderRequest{
		RepositoryId: repo.GetId(), SnapshotId: newer, Path: tree,
	}))
	if err != nil {
		t.Fatalf("MeasureSnapshotFolder: %v", err)
	}
	e.note(measured.Msg)
	if m := measured.Msg.GetMeasurement(); m.GetBytes() != 3332 || m.GetFiles() != 6 || m.GetDirs() != 3 || !m.GetComplete() ||
		m.GetUnreadable() != 0 || m.GetChildrenTruncated() || !slices.Equal(children(m), wantChildren) || measured.Msg.GetWorker().GetId() != w.id {
		t.Errorf("MeasureSnapshotFolder = %v, children %q", measured.Msg, children(m))
	}
	// The worker's own folder holds what the newer snapshot holds.
	onWorker, err := e.probes.MeasureWorkerFolder(ctx, connect.NewRequest(&syncrv1.MeasureWorkerFolderRequest{WorkerId: w.id, Path: src}))
	if err != nil {
		t.Fatalf("MeasureWorkerFolder: %v", err)
	}
	if m := onWorker.Msg.GetMeasurement(); m.GetBytes() != 3332 || m.GetFiles() != 6 || m.GetDirs() != 3 || !m.GetComplete() ||
		!slices.Equal(children(m), wantChildren) {
		t.Errorf("MeasureWorkerFolder = %v, children %q", onWorker.Msg, children(m))
	}
	// The repository's folder on the remote: restic's own folders.
	remote, err := e.probes.MeasureRemoteFolder(ctx, connect.NewRequest(&syncrv1.MeasureRemoteFolderRequest{RemoteId: minio, Path: "restic/browser"}))
	if err != nil {
		t.Fatalf("MeasureRemoteFolder: %v", err)
	}
	var names []string
	for _, ch := range remote.Msg.GetMeasurement().GetChildren() {
		names = append(names, ch.GetName())
	}
	slices.Sort(names)
	if m := remote.Msg.GetMeasurement(); !m.GetComplete() || m.GetFiles() < 5 || !slices.Contains(names, "data") || !slices.Contains(names, "snapshots") {
		t.Errorf("MeasureRemoteFolder(the repository) = %v, children %q", remote.Msg, names)
	}
	if usage, err := e.probes.GetRemoteUsage(ctx, connect.NewRequest(&syncrv1.GetRemoteUsageRequest{RemoteId: minio})); err != nil ||
		!usage.Msg.GetUsage().GetUnsupported() {
		t.Errorf("GetRemoteUsage(MinIO) = %v (%v), want unsupported", usage, err)
	}

	// 2. Search: both versions of the draft, newest first, found by its
	// name and by the anchored, escaped exact path ("Show versions").
	// The pattern is the web's (exactPathPattern), from the golden file:
	// the draft's, below a tree whose names need no escaping.
	if strings.ContainsAny(tree, `*?[\`) {
		t.Fatalf("the temporary folder %q needs escaping in a pattern", tree)
	}
	exact := strings.TrimSuffix(tree, "/") + loadFilterGolden(t).findPattern(t, "/"+draft)
	for _, pattern := range []string{"*DRAFT*", exact} {
		found, err := e.restic.FindInRepository(ctx, connect.NewRequest(&syncrv1.FindInRepositoryRequest{
			RepositoryId: repo.GetId(), Patterns: []string{pattern}, IgnoreCase: pattern != exact,
		}))
		if err != nil {
			t.Fatalf("FindInRepository(%q): %v", pattern, err)
		}
		e.note(found.Msg)
		var got []string
		for _, m := range found.Msg.GetMatches() {
			got = append(got, m.GetSnapshotId()+" "+m.GetEntry().GetPath()+" "+strconv.FormatInt(m.GetEntry().GetSize(), 10))
		}
		want := []string{newer + " " + tree + "/" + draft + " 25", older + " " + tree + "/" + draft + " 10"}
		if !slices.Equal(got, want) || !found.Msg.GetComplete() || found.Msg.GetTruncated() || found.Msg.GetSnapshotsSearched() != 2 {
			t.Errorf("FindInRepository(%q) = %q (complete %v, truncated %v, searched %d), want %q", pattern, got,
				found.Msg.GetComplete(), found.Msg.GetTruncated(), found.Msg.GetSnapshotsSearched(), want)
		}
	}
	// A search of one snapshot finds only its version.
	one, err := e.restic.FindInRepository(ctx, connect.NewRequest(&syncrv1.FindInRepositoryRequest{
		RepositoryId: repo.GetId(), Patterns: []string{"gone.txt"}, SnapshotIds: []string{newer},
	}))
	if err != nil || len(one.Msg.GetMatches()) != 0 || one.Msg.GetSnapshotsSearched() != 1 {
		t.Errorf("FindInRepository(gone.txt, the newer snapshot) = %v (%v), want no match in 1 snapshot", one, err)
	}

	// 3. Compare: the whole tree, then one folder.
	cmp, err := e.restic.CompareSnapshots(ctx, connect.NewRequest(&syncrv1.CompareSnapshotsRequest{
		RepositoryId: repo.GetId(), FromSnapshotId: older, ToSnapshotId: newer,
	}))
	if err != nil {
		t.Fatalf("CompareSnapshots: %v", err)
	}
	e.note(cmp.Msg)
	changes := map[string]syncrv1.ResticDiffModifier{}
	for _, ch := range cmp.Msg.GetChanges() {
		changes[ch.GetPath()] = ch.GetModifier()
	}
	for p, want := range map[string]syncrv1.ResticDiffModifier{
		tree + "/" + draft:     syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_CHANGED,
		tree + "/gone.txt":     syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_REMOVED,
		tree + "/new.txt":      syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_ADDED,
		tree + "/docs/one.txt": syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_UNSPECIFIED,
		tree + "/photos/a.jpg": syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_UNSPECIFIED,
	} {
		if changes[p] != want {
			t.Errorf("CompareSnapshots: %s is %v, want %v (changes %v)", p, changes[p], want, changes)
		}
	}
	for p := range changes {
		if !strings.HasPrefix(p, tree+"/") && !strings.HasSuffix(p, "/") {
			t.Errorf("CompareSnapshots: a change outside the folder: %s", p)
		}
	}
	if st := cmp.Msg.GetStats(); !cmp.Msg.GetComplete() || cmp.Msg.GetTruncated() || st.GetChangedFiles() != 1 ||
		st.GetAddedFiles() != 1 || st.GetRemovedFiles() != 1 {
		t.Errorf("CompareSnapshots: complete %v, truncated %v, stats %v", cmp.Msg.GetComplete(), cmp.Msg.GetTruncated(), st)
	}
	folder, err := e.restic.CompareSnapshots(ctx, connect.NewRequest(&syncrv1.CompareSnapshotsRequest{
		RepositoryId: repo.GetId(), FromSnapshotId: older, ToSnapshotId: newer, Path: tree + "/docs",
	}))
	if err != nil || len(folder.Msg.GetChanges()) != 0 || !folder.Msg.GetComplete() || folder.Msg.GetStats() == nil {
		t.Errorf("CompareSnapshots(an unchanged folder) = %v (%v), want no changes", folder, err)
	}

	// 4. "Restore to original location": selected items of a subfolder,
	// written where they were. An existing file is kept by default.
	if err := os.RemoveAll(filepath.Join(src, "docs")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(src, draft), "mine")
	restored := e.run(restoreJob(&syncrv1.ResticRestoreDefinition{
		RepositoryId: repo.GetId(), SnapshotId: older, Target: src, Subfolder: tree,
		Paths: []string{tree + "/docs/deep", tree + "/" + draft, tree + "/gone.txt"},
	}))
	if !strings.Contains(restored.GetTitle(), "3 items of "+tree) {
		t.Errorf("the restore's title = %q", restored.GetTitle())
	}
	sameTree(t, "the original folder after the restore", src, map[string]string{
		"docs/deep/two.txt": sized(200), draft: "mine", "gone.txt": sized(5),
		"photos/a.jpg": sized(1000), "photos/b.jpg": sized(2000), "new.txt": sized(7),
	})
	e.noPasswordAnywhere()
}

// filterGolden is web/src/lib/rclone-filters.golden.json: selections as the
// web turns them into a transfer's source folder and filter lines, checked
// with rclone lsf, and the web's exact-path patterns of restic find.
type filterGolden struct {
	Rclone string   `json:"rclone"`
	Tree   []string `json:"tree"`
	Cases  []struct {
		Name    string   `json:"name"`
		Source  string   `json:"source"`
		Filters []string `json:"filters"`
		Matched []string `json:"matched"`
	} `json:"cases"`
	ResticFind []struct {
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
	} `json:"resticFind"`
}

// findPattern is the web's exact find pattern of a snapshot tree path.
func (g filterGolden) findPattern(t *testing.T, path string) string {
	t.Helper()
	for _, f := range g.ResticFind {
		if f.Path == path {
			return f.Pattern
		}
	}
	t.Fatalf("the golden file has no find pattern of %q", path)
	return ""
}

func loadFilterGolden(t *testing.T) filterGolden {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "web", "src", "lib", "rclone-filters.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g filterGolden
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	if g.Rclone != "v1.75.2" || len(g.Cases) == 0 {
		t.Fatalf("the golden file is for rclone %s with %d cases", g.Rclone, len(g.Cases))
	}
	return g
}

// buildTree creates the golden tree under root: entries ending in "/" are
// empty folders, files hold their own path.
func (g filterGolden) buildTree(t *testing.T, root string) {
	t.Helper()
	for _, p := range g.Tree {
		if strings.HasSuffix(p, "/") {
			if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		write(t, filepath.Join(root, p), p)
	}
}

// filesBelow is the golden tree's files below folder (relative to it),
// with their content.
func (g filterGolden) filesBelow(folder string) map[string]string {
	out := map[string]string{}
	for _, p := range g.Tree {
		if strings.HasSuffix(p, "/") {
			continue
		}
		if folder == "" {
			out[p] = p
		} else if rel, ok := strings.CutPrefix(p, folder+"/"); ok {
			out[rel] = p
		}
	}
	return out
}

// selectionDef is the definition the web's transfer sheet sends for a
// selection: the filters, empty folders created, "Keep existing", and for
// a move the emptied folders removed.
func selectionDef(mode syncrv1.RcloneTransferMode, src, dst *syncrv1.JobLocation, lines []string) *syncrv1.JobDefinition {
	opts := &syncrv1.RcloneTransferOptions{
		IgnoreExisting: true, CreateEmptySrcDirs: true,
		DeleteEmptySrcDirs: mode == syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_MOVE,
	}
	for _, l := range lines {
		action := syncrv1.FilterAction_FILTER_ACTION_INCLUDE
		if strings.HasPrefix(l, "- ") {
			action = syncrv1.FilterAction_FILTER_ACTION_EXCLUDE
		}
		opts.Filters = append(opts.Filters, &syncrv1.FilterRule{Action: action, Pattern: l[2:]})
	}
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransferDefinition{
		Mode: mode, Source: src, Destination: dst, Options: opts,
	}}}
}

// folders returns every folder below dir as its slash-separated relative
// path.
func folders(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || p == dir {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		out = append(out, filepath.ToSlash(rel))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// TestSystemBrowserTransfers copies and moves selections the way the file
// browser does, with the real rclone between a worker's folder and MinIO:
// every case of the web's golden filter file is copied into the bucket,
// and exactly its selected files arrive there, under the selected items'
// own names (a selected folder arrives as a folder). A move takes exactly
// the selected items away from the worker and removes the moved folders
// (and folders the move emptied), never the source folder or an unselected
// one; a move back from the bucket takes only the selected folder.
func TestSystemBrowserTransfers(t *testing.T) {
	t.Parallel()
	rclone, restic := requirements(t)
	cbin, wbin := binaries(t)
	endpoint := startMinIO(t)
	c := newController(t, cbin)
	w := setupWorker(t, c, wbin, rclone, restic, false)
	w.start()
	c.waitOnline(w.id)
	minio := minioRemote(c, endpoint)
	probes := syncrv1connect.NewProbeServiceClient(c.client(), c.base)
	g := loadFilterGolden(t)
	run := func(def *syncrv1.JobDefinition) *syncrv1.Job {
		t.Helper()
		j, _ := c.watch(c.runJob(w.id, def))
		wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
		return j
	}
	at := func(root, rel string) string {
		if rel == "" {
			return root
		}
		return filepath.Join(root, filepath.FromSlash(rel))
	}

	// 1. Copy: every case into its own folder of the bucket, then the whole
	// bucket folder back, so each case's arrivals can be compared.
	tree := filepath.Join(w.root, "tree")
	g.buildTree(t, tree)
	copySel := syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY
	for i, cs := range g.Cases {
		run(selectionDef(copySel, local(at(tree, cs.Source)), onRemote(minio, "copy/"+strconv.Itoa(i)), cs.Filters))
	}
	back := filepath.Join(w.root, "back")
	run(copyDef(onRemote(minio, "copy"), local(back), 0))
	for i, cs := range g.Cases {
		below := g.filesBelow(cs.Source)
		want := map[string]string{}
		for _, m := range cs.Matched {
			if !strings.HasSuffix(m, "/") {
				want[m] = below[m]
			}
		}
		got := map[string]string{}
		if _, err := os.Stat(filepath.Join(back, strconv.Itoa(i))); err == nil {
			got = files(t, filepath.Join(back, strconv.Itoa(i)))
		}
		if !maps.Equal(got, want) {
			t.Errorf("copy %q: the bucket received %q, want %q", cs.Name, sortedKeys(got), sortedKeys(want))
		}
	}
	// The source is untouched.
	sameTree(t, "the copied tree", tree, g.filesBelow(""))

	// 2. Move three selections into the bucket: siblings (a folder and a
	// file), an empty folder, and folders with pattern characters next to
	// decoys.
	moved := filepath.Join(w.root, "moved")
	g.buildTree(t, moved)
	moveSel := syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_MOVE
	byName := map[string]int{}
	for i, cs := range g.Cases {
		byName[cs.Name] = i
	}
	wantLeft := g.filesBelow("")
	for _, name := range []string{"siblings, a folder and a file", "an empty folder", "awkward folder names"} {
		cs := g.Cases[byName[name]]
		run(selectionDef(moveSel, local(at(moved, cs.Source)), onRemote(minio, "move/"+strconv.Itoa(byName[name])), cs.Filters))
		for _, m := range cs.Matched {
			if !strings.HasSuffix(m, "/") {
				delete(wantLeft, path.Join(cs.Source, m))
			}
		}
	}
	sameTree(t, "the worker's folder after the moves", moved, wantLeft)
	// The moved folders are gone, and so is 02, which the move emptied; the
	// source folders and every unselected folder stay.
	dirs := folders(t, moved)
	for _, gone := range []string{"photos/2025/01", "photos/2025/02", "photos/empty", "awkward/dir*", "awkward/{x}", "awkward/[d]", "awkward/-rf", "awkward/ünï dir", "awkward/**"} {
		if slices.Contains(dirs, gone) {
			t.Errorf("the move left the folder %s", gone)
		}
	}
	for _, kept := range []string{"photos/2025", "photos/2025/03", "awkward", "awkward/dirX", "awkward/x", "awkward/d", "awkward/zz"} {
		if !slices.Contains(dirs, kept) {
			t.Errorf("the move removed the folder %s (left %q)", kept, dirs)
		}
	}

	// 3. Move one folder back from the bucket: only it leaves the bucket.
	awkward := "move/" + strconv.Itoa(byName["awkward folder names"])
	returned := filepath.Join(w.root, "returned")
	run(selectionDef(moveSel, onRemote(minio, awkward), local(returned), []string{`+ /dir\*/**`, "- **"}))
	sameTree(t, "the folder moved back", returned, map[string]string{"dir*/in.txt": "awkward/dir*/in.txt"})
	left, err := probes.MeasureRemoteFolder(t.Context(), connect.NewRequest(&syncrv1.MeasureRemoteFolderRequest{RemoteId: minio, Path: awkward}))
	if err != nil {
		t.Fatalf("MeasureRemoteFolder: %v", err)
	}
	var names []string
	for _, ch := range left.Msg.GetMeasurement().GetChildren() {
		names = append(names, ch.GetName())
	}
	slices.Sort(names)
	if want := []string{"**", "-rf", "[d]", "{x}", "ünï dir"}; !slices.Equal(names, want) || left.Msg.GetMeasurement().GetFiles() != 5 {
		t.Errorf("the bucket's folder after the move back holds %q (%d files), want %q (5 files)", names, left.Msg.GetMeasurement().GetFiles(), want)
	}
}
