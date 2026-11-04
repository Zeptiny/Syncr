//go:build integration

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/platform/logging"
	workerconfig "github.com/Zeptiny/Syncr/internal/worker/config"
	"github.com/Zeptiny/Syncr/internal/worker/paths"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// TestIntegrationWorkerRunsJobsFromTheRegistry assembles the worker exactly as
// `run` does (real supervisor, real tool verification, real localfs policy and
// rcloneenv builder), then executes jobs through the registry: a local rclone
// copy and a restic init, backup, snapshots and restore on local paths.
// SYNCR_TEST_RCLONE and SYNCR_TEST_RESTIC must be the pinned versions (see
// `make tools`).
func TestIntegrationWorkerRunsJobsFromTheRegistry(t *testing.T) {
	rcloneBin, resticBin := os.Getenv("SYNCR_TEST_RCLONE"), os.Getenv("SYNCR_TEST_RESTIC")
	if rcloneBin == "" || resticBin == "" {
		t.Skip("SYNCR_TEST_RCLONE and SYNCR_TEST_RESTIC are not set")
	}
	base := t.TempDir()
	root := filepath.Join(base, "roots")
	cfg := workerconfig.Default()
	cfg.DataDir = filepath.Join(base, "data")
	cfg.LocalRoots = []string{root}
	cfg.Tools.RclonePath, cfg.Tools.ResticPath = rcloneBin, resticBin

	var logs bytes.Buffer
	log, err := logging.New(&logs, cfg.Log)
	if err != nil {
		t.Fatal(err)
	}
	e, _, _ := testEnv(nil)
	w, err := buildWorker(t.Context(), cfg, workerDeps{paths: e.pathsEnv()}, log)
	if err != nil {
		t.Fatalf("buildWorker: %v\nlogs:\n%s", err, &logs)
	}

	runJob := func(id string, spec *syncrv1.JobSpec) (runner.Result, error) {
		t.Helper()
		rn, err := w.registry.ForSpec(spec)
		if err != nil {
			t.Fatalf("ForSpec: %v", err)
		}
		return rn.Run(context.Background(), runner.Job{ID: id, Spec: spec}, func(runner.Progress) {})
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(filepath.Join(src, "a.txt"), "alpha\n")
	write(filepath.Join(src, "sub", "b.txt"), "bravo\n")

	// rclone: local to local copy.
	if _, err := runJob("it-rclone-copy", &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{
		Mode:        syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY,
		Source:      &syncrv1.Location{Path: src},
		Destination: &syncrv1.Location{Path: dst},
	}}}); err != nil {
		t.Fatalf("rclone copy: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "sub", "b.txt")); err != nil || string(got) != "bravo\n" {
		t.Errorf("copied file = %q, %v", got, err)
	}

	// A path outside local_roots is refused before any tool runs.
	_, err = runJob("it-rclone-outside", &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{
		Mode:        syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY,
		Source:      &syncrv1.Location{Path: src},
		Destination: &syncrv1.Location{Path: filepath.Join(base, "outside")},
	}}})
	if got := runner.CodeOf(err); got != syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED {
		t.Errorf("copy outside local_roots: code %v (err %v), want PATH_NOT_ALLOWED", got, err)
	}

	// restic: init, backup, snapshots, restore, on a local repository.
	repo := &syncrv1.ResticRepository{Location: &syncrv1.Location{Path: filepath.Join(root, "repo")}, Password: "wiring-test-password"}
	if _, err := runJob("it-restic-init", &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_ResticInit{ResticInit: &syncrv1.ResticInit{Repository: repo}}}); err != nil {
		t.Fatalf("restic init: %v", err)
	}
	res, err := runJob("it-restic-backup", &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_ResticBackup{ResticBackup: &syncrv1.ResticBackup{
		Repository: repo, Paths: []string{src},
	}}})
	if err != nil {
		t.Fatalf("restic backup: %v", err)
	}
	snapshotID := res.Output.GetResticBackup().GetSnapshotId()
	if snapshotID == "" || len(res.Warnings) != 0 {
		t.Errorf("backup snapshot %q, warnings %q", snapshotID, res.Warnings)
	}
	res, err = runJob("it-restic-snapshots", &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_ResticSnapshots{ResticSnapshots: &syncrv1.ResticSnapshots{Repository: repo}}})
	if err != nil {
		t.Fatalf("restic snapshots: %v", err)
	}
	if snaps := res.Output.GetResticSnapshots().GetSnapshots(); len(snaps) != 1 || snaps[0].GetId() != snapshotID {
		t.Errorf("snapshots = %v, want just %s", snaps, snapshotID)
	}
	target := filepath.Join(root, "restored")
	if _, err := runJob("it-restic-restore", &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_ResticRestore{ResticRestore: &syncrv1.ResticRestore{
		Repository: repo, SnapshotId: snapshotID, Target: target,
	}}}); err != nil {
		t.Fatalf("restic restore: %v", err)
	}
	// restic restores a path below the target; on Windows "C:\x" becomes "C\x".
	restored := filepath.Join(target, src[len(filepath.VolumeName(src)):])
	if vol := filepath.VolumeName(src); vol != "" {
		restored = filepath.Join(target, vol[:1], src[len(vol):])
	}
	if got, err := os.ReadFile(filepath.Join(restored, "a.txt")); err != nil || string(got) != "alpha\n" {
		t.Errorf("restored a.txt = %q, %v (looked in %s)", got, err, restored)
	}

	// Jobs leave no scratch directories behind.
	for _, dir := range []string{filepath.Join(paths.ScratchDir(cfg.DataDir), "rclone"), filepath.Join(paths.ScratchDir(cfg.DataDir), "restic")} {
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%s not cleaned up: %v", dir, entries)
		}
	}
}
