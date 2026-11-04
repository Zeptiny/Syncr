//go:build integration

package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
	"github.com/Zeptiny/Syncr/internal/worker/runner/rclone"
)

// TestIntegrationRcloneOverActiveLink runs a real rclone copy (local to
// local, SYNCR_TEST_RCLONE) on a worker enrolled and connected over HTTPS.
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
	id, secret := c.enrollActive("rclone")
	startActive(t, rn, activeOptions(c, id, secret))
	job := c.run(id, &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{
		Mode:        syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY,
		Source:      &syncrv1.Location{Path: src},
		Destination: &syncrv1.Location{Path: dst},
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
