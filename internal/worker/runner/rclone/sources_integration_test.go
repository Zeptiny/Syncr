//go:build integration

package rclone

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
)

// TestIntegrationTransfersKeepOutOfSources: copy, sync and move with the
// pinned rclone, into a destination that holds the worker's folder of
// backups of remotes, from a source that holds a tree planted where that
// folder lies (with and without the user's own filters), and out of a
// source that holds it: nothing in the folder changes, nothing of it is
// copied, and everything else is transferred.
func TestIntegrationTransfersKeepOutOfSources(t *testing.T) {
	it := newITest(t)
	policy, err := localfs.NewPolicy([]string{it.root})
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]syncrv1.RcloneTransferMode{
		"copy": syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY,
		"sync": syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_SYNC,
		"move": syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_MOVE,
	}
	userFilters := []*syncrv1.FilterRule{
		{Action: syncrv1.FilterAction_FILTER_ACTION_INCLUDE, Pattern: "*.txt"},
		{Action: syncrv1.FilterAction_FILTER_ACTION_INCLUDE, Pattern: "*/"},
		{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: "**"},
	}
	for _, name := range []string{"copy", "sync", "move"} {
		for _, filtered := range []bool{false, true} {
			// lib/ plays /var/lib: it holds syncr/sources, the folder.
			lib := filepath.Join(it.root, name, "lib")
			sources := filepath.Join(lib, "syncr", "sources")
			src := filepath.Join(it.root, name, "src")
			if filtered {
				lib, sources, src = lib+"-f", filepath.Join(lib+"-f", "syncr", "sources"), src+"-f"
			}
			plantSources(t, sources)
			for rel, data := range map[string]string{
				"syncr/sources/mirror/0123/data/photo.txt": "attacker",
				"syncr/sources/mirror/0123/data/new.txt":   "new",
				"syncr/sources/mirror/0123/last-run":       "2099-01-01T00:00:00Z",
				"syncr/sources/mount/0123/planted.txt":     "planted",
				"other/x.txt":                              "x",
			} {
				writeTestFile(t, filepath.Join(src, filepath.FromSlash(rel)), data)
			}
			must(t, os.MkdirAll(filepath.Join(src, "syncr", "sources", "mount", "4567"), 0o755))
			before, planted := treeHash(t, sources), treeHash(t, filepath.Join(src, "syncr", "sources"))

			r := it.withSources(t, policy, sources)
			opts := &syncrv1.RcloneTransferOptions{CreateEmptySrcDirs: true}
			if name == "move" {
				opts.DeleteEmptySrcDirs = true
			}
			if filtered {
				opts.Filters = userFilters
			}
			if _, _, err := it.runWith(t, r, modes[name], local(src), local(lib), opts); err != nil {
				t.Fatalf("%s (filters %v): %v", name, filtered, err)
			}
			if after := treeHash(t, sources); !slices.Equal(after, before) {
				t.Errorf("%s (filters %v): the folder changed:\nbefore %q\nafter  %q", name, filtered, before, after)
			}
			if after := treeHash(t, filepath.Join(src, "syncr", "sources")); !slices.Equal(after, planted) {
				t.Errorf("%s (filters %v): the source's planted tree changed: %q", name, filtered, after)
			}
			if data, err := os.ReadFile(filepath.Join(lib, "other", "x.txt")); err != nil || string(data) != "x" {
				t.Errorf("%s (filters %v): the rest was not transferred: %q, %v", name, filtered, data, err)
			}
		}
	}

	// A move out of a folder that holds it (as of /var/lib to elsewhere):
	// the folder stays, nothing of it is copied, its empty mount points
	// are not removed as empty folders.
	lib := filepath.Join(it.root, "out", "lib")
	sources := filepath.Join(lib, "syncr", "sources")
	plantSources(t, sources)
	writeTestFile(t, filepath.Join(lib, "keep", "y.txt"), "y")
	before := treeHash(t, sources)
	dst := filepath.Join(it.root, "out", "dst")
	r := it.withSources(t, policy, sources)
	opts := &syncrv1.RcloneTransferOptions{DeleteEmptySrcDirs: true, CreateEmptySrcDirs: true}
	if _, _, err := it.runWith(t, r, modes["move"], local(lib), local(dst), opts); err != nil {
		t.Fatal(err)
	}
	if after := treeHash(t, sources); !slices.Equal(after, before) {
		t.Errorf("move out: the folder changed:\nbefore %q\nafter  %q", before, after)
	}
	if _, err := os.Stat(filepath.Join(dst, "syncr", "sources")); err == nil {
		t.Error("move out: the folder was copied")
	}
	if data, err := os.ReadFile(filepath.Join(dst, "keep", "y.txt")); err != nil || string(data) != "y" {
		t.Errorf("move out: %q, %v", data, err)
	}

	// A sync from an empty folder into one that holds it deletes
	// everything else, and nothing in the folder.
	empty := filepath.Join(it.root, "empty")
	must(t, os.MkdirAll(empty, 0o755))
	if _, _, err := it.runWith(t, r, modes["sync"], local(empty), local(lib), nil); err != nil {
		t.Fatal(err)
	}
	if after := treeHash(t, sources); !slices.Equal(after, before) {
		t.Errorf("sync from empty: the folder changed: %q", after)
	}
}

// withSources returns a runner like it's, with Reserved set and the path
// policy reserving it.
func (it *itest) withSources(t *testing.T, policy *localfs.Policy, sources string) *Runner {
	t.Helper()
	p, err := policy.Reserve(sources)
	if err != nil {
		t.Fatal(err)
	}
	cfg := it.cfg
	cfg.Paths, cfg.Reserved = p, []string{sources}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// runWith runs a transfer with r, as it.run does with it.runner.
func (it *itest) runWith(t *testing.T, r *Runner, mode syncrv1.RcloneTransferMode, src, dst *syncrv1.Location, opts *syncrv1.RcloneTransferOptions) (any, any, error) {
	t.Helper()
	saved := it.runner
	it.runner = r
	defer func() { it.runner = saved }()
	res, progress, err := it.run(t.Context(), t, mode, src, dst, opts)
	return res, progress, err
}

// plantSources fills a fake folder of backups of remotes as the worker
// keeps it: the marker, a mirror with a file and last-run, a mount point
// (an empty folder) and its lock.
func plantSources(t *testing.T, sources string) {
	t.Helper()
	writeTestFile(t, filepath.Join(sources, ".syncr-sources"), "marker")
	writeTestFile(t, filepath.Join(sources, "mirror", "0123", "data", "photo.txt"), "victim")
	writeTestFile(t, filepath.Join(sources, "mirror", "0123", "last-run"), "2026-11-01T00:00:00Z")
	writeTestFile(t, filepath.Join(sources, "mount", "0123.lock"), "")
	must(t, os.MkdirAll(filepath.Join(sources, "mount", "0123"), 0o700))
}

// treeHash lists every entry below dir with its type, mode and content
// hash.
func treeHash(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		info, err := d.Info()
		if err != nil {
			return err
		}
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

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
