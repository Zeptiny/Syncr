//go:build integration

package restic

import (
	"os"
	"path/filepath"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/ftptest"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
)

// TestIntegrationFTPLifecycle runs the full lifecycle (init, backup,
// snapshots, restore, forget, prune, check) on a repository on an FTP
// server (rclone serve ftp), through restic's rclone backend, like on a
// real worker: the remote is defined by the real rcloneenv.Builder and
// local paths are checked by the real localfs.Policy.
func TestIntegrationFTPLifecycle(t *testing.T) {
	served := filepath.Join(t.TempDir(), "served")
	if err := os.MkdirAll(served, 0o700); err != nil {
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
	srv := ftptest.Start(t, served, "syncr restic", `p'a"s,s w:rd`)
	h.defs = []*syncrv1.RemoteDefinition{srv.Remote("ftpbox")}
	h.loc = &syncrv1.Location{Remote: "ftpbox", Path: "backups/repo"}
	runLifecycle(t, h)

	// The repository is on the server, in the folder the remote names.
	if _, err := os.Stat(filepath.Join(served, "backups", "repo", "config")); err != nil {
		t.Errorf("no repository config on the server: %v", err)
	}
}
