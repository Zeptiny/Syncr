//go:build integration

package restic

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
)

// minioImage is the image of deploy/docker-compose.dev.yml.
const minioImage = "pgsty/minio:RELEASE.2026-08-04T00-00-00Z"

// TestIntegrationMinIOLifecycle runs the full lifecycle on a repository in an
// S3 bucket, like on a real worker: the remote is defined purely through
// environment variables by the real rcloneenv.Builder, and local paths
// (backup sources, restore targets) are checked by the real localfs.Policy.
// No fakes sit between the runners and the tools.
func TestIntegrationMinIOLifecycle(t *testing.T) {
	toolPath(t, "SYNCR_TEST_RESTIC")
	toolPath(t, "SYNCR_TEST_RCLONE")
	if runtime.GOOS == "windows" {
		t.Skip("skipping: testcontainers needs Linux containers, and Windows CI runners only run Windows containers")
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)

	const accessKey, secretKey = "syncr-dev", "syncr-dev-secret"
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        minioImage,
			Cmd:          []string{"server", "/data"},
			Env:          map[string]string{"MINIO_ROOT_USER": accessKey, "MINIO_ROOT_PASSWORD": secretKey},
			ExposedPorts: []string{"9000/tcp"},
			// MinIO refuses writes when the data drive has less than ~900 MiB
			// free, which small CI disks do not guarantee; memory-backed
			// storage also makes the test fast and leaves nothing behind.
			Tmpfs:      map[string]string{"/data": "size=2g"},
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

	h := newHarnessWith(t, func(h *harness, cfg *Config) {
		policy, err := localfs.NewPolicy([]string{h.root})
		if err != nil {
			t.Fatalf("localfs.NewPolicy: %v", err)
		}
		cfg.Paths = policy
		cfg.Remotes = rcloneenv.NewBuilder(policy)
	})
	h.defs = []*syncrv1.RemoteDefinition{{
		Alias:       "minio",
		BackendType: "s3",
		Options: map[string]string{
			"provider":         "Minio",
			"endpoint":         fmt.Sprintf("http://%s:%s", host, port.Port()),
			"region":           "us-east-1",
			"access_key_id":    accessKey,
			"force_path_style": "true",
		},
		Secrets: map[string]string{"secret_access_key": secretKey},
	}}
	// rclone creates the bucket on init.
	h.loc = &syncrv1.Location{Remote: "minio", Path: "syncr-restic-it/repo"}
	runLifecycle(t, h)

	// The real policy rejects what is outside local_roots, whatever the
	// repository location: here a backup source.
	outside := filepath.Join(filepath.Dir(h.root), "elsewhere")
	writeFile(t, filepath.Join(outside, "secret.txt"), []byte("not for backups"))
	_, _, err = h.run(t.Context(), func(s *syncrv1.JobSpec) {
		s.Operation = &syncrv1.JobSpec_ResticBackup{ResticBackup: &syncrv1.ResticBackup{
			Repository: h.repo(itPassword), Paths: []string{outside},
		}}
	})
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED)
}
