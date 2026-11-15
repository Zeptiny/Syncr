//go:build integration && !windows

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/platform/logging"
	workerconfig "github.com/Zeptiny/Syncr/internal/worker/config"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/paths"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// linksWorker builds a worker as `run` does, with the pinned tools, and
// returns it with a function that runs a job through its registry.
func linksWorker(t *testing.T, cfg workerconfig.Config, configFile string) func(id string, spec *syncrv1.JobSpec) (runner.Result, error) {
	t.Helper()
	var logs bytes.Buffer
	log, err := logging.New(&logs, cfg.Log)
	if err != nil {
		t.Fatal(err)
	}
	e, _, _ := testEnv(nil)
	w, err := buildWorker(t.Context(), cfg, workerDeps{paths: e.pathsEnv(), configFile: configFile}, log)
	if err != nil {
		t.Fatalf("buildWorker: %v\nlogs:\n%s", err, &logs)
	}
	return func(id string, spec *syncrv1.JobSpec) (runner.Result, error) {
		t.Helper()
		rn, err := w.registry.ForSpec(spec)
		if err != nil {
			t.Fatalf("ForSpec: %v", err)
		}
		return rn.Run(t.Context(), runner.Job{ID: id, Spec: spec}, func(runner.Progress) {})
	}
}

// pinnedTools returns SYNCR_TEST_RCLONE and SYNCR_TEST_RESTIC, or skips.
func pinnedTools(t *testing.T) (rcloneBin, resticBin string) {
	t.Helper()
	rcloneBin, resticBin = os.Getenv("SYNCR_TEST_RCLONE"), os.Getenv("SYNCR_TEST_RESTIC")
	if rcloneBin == "" || resticBin == "" {
		t.Skip("SYNCR_TEST_RCLONE and SYNCR_TEST_RESTIC are not set")
	}
	return rcloneBin, resticBin
}

func putTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestIntegrationRestoredLinkCannotRedirectACopy replays the escape from
// local_roots with the pinned tools: a user restores, from a repository of
// their own, a snapshot holding a link to a folder outside the roots, then
// copies files into the restored folder. rclone would write through the
// link; the worker refuses the copy before rclone starts, and nothing
// lands outside the roots.
func TestIntegrationRestoredLinkCannotRedirectACopy(t *testing.T) {
	rcloneBin, resticBin := pinnedTools(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, outside, evilSrc := filepath.Join(base, "root"), filepath.Join(base, "outside"), filepath.Join(base, "evil-src")
	cfg := workerconfig.Default()
	cfg.DataDir = filepath.Join(base, "data")
	cfg.LocalRoots = []string{root}
	cfg.Tools.RclonePath, cfg.Tools.ResticPath = rcloneBin, resticBin
	runJob := linksWorker(t, cfg, "")

	// The user's snapshot, made outside Syncr: a link to the folder outside.
	for _, d := range []string{root, outside, evilSrc} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(evilSrc, "evil")); err != nil {
		t.Fatal(err)
	}
	repo := &syncrv1.ResticRepository{Location: &syncrv1.Location{Path: filepath.Join(root, "repo")}, Password: "links-test-password"}
	restic := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(resticBin, append([]string{"--repo", repo.GetLocation().GetPath(), "--no-cache", "-q"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "RESTIC_PASSWORD="+repo.GetPassword())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("restic %q: %v\n%s", args, err, out)
		}
	}
	restic(base, "init")
	restic(evilSrc, "backup", ".")

	// The restore plants the link inside the root.
	target := filepath.Join(root, "restored")
	if _, err := runJob("it-links-restore", &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_ResticRestore{ResticRestore: &syncrv1.ResticRestore{
		Repository: repo, SnapshotId: "latest", Target: target,
	}}}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if info, err := os.Lstat(filepath.Join(target, "evil")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the restore did not recreate the link: %v %v", info, err)
	}

	// A copy into the restored folder, or a folder holding it, is refused.
	payload := filepath.Join(root, "payload")
	putTestFile(t, filepath.Join(payload, "evil", "pwned.txt"), "pwned\n")
	for _, dst := range []string{target, root} {
		_, err := runJob("it-links-copy", &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{
			Mode:        syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY,
			Source:      &syncrv1.Location{Path: payload},
			Destination: &syncrv1.Location{Path: dst},
		}}})
		if runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED || !errors.Is(err, localfs.ErrLink) {
			t.Errorf("copy into %s: %v (code %v), want PATH_NOT_ALLOWED for the link", dst, err, runner.CodeOf(err))
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a file was written outside local_roots: %v", err)
	}
}

