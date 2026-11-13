//go:build integration

package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	wprobe "github.com/Zeptiny/Syncr/internal/worker/probe"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
	"github.com/Zeptiny/Syncr/internal/worker/runner/rclone"
	wrestic "github.com/Zeptiny/Syncr/internal/worker/runner/restic"
)

// scanPassword is the restic password of the scan tests' repository.
const scanPassword = "e2e-scan-repo-password"

// TestIntegrationScans measures, searches and compares through a real
// worker (its probe executor with the real rclone and restic,
// SYNCR_TEST_RCLONE and SYNCR_TEST_RESTIC) connected over HTTPS: a worker's
// folder, a remote on its disk, a union remote (rclone lsjson -R), a
// remote's usage, and a repository backed up twice with a change in
// between (restic ls --recursive, find and diff).
func TestIntegrationScans(t *testing.T) {
	rcloneBin, resticBin := os.Getenv("SYNCR_TEST_RCLONE"), os.Getenv("SYNCR_TEST_RESTIC")
	if rcloneBin == "" || resticBin == "" {
		t.Skip("SYNCR_TEST_RCLONE or SYNCR_TEST_RESTIC not set")
	}
	ctx := t.Context()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	write := func(name string, size int) {
		t.Helper()
		p := filepath.Join(data, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, bytes.Repeat([]byte{'x'}, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", 100)
	write("docs/b.txt", 2000)
	write("photos/2026/c.jpg", 30000)
	write("photos/2026/d.jpg", 40000)

	// Two snapshots: a.txt grows, docs/b.txt goes, new.txt comes.
	repo := filepath.Join(root, "repo")
	resticCmd := func(args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, resticBin, args...)
		cmd.Env = []string{"RESTIC_REPOSITORY=" + repo, "RESTIC_PASSWORD=" + scanPassword, "RESTIC_CACHE_DIR=" + filepath.Join(root, "cache"), "HOME=" + root}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("restic %v: %v", args, err)
		}
		return out
	}
	resticCmd("init")
	resticCmd("backup", "--host", "e2e-host", "--", data)
	write("a.txt", 150)
	write("new.txt", 7)
	if err := os.Remove(filepath.Join(data, "docs", "b.txt")); err != nil {
		t.Fatal(err)
	}
	resticCmd("backup", "--host", "e2e-host", "--", data)
	var snaps []struct {
		ID   string    `json:"id"`
		Time time.Time `json:"time"`
	}
	if err := json.Unmarshal(resticCmd("snapshots", "--json"), &snaps); err != nil || len(snaps) != 2 {
		t.Fatalf("snapshots: %v (%d)", err, len(snaps))
	}
	slices.SortFunc(snaps, func(a, b struct {
		ID   string    `json:"id"`
		Time time.Time `json:"time"`
	}) int {
		return a.Time.Compare(b.Time)
	})
	first, second := snaps[0].ID, snaps[1].ID

	if err := os.Mkdir(filepath.Join(root, "spare"), 0o755); err != nil { // the union's empty second member
		t.Fatal(err)
	}

	// The worker: its real probe executor.
	policy, err := localfs.NewPolicy([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	sup := proc.New(proc.Options{GracePeriod: 5 * time.Second})
	env := rcloneenv.NewBuilder(policy)
	rp, err := rclone.NewProber(rclone.Config{Binary: rcloneBin, Supervisor: sup, Env: env, Paths: policy, ScratchDir: filepath.Join(t.TempDir(), "rclone")})
	if err != nil {
		t.Fatal(err)
	}
	sp, err := wrestic.NewProber(wrestic.Config{
		ResticPath: resticBin, RclonePath: rcloneBin, Supervisor: sup, Remotes: env, Paths: policy,
		ScratchRoot: filepath.Join(t.TempDir(), "restic"), CacheDir: filepath.Join(t.TempDir(), "cache"),
	})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := wprobe.New(wprobe.Config{Restic: sp, Rclone: rp, Local: policy})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(executor.Ops(), link.IsHeavyProbeOp) {
		t.Fatalf("the executor runs no heavy probe: %v", executor.Ops())
	}

	c := newController(t)
	disk, pool := fixedRemoteID("00000000-0000-7000-8000-00000000c002"), fixedRemoteID("00000000-0000-7000-8000-00000000c003")
	c.addRemote(disk, "Disk", backend.Rclone{Root: root})
	c.addRemote(pool, "Pool", backend.Rclone{Backend: "union", Options: map[string]string{}})
	c.remotes.mu.Lock()
	r := c.remotes.resolved[pool]
	r.Members = []remotes.Member{{RemoteID: disk, Path: "data", Mode: backend.UnionReadWrite}, {RemoteID: disk, Path: "spare", Mode: backend.UnionReadOnly}}
	c.remotes.resolved[pool] = r
	c.remotes.mu.Unlock()
	pw := newProbeWorker(t, c, false, workerOptions{prober: executor, features: link.Features(), probeOps: executor.Ops()})
	svc, runner := c.probeService(), c.probeRunner()

	// A worker's folder: after the change, 4 files in 3 folders.
	m, err := svc.MeasureWorkerFolder(ctx, testActor(), pw.id, data)
	if err != nil {
		t.Fatal(err)
	}
	mm := m.GetMeasurement()
	if mm.GetPath() != data || mm.GetFiles() != 4 || mm.GetDirs() != 3 || mm.GetBytes() != 150+7+30000+40000 || !mm.GetComplete() ||
		len(mm.GetChildren()) != 2 || mm.GetChildren()[0].GetName() != "photos" || mm.GetChildren()[0].GetBytes() != 70000 {
		t.Errorf("worker folder = %v", m)
	}
	// A remote on the worker's disk, through the chosen worker.
	if rm, err := svc.MeasureRemoteFolder(ctx, testActor(), disk, "data/photos", pw.id); err != nil || rm.GetMeasurement().GetPath() != "data/photos" ||
		rm.GetMeasurement().GetFiles() != 2 || rm.GetMeasurement().GetBytes() != 70000 {
		t.Errorf("local remote = %v, %v", rm, err)
	}
	// A union of it, through rclone (the only worker is picked).
	um, err := svc.MeasureRemoteFolder(ctx, testActor(), pool, "", domain.WorkerID{})
	if err != nil || um.GetMeasurement().GetPath() != "" || um.GetMeasurement().GetFiles() != 4 || um.GetMeasurement().GetBytes() != mm.GetBytes() ||
		!um.GetMeasurement().GetComplete() || um.GetWorker().GetId() != pw.id.String() {
		t.Errorf("union remote = %v, %v", um, err)
	}
	if _, err := svc.MeasureRemoteFolder(ctx, testActor(), pool, "nope", domain.WorkerID{}); err == nil {
		t.Error("a missing remote folder was measured")
	}
	// Usage: the union asks rclone; the disk remote has none.
	if u, err := svc.RemoteUsage(ctx, testActor(), pool, domain.WorkerID{}); err != nil || u.GetUsage() == nil {
		t.Errorf("union usage = %v, %v", u, err)
	}
	if u, err := svc.RemoteUsage(ctx, testActor(), disk, pw.id); err != nil || !u.GetUsage().GetUnsupported() {
		t.Errorf("disk usage = %v, %v", u, err)
	}

	// The repository on the worker's disk.
	target := restic.Target{RemoteID: disk, Path: "repo", Password: scanPassword, WorkerID: pw.id}
	tree := filepath.ToSlash(data)
	if !strings.HasPrefix(tree, "/") {
		tree = "/" + strings.Replace(tree, ":", "", 1) // C:/x is /C/x in a snapshot
	}
	sm, err := runner.Measure(ctx, testOwner, target, second, tree)
	if err != nil || sm.Path != tree || sm.Files != 4 || sm.Bytes != mm.GetBytes() || !sm.Complete || sm.WorkerID != pw.id {
		t.Errorf("snapshot folder = %+v, %v", sm, err)
	}
	if fm, err := runner.Measure(ctx, testOwner, target, first[:8], tree+"/docs"); err != nil || fm.Files != 1 || fm.Bytes != 2000 {
		t.Errorf("the first snapshot's docs = %+v, %v", fm, err)
	}
	var pe *restic.ProbeError
	if _, err := runner.Measure(ctx, testOwner, target, second, tree+"/nope"); !errors.As(err, &pe) || pe.Code != "not_found" {
		t.Errorf("a folder the snapshot lacks = %v", err)
	}

	// Search: a.txt in both snapshots, newest first, in two versions.
	f, err := runner.Find(ctx, testOwner, target, restic.FindQuery{Patterns: []string{"A.TXT"}, IgnoreCase: true})
	if err != nil || len(f.Matches) != 2 || !f.Complete || f.Truncated || f.SnapshotsSearched != 2 ||
		f.Matches[0].SnapshotID != second || f.Matches[0].Entry.Size != 150 || f.Matches[1].Entry.Size != 100 || f.Matches[0].Entry.Path != tree+"/a.txt" {
		t.Errorf("find = %+v, %v", f, err)
	}
	if f, err := runner.Find(ctx, testOwner, target, restic.FindQuery{Patterns: []string{"*.jpg"}, SnapshotIDs: []string{first[:8]}}); err != nil ||
		len(f.Matches) != 2 || f.SnapshotsSearched != 1 || f.Matches[0].SnapshotID != first {
		t.Errorf("find in one snapshot = %+v, %v", f, err)
	}
	if _, err := runner.Find(ctx, testOwner, target, restic.FindQuery{Patterns: []string{"x"}, SnapshotIDs: []string{"deadbeef"}}); !errors.As(err, &pe) || pe.Code != "not_found" {
		t.Errorf("find in an unknown snapshot = %v", err)
	}

	// Compare: a.txt changed, docs/b.txt removed, new.txt added.
	d, err := runner.Diff(ctx, testOwner, target, first, second, "/")
	if err != nil || !d.Complete || d.Truncated || d.Stats == nil || d.Stats.ChangedFiles != 1 || d.Stats.AddedFiles != 1 || d.Stats.RemovedFiles != 1 {
		t.Fatalf("diff = %+v, %v", d, err)
	}
	got := map[string]restic.DiffModifier{}
	for _, ch := range d.Changes {
		got[ch.Path] = ch.Modifier
	}
	want := map[string]restic.DiffModifier{tree + "/a.txt": restic.DiffChanged, tree + "/docs/b.txt": restic.DiffRemoved, tree + "/new.txt": restic.DiffAdded}
	for p, m := range want {
		if got[p] != m {
			t.Errorf("change of %s = %q, want %q (all: %v)", p, got[p], m, d.Changes)
		}
	}
	if sub, err := runner.Diff(ctx, testOwner, target, first, second, tree+"/photos"); err != nil || len(sub.Changes) != 0 || sub.Stats == nil {
		t.Errorf("diff of an unchanged folder = %+v, %v", sub, err)
	}
	if _, err := runner.Diff(ctx, testOwner, target, first, "deadbeef", "/"); !errors.As(err, &pe) || pe.Code != "not_found" {
		t.Errorf("diff with an unknown snapshot = %v", err)
	}
	if strings.Contains(c.logs.String(), scanPassword) {
		t.Error("the controller logged the repository password")
	}
}
