package restic

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/sourcestate"
)

// TestRestoreKeepsOutOfSources: a restore into a target that holds the
// worker's folder of backups of remotes leaves out what would land there
// (an --exclude per form of the folder's path relative to the target, as
// written and through a link); a restore of selected items goes on when no
// item would land in or over the folder (restic refuses --exclude with
// --include) and is refused otherwise, as are include patterns and any
// such restore on Windows. Targets beside the folder are unchanged.
func TestRestoreKeepsOutOfSources(t *testing.T) {
	t.Parallel()
	notAllowed := syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED
	for _, tt := range []struct {
		name, goos, target, sub string
		paths, includes         []string
		excludes                []string // Syncr's, then the user's
		refused                 string
	}{
		{name: "whole snapshot into the root", target: ".", excludes: []string{"/var/lib/syncr/sources", "/disk/syncr/sources"}},
		{name: "into the data directory", target: "var/lib/syncr", excludes: []string{"/sources"}},
		{name: "into the data directory's real folder", target: "disk", excludes: []string{"/syncr/sources"}},
		{name: "a subfolder into the data directory's parent", target: "var/lib", sub: "/srv/x", excludes: []string{"/syncr/sources"}},
		{name: "beside it", target: "srv"},
		{name: "beside it, on windows", goos: "windows", target: "srv"},
		{name: "selected items beside it", target: ".", paths: []string{"/srv/docs", "/var/lib/other", "/var/lib/syncr/bisync"}},
		{name: "selected items of a subfolder beside it", target: ".", sub: "/srv/x", paths: []string{"/srv/x/var/lib/other"}},
		{name: "a selected item that holds it", target: ".", paths: []string{"/srv/docs", "/var"}, refused: `the selected item "/var" would be restored into it or over it`},
		{name: "a selected item inside it", target: ".", paths: []string{"/var/lib/syncr/sources/mirror"}, refused: "would be restored into it or over it"},
		{name: "a selected item that is it, resolved", target: ".", paths: []string{"/disk/syncr/sources"}, refused: "would be restored into it or over it"},
		{name: "a selected item of a subfolder that holds it", target: ".", sub: "/srv/x", paths: []string{"/srv/x/var/lib"},
			refused: `the selected item "/var/lib"`},
		{name: "include patterns", target: ".", includes: []string{"*.jpg"}, refused: "include patterns"},
		{name: "windows", goos: "windows", target: ".", refused: "on Windows a restore cannot be kept out of it exactly"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var store *sourcestate.Store
			te := newTestEnv(t, func(c *Config) {
				policy, ok := c.Paths.(rootPolicy)
				if !ok {
					t.Fatalf("paths = %T", c.Paths)
				}
				root := policy.root
				// root/var/lib/syncr is a link to root/disk/syncr.
				for _, d := range []string{filepath.Join(root, "disk", "syncr", "sources"), filepath.Join(root, "var", "lib"), filepath.Join(root, "srv")} {
					if err := os.MkdirAll(d, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(filepath.Join(root, "disk", "syncr"), filepath.Join(root, "var", "lib", "syncr")); err != nil {
					t.Skipf("symlinks: %v", err)
				}
				var err error
				if store, err = sourcestate.New(sourcestate.Config{Root: filepath.Join(root, "var", "lib", "syncr", "sources")}); err != nil {
					t.Fatal(err)
				}
				c.RemoteSources.Store = store
			})
			if tt.goos != "" {
				setGOOS(t, te, tt.goos)
			} else {
				setGOOS(t, te, "linux")
			}
			te.sup.exited(0)
			op := &syncrv1.ResticRestore{SnapshotId: "latest", Target: te.local(filepath.FromSlash(tt.target)), Subfolder: tt.sub,
				Paths: tt.paths, Includes: tt.includes, Excludes: []string{"*.log"}}
			if len(tt.paths) > 0 || len(tt.includes) > 0 {
				op.Excludes = nil // restic refuses both
			}
			_, _, err := te.run(restoreSpec(te, op))
			if tt.refused != "" {
				mustFail(t, err, notAllowed)
				if !strings.Contains(err.Error(), tt.refused) || !strings.Contains(err.Error(), store.Root()) || te.sup.called > 0 {
					t.Errorf("error = %v (restic ran %d times)", err, te.sup.called)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var excludes []string
			for _, a := range te.sup.spec.Args {
				if x, ok := strings.CutPrefix(a, "--exclude="); ok {
					excludes = append(excludes, x)
				}
			}
			want := slices.Clone(tt.excludes)
			if len(tt.paths) == 0 && len(tt.includes) == 0 {
				want = append(want, "*.log")
			}
			if !slices.Equal(excludes, want) {
				t.Errorf("excludes = %q, want %q (args %q)", excludes, want, te.sup.spec.Args)
			}
		})
	}
}
