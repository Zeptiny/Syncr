//go:build integration

package restic

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// minioImage is the image of deploy/docker-compose.dev.yml.
const minioImage = "pgsty/minio:RELEASE.2026-08-04T00-00-00Z"

// TestIntegrationMinIOLifecycle runs the full lifecycle on a repository in an
// S3 bucket, defined as an rclone remote purely through environment
// variables, like on a real worker.
func TestIntegrationMinIOLifecycle(t *testing.T) {
	toolPath(t, "SYNCR_TEST_RESTIC")
	toolPath(t, "SYNCR_TEST_RCLONE")
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

	h := newHarness(t)
	h.defs = []*syncrv1.RemoteDefinition{{
		Alias:       "minio",
		BackendType: "s3",
		Options: map[string]string{
			"provider":         "Minio",
			"endpoint":         fmt.Sprintf("http://%s:%s", host, port.Port()),
			"region":           "us-east-1",
			"access_key_id":    accessKey,
			"force_path_style": "true",
			"env_auth":         "false",
		},
		Secrets: map[string]string{"secret_access_key": secretKey},
	}}
	// rclone creates the bucket on init.
	h.loc = &syncrv1.Location{Remote: "minio", Path: "syncr-restic-it/repo"}
	runLifecycle(t, h)
}
