//go:build integration

package restic

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/sourcestate"
)

// TestIntegrationRestoreKeepsOutOfSources restores, with the pinned
// restic, a snapshot whose tree holds files where the worker's folder of
// backups of remotes lies, into a target that holds that folder: a whole
// snapshot (Syncr's --exclude), and a subfolder with a selected item beside
// the folder (no exclude: restic refuses one with --include). Nothing in
// the folder changes, the rest is restored, and a selected item that holds
// the folder is refused before restic runs. On Windows every such restore
// is refused.
func TestIntegrationRestoreKeepsOutOfSources(t *testing.T) {
	h := newHarness(t)
	sources := h.local("var", "lib", "syncr", "sources")
	store, err := sourcestate.New(sourcestate.Config{Root: sources})
	if err != nil {
		t.Fatal(err)
	}
	h.cfg.RemoteSources = RemoteSources{Store: store, Support: &syncrv1.RemoteSourceSupport{Mirror: true}, MountTimeout: 30 * time.Second}
	rs, err := New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		h.runners[r.Kind()] = r
	}
	remote := localSource(h)
	h.init(itPassword)
	// The snapshot: a remote folder (its tree starts at the folder's
	// contents) with files where the worker's mirrors lie.
	const otherKey = "fedcba9876543210fedcba9876543210"
	for rel, data := range map[string]string{
		"tree/var/lib/syncr/sources/mirror/" + otherKey + "/data/photo.txt":       "attacker",
		"tree/var/lib/syncr/sources/mirror/" + otherKey + "/last-run":             "2099-01-01T00:00:00Z",
		"tree/var/lib/syncr/sources/mount/" + otherKey + "/planted.txt":           "planted",
		"tree/var/lib/other/x.txt":                                                "x",
		"tree/srv/x/var/lib/syncr/sources/mirror/" + otherKey + "/data/photo.txt": "attacker",
		"tree/srv/x/var/lib/other/y.txt":                                          "y",
	} {
		writeFile(t, filepath.Join(remote, filepath.FromSlash(rel)), []byte(data))
	}
	id := h.mustRemoteRun("src", "tree").GetSnapshotId()
	// Another user's mirror and an unmounted mount point.
	writeFile(t, filepath.Join(sources, "mirror", otherKey, "data", "photo.txt"), []byte("victim"))
	writeFile(t, filepath.Join(sources, "mirror", otherKey, "last-run"), []byte("2026-11-01T00:00:00Z"))
	if err := os.MkdirAll(filepath.Join(sources, "mount", otherKey), 0o700); err != nil {
		t.Fatal(err)
	}
	before := dirState(t, sources)

	restore := func(sub string, paths ...string) error {
		_, _, err := h.run(t.Context(), func(s *syncrv1.JobSpec) {
			s.Operation = &syncrv1.JobSpec_ResticRestore{ResticRestore: &syncrv1.ResticRestore{
				Repository: h.repo(itPassword), SnapshotId: id, Target: h.root, Subfolder: sub, Paths: paths,
				Overwrite: syncrv1.ResticRestoreOverwrite_RESTIC_RESTORE_OVERWRITE_ALWAYS,
			}}
		})
		return err
	}
	unchanged := func(when string) {
		t.Helper()
		if after := dirState(t, sources); !slices.Equal(after, before) {
			t.Errorf("%s: the folder changed:\nbefore %q\nafter  %q", when, before, after)
		}
	}

	if runtime.GOOS == "windows" {
		// No literal pattern covers every name that opens the folder on
		// Windows: every restore into a target that holds it is refused.
		for _, paths := range [][]string{nil, {"/srv/x/var/lib/other"}} {
			err := restore(map[bool]string{false: "", true: "/srv/x"}[paths != nil], paths...)
			mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED)
		}
		unchanged("refused restores")
		return
	}

	// 1. The whole snapshot.
	if err := restore(""); err != nil {
		t.Fatal(err)
	}
	unchanged("whole snapshot")
	if data, err := os.ReadFile(h.local("var", "lib", "other", "x.txt")); err != nil || string(data) != "x" {
		t.Errorf("whole snapshot: x.txt = %q, %v", data, err)
	}

	// 2. A subfolder and a selected item beside the folder.
	if err := restore("/srv/x", "/srv/x/var/lib/other"); err != nil {
		t.Fatal(err)
	}
	unchanged("subfolder and paths")
	if data, err := os.ReadFile(h.local("var", "lib", "other", "y.txt")); err != nil || string(data) != "y" {
		t.Errorf("subfolder and paths: y.txt = %q, %v", data, err)
	}

	// 3. A selected item that holds the folder.
	err = restore("/srv/x", "/srv/x/var")
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED)
	unchanged("a selected item that holds it")
	t.Logf("refused: %v", err)
}

// dirState lists every entry below dir with its mode and, for a file, its
// content hash and modification time.
func dirState(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		entry := filepath.ToSlash(rel) + " " + info.Mode().String()
		if !d.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			entry += " " + hex.EncodeToString(sum[:8]) + " " + info.ModTime().UTC().String()
		}
		out = append(out, entry)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
