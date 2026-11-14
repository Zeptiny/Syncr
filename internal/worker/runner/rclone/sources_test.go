package rclone

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// TestTransferKeepsOutOfSources: a transfer whose local source or
// destination holds the worker's folder of backups of remotes gets an
// anchored exclude rule for each form of the folder's path relative to
// that side (as written and through symbolic links), before the user's
// filters; any other transfer is unchanged, the marker flag is gone, and a
// path inside the folder is refused by the policy.
func TestTransferKeepsOutOfSources(t *testing.T) {
	t.Parallel()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "root")
	// The data directory is reached through a link: root/var/lib/syncr ->
	// root/disk/syncr, so the folder has two forms.
	realDir := filepath.Join(root, "disk", "syncr")
	for _, d := range []string{filepath.Join(root, "srv"), filepath.Join(realDir, "sources"), filepath.Join(root, "var", "lib")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(realDir, filepath.Join(root, "var", "lib", "syncr")); err != nil {
		t.Skipf("symlinks: %v", err)
	}
	sources := filepath.Join(root, "var", "lib", "syncr", "sources")
	policy, err := localfs.NewPolicy([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if policy, err = policy.Reserve(sources); err != nil {
		t.Fatal(err)
	}
	userFilters := []*syncrv1.FilterRule{
		{Action: syncrv1.FilterAction_FILTER_ACTION_INCLUDE, Pattern: "*.txt"},
		{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: "**"},
	}
	remote := &syncrv1.Location{Remote: "bk", Path: "b"}
	for _, tt := range []struct {
		name     string
		src, dst *syncrv1.Location
		want     []string
		code     syncrv1.JobErrorCode
	}{
		{name: "neither side holds it", src: &syncrv1.Location{Path: filepath.Join(root, "srv")}, dst: remote},
		{name: "source is the root", src: &syncrv1.Location{Path: root}, dst: remote,
			want: []string{"- /var/lib/syncr/sources/**", "- /disk/syncr/sources/**"}},
		{name: "destination as written", src: remote, dst: &syncrv1.Location{Path: filepath.Join(root, "var")},
			want: []string{"- /lib/syncr/sources/**"}},
		{name: "destination through the link", src: remote, dst: &syncrv1.Location{Path: filepath.Join(root, "var", "lib", "syncr")},
			want: []string{"- /sources/**"}},
		{name: "destination resolved only", src: remote, dst: &syncrv1.Location{Path: filepath.Join(root, "disk")},
			want: []string{"- /syncr/sources/**"}},
		{name: "both sides", src: &syncrv1.Location{Path: filepath.Join(root, "disk", "syncr")}, dst: &syncrv1.Location{Path: filepath.Join(root, "var", "lib", "syncr")},
			want: []string{"- /sources/**"}},
		{name: "inside the folder", src: &syncrv1.Location{Path: filepath.Join(sources, "mirror")}, dst: remote,
			code: syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED},
		{name: "inside the folder, resolved", src: remote, dst: &syncrv1.Location{Path: filepath.Join(realDir, "sources", "mount")},
			code: syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sup := &fakeSupervisor{}
			r, err := New(Config{
				Binary: "/opt/syncr/rclone", Supervisor: sup, Env: rcloneenv.NewBuilder(policy), Paths: policy,
				ScratchDir: filepath.Join(base, "scratch"), SourcesDir: sources,
			})
			if err != nil {
				t.Fatal(err)
			}
			r.goos = "linux" // the rules' spelling on Windows: TestExcludeRule
			job := transferJob(tt.src, tt.dst, sftpRemote("bk", "pw"))
			job.Spec.GetRcloneTransfer().Options = &syncrv1.RcloneTransferOptions{Filters: userFilters}
			_, err = r.Run(t.Context(), job, nil)
			if tt.code != 0 {
				if code := runner.CodeOf(err); code != tt.code || sup.called {
					t.Errorf("%v (ran %v)", err, sup.called)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			args := sup.spec.Args
			var filters []string
			for _, a := range args {
				if f, ok := strings.CutPrefix(a, "--filter="); ok {
					filters = append(filters, f)
				}
			}
			want := append(slices.Clone(tt.want), "+ *.txt", "- **")
			if !slices.Equal(filters, want) || slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--exclude-if-present") }) {
				t.Errorf("filters = %q, want %q (args %q)", filters, want, args)
			}
			if i := slices.Index(args, "--"); i < 0 || slices.IndexFunc(args, func(a string) bool { return strings.HasPrefix(a, "--filter=") }) > i {
				t.Errorf("args = %q", args)
			}
		})
	}
}

// TestExcludeRule: the rule of a relative path, escaped for rclone's
// globs; on Windows case-insensitive (every rune of the letter's case
// orbit, such as the long s for "s"), with short names as alternatives.
// The patterns were checked against rclone v1.75.2 (lsf with the rule
// lists none of the folder's files and every sibling's).
func TestExcludeRule(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		rel  localfs.RelPath
		goos string
		want string
	}{
		{localfs.RelPath{{"var"}, {"lib"}, {"syncr"}, {"sources"}}, "linux", "- /var/lib/syncr/sources/**"},
		{localfs.RelPath{{"we[i]rd*{x},y"}, {"sources"}}, "linux", `- /we\[i\]rd\*\{x\}\,y/sources/**`},
		{localfs.RelPath{{`back\slash?`}, {"sources"}}, "linux", `- /back\\slash\?/sources/**`},
		{localfs.RelPath{{"Syncr"}, {"sources"}}, "windows", "- /[Ssſ][yY][nN][cC][rR]/[sſS][oO][uU][rR][cC][eE][sſS]/**"},
		{localfs.RelPath{{"ProgramData", "PROGRA~3"}, {"Syncr"}, {"sources"}}, "windows",
			"- /{[Pp][rR][oO][gG][rR][aA][mM][Dd][aA][tT][aA],[Pp][Rr][Oo][Gg][Rr][Aa]~3}/[Ssſ][yY][nN][cC][rR]/[sſS][oO][uU][rR][cC][eE][sſS]/**"},
		{localfs.RelPath{{"a,b", "AB~1"}, {"sources"}}, "windows", `- /{[aA]\,[bB],[Aa][Bb]~1}/[sſS][oO][uU][rR][cC][eE][sſS]/**`},
		{localfs.RelPath{{"Data 1"}, {"ü"}}, "windows", "- /[Dd][aA][tT][aA] 1/[üÜ]/**"},
		{localfs.RelPath{{"x", "x"}}, "linux", "- /x/**"},
	} {
		if got := excludeRule(tt.rel, tt.goos); got != tt.want {
			t.Errorf("excludeRule(%q, %s) = %q, want %q", tt.rel, tt.goos, got, tt.want)
		}
	}
}
