//go:build integration

package system

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// slowCopy is a bandwidth-limited copy of size bytes that lasts several
// seconds, long enough to kill a process in the middle of it.
const (
	slowSize  = 4 << 20
	slowLimit = 512 << 10
)

// TestSystemActiveCopy: enroll an active worker with `setup`, run it, and
// copy files through the API, following the job with WatchJob.
func TestSystemActiveCopy(t *testing.T) {
	t.Parallel()
	rclone, restic := requirements(t)
	cbin, wbin := binaries(t)
	c := newController(t, cbin)
	w := setupWorker(t, c, wbin, rclone, restic, false)
	w.start()
	info := c.waitOnline(w.id)
	if info.GetMode() != syncrv1.WorkerMode_WORKER_MODE_ACTIVE || len(info.GetInfo().GetLocalRoots()) != 1 {
		t.Errorf("worker = %v", info)
	}

	src, dst := filepath.Join(w.root, "src"), filepath.Join(w.root, "dst")
	writeRandom(t, filepath.Join(src, "a.bin"), 256<<10)
	writeRandom(t, filepath.Join(src, "sub", "b.bin"), 64<<10)
	j, seen := c.watch(c.runJob(w.id, copySpec(src, dst, 0)))
	wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
	sameFiles(t, src, dst)
	if j.GetStats().GetFiles() != 2 {
		t.Errorf("stats = %v", j.GetStats())
	}
	t.Logf("statuses seen: %v", seen)

	// A path outside local_roots is refused by the worker.
	j, _ = c.watch(c.runJob(w.id, copySpec(src, t.TempDir(), 0)))
	wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_FAILED)
	if j.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED {
		t.Errorf("error = %v", j.GetError())
	}

	// A graceful stop of the worker is clean.
	if err := w.proc.stop(30 * time.Second); err != nil {
		t.Errorf("worker stop: %v", err)
	}
}

// TestSystemPassiveCopy: `setup --passive` enrolls with the controller, which
// pins the generated certificate and calls the worker.
func TestSystemPassiveCopy(t *testing.T) {
	t.Parallel()
	rclone, restic := requirements(t)
	cbin, wbin := binaries(t)
	c := newController(t, cbin)
	w := setupWorker(t, c, wbin, rclone, restic, true)
	w.start()
	info := c.waitOnline(w.id)
	if info.GetMode() != syncrv1.WorkerMode_WORKER_MODE_PASSIVE || len(info.GetCertFingerprint()) != 64 {
		t.Errorf("worker = %v", info)
	}

	src, dst := filepath.Join(w.root, "src"), filepath.Join(w.root, "dst")
	writeRandom(t, filepath.Join(src, "a.bin"), 300<<10)
	j, _ := c.watch(c.runJob(w.id, copySpec(src, dst, 0)))
	wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
	sameFiles(t, src, dst)

	if err := w.proc.stop(30 * time.Second); err != nil {
		t.Errorf("worker stop: %v", err)
	}
}

// TestSystemControllerRestartMidJob kills the controller while a job runs and
// starts it again on the same database and port: the worker's buffered
// events are delivered and the job ends succeeded.
func TestSystemControllerRestartMidJob(t *testing.T) {
	t.Parallel()
	rclone, restic := requirements(t)
	cbin, wbin := binaries(t)
	c := newController(t, cbin)
	w := setupWorker(t, c, wbin, rclone, restic, false)
	w.start()
	c.waitOnline(w.id)

	src, dst := filepath.Join(w.root, "src"), filepath.Join(w.root, "dst")
	writeRandom(t, filepath.Join(src, "big.bin"), slowSize)
	id := c.runJob(w.id, copySpec(src, dst, slowLimit))
	c.waitJob(id, "running", func(j *syncrv1.Job) bool { return j.GetStatus() == syncrv1.JobStatus_JOB_STATUS_RUNNING })

	c.proc.kill()
	time.Sleep(time.Second)
	c.start()

	j, _ := c.watch(id)
	wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
	sameFiles(t, src, dst)
	if j.GetStats().GetBytes() != slowSize {
		t.Errorf("stats = %v", j.GetStats())
	}
}

// TestSystemWorkerKillMidJob kills the worker while a job runs: when the
// worker comes back (a new process, a new epoch) the job is interrupted, and
// the next job succeeds.
func TestSystemWorkerKillMidJob(t *testing.T) {
	t.Parallel()
	rclone, restic := requirements(t)
	cbin, wbin := binaries(t)
	c := newController(t, cbin)
	w := setupWorker(t, c, wbin, rclone, restic, false)
	t.Cleanup(func() { killMatching(t, w.dir) })
	w.start()
	c.waitOnline(w.id)

	src := filepath.Join(w.root, "src")
	writeRandom(t, filepath.Join(src, "big.bin"), slowSize)
	id := c.runJob(w.id, copySpec(src, filepath.Join(w.root, "dst"), slowLimit/2))
	c.waitJob(id, "running", func(j *syncrv1.Job) bool { return j.GetStatus() == syncrv1.JobStatus_JOB_STATUS_RUNNING })

	w.proc.kill()
	killMatching(t, w.dir) // the orphaned rclone
	w.start()

	j, _ := c.watch(id)
	wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_INTERRUPTED)
	if j.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_WORKER_RESTARTED {
		t.Errorf("error = %v", j.GetError())
	}

	small := filepath.Join(w.root, "small")
	writeRandom(t, filepath.Join(small, "s.bin"), 32<<10)
	next, _ := c.watch(c.runJob(w.id, copySpec(small, filepath.Join(w.root, "dst2"), 0)))
	wantStatus(t, next, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
	sameFiles(t, small, filepath.Join(w.root, "dst2"))
}