// TestIntegrationBackupLeavesTheWorkersFilesOut: with the data directory
// and the config file inside local_roots, a backup and a copy of the
// folder that holds them leave them out, and jobs cannot use them.
func TestIntegrationBackupLeavesTheWorkersFilesOut(t *testing.T) {
	rcloneBin, resticBin := pinnedTools(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, other := filepath.Join(base, "root"), filepath.Join(base, "other")
	configFile := filepath.Join(root, "etc", "worker.yaml")
	cfg := workerconfig.Default()
	cfg.DataDir = filepath.Join(root, "data")
	cfg.LocalRoots = []string{root, other}
	cfg.Tools.RclonePath, cfg.Tools.ResticPath = rcloneBin, resticBin
	putTestFile(t, configFile, "worker_secret: s3cr3t\n")
	putTestFile(t, filepath.Join(root, "etc", "other.conf"), "kept\n")
	putTestFile(t, filepath.Join(root, "docs", "a.txt"), "alpha\n")
	putTestFile(t, filepath.Join(paths.BisyncDir(cfg.DataDir), "k", "listing"), "someone else's files\n")
	runJob := linksWorker(t, cfg, configFile)

	repo := &syncrv1.ResticRepository{Location: &syncrv1.Location{Path: filepath.Join(other, "repo")}, Password: "links-test-password"}
	if _, err := runJob("it-reserved-init", &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_ResticInit{ResticInit: &syncrv1.ResticInit{Repository: repo}}}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := runJob("it-reserved-backup", &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_ResticBackup{ResticBackup: &syncrv1.ResticBackup{
		Repository: repo, Paths: []string{root},
	}}}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	cmd := exec.Command(resticBin, "--repo", repo.GetLocation().GetPath(), "--no-cache", "ls", "latest")
	cmd.Env = append(os.Environ(), "RESTIC_PASSWORD="+repo.GetPassword())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("restic ls: %v", err)
	}
	listing := string(out)
	if !strings.Contains(listing, filepath.Join(root, "etc", "other.conf")) || !strings.Contains(listing, filepath.Join(root, "docs", "a.txt")) {
		t.Errorf("the backup misses files beside the reserved ones:\n%s", listing)
	}
	// restic keeps a folder left out with --exclude-if-present, and its
	// marker, but nothing else in it.
	if strings.Contains(listing, "worker.yaml") || strings.Contains(listing, paths.BisyncDir(cfg.DataDir)) ||
		strings.Contains(listing, paths.SourcesDir(cfg.DataDir)) {
		t.Errorf("the backup holds the worker's files:\n%s", listing)
	}

	copyDst := filepath.Join(other, "copy")
	if _, err := runJob("it-reserved-copy", &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{
		Mode:        syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY,
		Source:      &syncrv1.Location{Path: root},
		Destination: &syncrv1.Location{Path: copyDst},
	}}}); err != nil {
		t.Fatalf("copy: %v", err)
	}
	for _, p := range []string{filepath.Join(copyDst, "etc", "worker.yaml"), filepath.Join(copyDst, "data")} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the copy holds %s: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(copyDst, "etc", "other.conf")); err != nil {
		t.Errorf("the copy misses etc/other.conf: %v", err)
	}

	for _, p := range []string{configFile, paths.BisyncDir(cfg.DataDir)} {
		_, err := runJob("it-reserved-direct", &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{
			Mode:        syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY,
			Source:      &syncrv1.Location{Path: p},
			Destination: &syncrv1.Location{Path: filepath.Join(other, "stolen")},
		}}})
		if runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED || !errors.Is(err, localfs.ErrReserved) {
			t.Errorf("copy of %s: %v, want PATH_NOT_ALLOWED", p, err)
		}
	}
}
