//go:build integration

package restic

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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

// newMinIOHarness starts MinIO and returns a harness set up like a real
// worker: the remote "minio" is defined purely through environment
// variables by the real rcloneenv.Builder, and local paths (backup sources,
// restore targets) are checked by the real localfs.Policy. No fakes sit
// between the runners and the tools.
func newMinIOHarness(t *testing.T) *harness {
	t.Helper()
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
	return h
}

// TestIntegrationMinIOLifecycle runs the full lifecycle on a repository in an
// S3 bucket, like on a real worker (see newMinIOHarness).
func TestIntegrationMinIOLifecycle(t *testing.T) {
	h := newMinIOHarness(t)
	// rclone creates the bucket on init.
	h.loc = &syncrv1.Location{Remote: "minio", Path: "syncr-restic-it/repo"}
	runLifecycle(t, h)

	// The real policy rejects what is outside local_roots, whatever the
	// repository location: here a backup source.
	outside := filepath.Join(filepath.Dir(h.root), "elsewhere")
	writeFile(t, filepath.Join(outside, "secret.txt"), []byte("not for backups"))
	_, _, err := h.run(t.Context(), func(s *syncrv1.JobSpec) {
		s.Operation = &syncrv1.JobSpec_ResticBackup{ResticBackup: &syncrv1.ResticBackup{
			Repository: h.repo(itPassword), Paths: []string{outside},
		}}
	})
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED)
}

// rclone runs the real rclone against the harness's remotes and returns its
// standard output. Tests use it to put unrelated objects next to a
// repository and to see what a deletion left.
func (h *harness) rclone(args ...string) string {
	h.t.Helper()
	env, err := h.cfg.Remotes.Env(h.defs)
	if err != nil {
		h.t.Fatal(err)
	}
	cmd := exec.CommandContext(h.t.Context(), h.cfg.RclonePath, append([]string{"--config="}, args...)...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		h.t.Fatalf("rclone %v: %v\n%s", args, err, stderr.String())
	}
	return string(out)
}

// objects lists every object below path on the remote "minio", sorted.
func (h *harness) objects(path string) []string {
	h.t.Helper()
	names := strings.Fields(h.rclone("lsf", "-R", "--files-only", "--", "minio:"+path))
	slices.Sort(names)
	return names
}

// TestIntegrationMinIODeleteData deletes repositories in a bucket with the
// real restic and rclone: only restic's own objects go, whatever else
// shares the folder or the bucket stays.
func TestIntegrationMinIODeleteData(t *testing.T) {
	h := newMinIOHarness(t)
	at := func(path string) *syncrv1.ResticRepository {
		return &syncrv1.ResticRepository{Location: &syncrv1.Location{Remote: "minio", Path: path}, Password: itPassword}
	}
	note := h.local("note.txt")
	writeFile(t, note, []byte("not restic's"))

	// A repository that shares its folder, and the bucket, with other
	// objects.
	shared := at("syncr-e2e/shared/repo")
	sharedID := h.newRepository(shared)
	h.rclone("copyto", "--", note, "minio:syncr-e2e/shared/repo/README.txt")
	h.rclone("copyto", "--", note, "minio:syncr-e2e/shared/notes.txt")
	// A second repository alone in its folder.
	solo := at("syncr-e2e/solo/repo")
	soloID := h.newRepository(solo)
	// A third one inside the first one's data folder, and an object restic
	// did not write in its keys folder.
	nested := at("syncr-e2e/shared/repo/data/nested")
	h.newRepository(nested)
	h.rclone("copyto", "--", note, "minio:syncr-e2e/shared/repo/keys/notes.txt")
	nestedObjects := h.objects("syncr-e2e/shared/repo/data/nested")
	before := h.objects("syncr-e2e")

	// The wrong expected ID deletes nothing.
	_, _, err := h.deleteData(shared, soloID, true)
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_MISMATCH)
	if got := h.objects("syncr-e2e"); !slices.Equal(got, before) {
		t.Fatalf("a mismatching deletion changed the bucket: %d objects, were %d", len(got), len(before))
	}

	res, snaps, err := h.deleteData(shared, sharedID, true)
	if err != nil {
		t.Fatal(err)
	}
	out := res.Output.GetResticDeleteData()
	if out.GetDirectoryRemoved() || out.GetAlreadyGone() || out.GetDeletedObjects() < 4 ||
		!slices.Equal(out.GetRemovedParts(), []string{"snapshots", "index", "data", "keys", "config"}) && !slices.Equal(out.GetRemovedParts(), []string{"locks", "snapshots", "index", "data", "keys", "config"}) {
		t.Errorf("output = %v", out)
	}
	if len(snaps) == 0 || res.Stats.Deletes != out.GetDeletedObjects() {
		t.Errorf("stats = %+v with %d progress snapshots", res.Stats, len(snaps))
	}
	if got := h.objects("syncr-e2e/shared/repo/data/nested"); !slices.Equal(got, nestedObjects) {
		t.Errorf("the nested repository changed: %v, was %v", got, nestedObjects)
	}
	var rest []string
	for _, o := range h.objects("syncr-e2e/shared") {
		if !strings.HasPrefix(o, "repo/data/nested/") {
			rest = append(rest, o)
		}
	}
	if want := []string{"notes.txt", "repo/README.txt", "repo/keys/notes.txt"}; !slices.Equal(rest, want) {
		t.Errorf("left in the shared folder: %v, want %v and the nested repository", rest, want)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "keys/notes.txt") || !strings.Contains(res.Warnings[0], "data/nested/") {
		t.Errorf("warnings = %q, want what was kept", res.Warnings)
	}
	// The other repositories are untouched and still open.
	for _, other := range []*syncrv1.ResticRepository{solo, nested} {
		if _, err := h.prober().Config(t.Context(), itProbeID, h.remotes(), &syncrv1.ProbeResticConfig{Repository: other}); err != nil {
			t.Errorf("repository %s no longer opens: %v", other.GetLocation().GetPath(), err)
		}
	}

	// Again: nothing is left to delete, which is done only for a retry.
	res, _, err = h.resumeDeleteData(shared, sharedID, true)
	if err != nil || !res.Output.GetResticDeleteData().GetAlreadyGone() {
		t.Errorf("second run = %v, %v; want already_gone", res.Output, err)
	}
	_, _, err = h.deleteData(shared, sharedID, true)
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_NOT_FOUND)

	// Resume: an attempt that stopped after keys/ finishes with the config.
	for _, key := range h.objects("syncr-e2e/solo/repo/keys") {
		h.rclone("deletefile", "--", "minio:syncr-e2e/solo/repo/keys/"+key)
	}
	for _, part := range []string{"locks", "snapshots", "index", "data"} {
		h.rclone("delete", "--", "minio:syncr-e2e/solo/repo/"+part)
	}
	res, _, err = h.resumeDeleteData(solo, soloID, true)
	if err != nil {
		t.Fatal(err)
	}
	if out := res.Output.GetResticDeleteData(); !slices.Equal(out.GetRemovedParts(), []string{"config"}) || !out.GetDirectoryRemoved() {
		t.Errorf("resumed output = %v, want only the config deleted and the folder gone", out)
	}
	if got := h.objects("syncr-e2e/solo"); len(got) != 0 {
		t.Errorf("left of the resumed repository: %v", got)
	}
}