// TestSystemWorkerStopMidJob stops a passive worker gracefully (SIGTERM)
// while a job runs: the worker cancels the job and keeps serving until the
// controller has pulled the result, so the job ends interrupted
// (worker_shutdown) without waiting for the worker to come back.
func TestSystemWorkerStopMidJob(t *testing.T) {
	t.Parallel()
	rclone, restic := requirements(t)
	cbin, wbin := binaries(t)
	c := newController(t, cbin)
	w := setupWorker(t, c, wbin, rclone, restic, true)
	t.Cleanup(func() { killMatching(t, w.dir) })
	w.start()
	c.waitOnline(w.id)

	src := filepath.Join(w.root, "src")
	writeRandom(t, filepath.Join(src, "big.bin"), slowSize)
	id := c.runJob(w.id, copySpec(src, filepath.Join(w.root, "dst"), slowLimit/2))
	c.waitJob(id, "running", func(j *syncrv1.Job) bool { return j.GetStatus() == syncrv1.JobStatus_JOB_STATUS_RUNNING })

	if err := w.proc.stop(30 * time.Second); err != nil {
		t.Fatalf("worker stop: %v", err)
	}
	j := c.waitJob(id, "a terminal status", func(j *syncrv1.Job) bool { return terminal(j.GetStatus()) })
	wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_INTERRUPTED)
	if j.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_WORKER_SHUTDOWN {
		t.Errorf("error = %v", j.GetError())
	}
	if !strings.Contains(w.proc.out.String(), "shutdown: every event collected by the controller") {
		t.Errorf("the worker did not report a clean flush:\n%s", tail(w.proc.out.String(), 4<<10))
	}
}

// TestSystemResticToMinIO initializes a restic repository in a MinIO bucket
// and backs up the worker's files into it, one job each, over the active
// link. The S3 credentials travel only inside the encrypted job spec.
func TestSystemResticToMinIO(t *testing.T) {
	t.Parallel()
	rclone, restic := requirements(t)
	cbin, wbin := binaries(t)
	endpoint := startMinIO(t)
	c := newController(t, cbin)
	w := setupWorker(t, c, wbin, rclone, restic, false)
	w.start()
	c.waitOnline(w.id)

	src := filepath.Join(w.root, "src")
	writeRandom(t, filepath.Join(src, "a.bin"), 200<<10)
	writeRandom(t, filepath.Join(src, "docs", "b.bin"), 20<<10)
	remotes := []*syncrv1.RemoteDefinition{{
		Alias:       "minio",
		BackendType: "s3",
		Options: map[string]string{
			"provider": "Minio", "endpoint": endpoint, "region": "us-east-1",
			"access_key_id": minioUser, "force_path_style": "true",
		},
		Secrets: map[string]string{"secret_access_key": minioPassword},
	}}
	repo := &syncrv1.ResticRepository{
		Location: &syncrv1.Location{Remote: "minio", Path: "syncr-e2e/repo"},
		Password: "e2e-repository-password",
	}

	j, _ := c.watch(c.runJob(w.id, &syncrv1.JobSpec{
		Remotes:   remotes,
		Operation: &syncrv1.JobSpec_ResticInit{ResticInit: &syncrv1.ResticInit{Repository: repo}},
	}))
	wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)

	j, _ = c.watch(c.runJob(w.id, &syncrv1.JobSpec{
		Remotes:   remotes,
		Operation: &syncrv1.JobSpec_ResticBackup{ResticBackup: &syncrv1.ResticBackup{Repository: repo, Paths: []string{src}, Tags: []string{"e2e"}}},
	}))
	wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
	out := j.GetOutput().GetResticBackup()
	if out.GetSnapshotId() == "" || out.GetFilesNew() != 2 {
		t.Errorf("backup output = %v", out)
	}
	if strings.Contains(w.proc.out.String(), minioPassword) || strings.Contains(c.proc.out.String(), minioPassword) {
		t.Error("the S3 secret appears in a log")
	}
}

const (
	minioUser     = "syncr-e2e"
	minioPassword = "syncr-e2e-secret-key"
)

// startMinIO starts a MinIO server and returns its endpoint URL.
func startMinIO(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        minioImage,
			Cmd:          []string{"server", "/data"},
			Env:          map[string]string{"MINIO_ROOT_USER": minioUser, "MINIO_ROOT_PASSWORD": minioPassword},
			ExposedPorts: []string{"9000/tcp"},
			// Memory-backed: MinIO refuses writes on a nearly full disk, and
			// nothing is left behind.
			Tmpfs:      map[string]string{"/data": "size=1g"},
			WaitingFor: wait.ForHTTP("/minio/health/live").WithPort("9000/tcp").WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start MinIO: %v", err)
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := ctr.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("http://%s:%s", host, port.Port())
}
