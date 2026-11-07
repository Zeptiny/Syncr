//go:build integration

package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
	"github.com/Zeptiny/Syncr/internal/worker/runner/rclone"
)

// TestIntegrationRcloneOverActiveLink runs a real rclone copy (a local
// remote to a local path, SYNCR_TEST_RCLONE) on a worker enrolled and
// connected over HTTPS, with the spec built from the job's definition.
func TestIntegrationRcloneOverActiveLink(t *testing.T) {
	bin := os.Getenv("SYNCR_TEST_RCLONE")
	if bin == "" {
		t.Skip("SYNCR_TEST_RCLONE not set")
	}
	root := t.TempDir()
	policy, err := localfs.NewPolicy([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	rn, err := rclone.New(rclone.Config{
		Binary:     bin,
		Supervisor: proc.New(proc.Options{GracePeriod: 5 * time.Second}),
		Env:        rcloneenv.NewBuilder(policy),
		Paths:      policy,
		ScratchDir: filepath.Join(t.TempDir(), "scratch"),
	})
	if err != nil {
		t.Fatal(err)
	}
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	files := map[string][]byte{
		"a.txt":                       []byte("alpha\n"),
		filepath.Join("sub", "b.bin"): bytes.Repeat([]byte{7}, 2<<20),
	}
	for name, content := range files {
		p := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	c := newController(t)
	// The source is a local remote rooted at the worker's root; the
	// destination a path on the worker.
	local := fixedRemoteID("00000000-0000-7000-8000-00000000c002")
	c.addRemote(local, "Worker disk", backend.Rclone{Root: root})
	id, secret := c.enrollActive("rclone")
	startActive(t, rn, activeOptions(c, id, secret))
	job := c.run(id, &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransferDefinition{
		Mode:        syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY,
		Source:      &syncrv1.JobLocation{RemoteId: local.String(), Path: "src"},
		Destination: &syncrv1.JobLocation{Path: dst},
	}}})
	j := c.waitStatus(job, domain.JobSucceeded)
	if j.Stats.Files != int64(len(files)) || j.Stats.Bytes < 2<<20 || j.ExitCode != 0 {
		t.Errorf("job stats %+v exit %d", j.Stats, j.ExitCode)
	}
	for name, content := range files {
		got, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil || !bytes.Equal(got, content) {
			t.Errorf("%s: %d bytes copied, %v", name, len(got), err)
		}
	}
}

// TestIntegrationRcloneFromUnion copies from a union of two local members
// (one read-only) with a real rclone: the union and its upstreams are built
// by the job kind and must pass the worker's checks.
func TestIntegrationRcloneFromUnion(t *testing.T) {
	bin := os.Getenv("SYNCR_TEST_RCLONE")
	if bin == "" {
		t.Skip("SYNCR_TEST_RCLONE not set")
	}
	root := t.TempDir()
	policy, err := localfs.NewPolicy([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	rn, err := rclone.New(rclone.Config{
		Binary:     bin,
		Supervisor: proc.New(proc.Options{GracePeriod: 5 * time.Second}),
		Env:        rcloneenv.NewBuilder(policy),
		Paths:      policy,
		ScratchDir: filepath.Join(t.TempDir(), "scratch"),
	})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{filepath.Join("disk1", "a.txt"): "from disk 1\n", filepath.Join("disk2", "b.txt"): "from disk 2\n"}
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	c := newController(t)
	disk := fixedRemoteID("00000000-0000-7000-8000-00000000c002")
	pool := fixedRemoteID("00000000-0000-7000-8000-00000000c003")
	c.addRemote(disk, "Disks", backend.Rclone{Root: root})
	c.addRemote(pool, "Pool", backend.Rclone{Backend: "union", Options: map[string]string{}})
	c.remotes.mu.Lock()
	r := c.remotes.resolved[pool]
	r.Members = []remotes.Member{{RemoteID: disk, Path: "disk1", Mode: backend.UnionReadWrite}, {RemoteID: disk, Path: "disk2", Mode: backend.UnionReadOnly}}
	c.remotes.resolved[pool] = r
	c.remotes.resolved[disk] = remotes.Resolved{ID: disk, Name: "Disks", Type: "local", Rclone: backend.Rclone{Root: root}}
	c.remotes.mu.Unlock()

	id, secret := c.enrollActive("rclone")
	startActive(t, rn, activeOptions(c, id, secret))
	dst := filepath.Join(root, "dst")
	job := c.run(id, &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransferDefinition{
		Mode:        syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY,
		Source:      &syncrv1.JobLocation{RemoteId: pool.String()},
		Destination: &syncrv1.JobLocation{Path: dst},
	}}})
	j := c.waitJob(job, "finished", func(j jobs.Job) bool { return j.Status.Terminal() })
	if j.Status != domain.JobSucceeded || j.Stats.Files != 2 {
		t.Fatalf("job %s %+v stats %+v", j.Status, j.Error, j.Stats)
	}
	for name, content := range files {
		got, err := os.ReadFile(filepath.Join(dst, filepath.Base(name)))
		if err != nil || string(got) != content {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
}
