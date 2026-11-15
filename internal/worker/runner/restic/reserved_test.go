package restic

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// reservedEnv is a test env whose Config.Reserved holds a marked data
// directory, a config file and a tools directory, all inside the root.
type reservedEnv struct {
	*testEnv
	data, config, tools string
}

func newReservedEnv(t *testing.T) *reservedEnv {
	t.Helper()
	re := &reservedEnv{}
	re.testEnv = newTestEnv(t, func(c *Config) {
		policy, ok := c.Paths.(rootPolicy)
		if !ok {
			t.Fatalf("paths = %T", c.Paths)
		}
		root := policy.root
		re.data = filepath.Join(root, "var", "lib", "syncr")
		re.config = filepath.Join(root, "etc[1]", "worker.yaml")
		re.tools = filepath.Join(root, "opt", "tools")
		c.Reserved = []string{re.data, re.config, re.tools}
		c.Marked = map[string]string{re.data: ".syncr-data"}
	})
	putFile(t, re.config, []byte("worker_secret: x"))
	for _, d := range []string{re.data, re.tools, re.local("srv")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return re
}

// keepOutFlags returns the --exclude and --exclude-if-present flags of
// the last restic run.
func keepOutFlags(te *testEnv) []string {
	var out []string
	for _, a := range te.sup.spec.Args {
		if strings.HasPrefix(a, "--exclude") {
			out = append(out, a)
		}
	}
	return out
}

// TestBackupLeavesReservedOut: a backup of a folder that holds what the
// worker keeps for its own use leaves the marked data directory out with
// --exclude-if-present and the rest with literal --exclude patterns of
// their paths below the backed-up folder; on Windows such a backup is
// refused unless only marked folders are held. Folders beside them are
// unchanged.
func TestBackupLeavesReservedOut(t *testing.T) {
	t.Parallel()
	re := newReservedEnv(t)
	setGOOS(t, re.testEnv, "linux")
	for _, tt := range []struct {
		path string
		want []string
	}{
		{re.local("srv"), nil},
		{re.local("var"), []string{"--exclude-if-present=.syncr-data"}},
		{re.local(), []string{"--exclude-if-present=.syncr-data",
			"--exclude=" + includePattern(filepath.Join(re.local(), "etc[1]", "worker.yaml"), "linux"),
			"--exclude=" + includePattern(re.tools, "linux")}},
	} {
		re.sup.exited(0)
		re.sup.stdout = fixture(t, "backup_summary.out")
		if _, _, err := re.run(backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{tt.path}})); err != nil {
			t.Fatal(err)
		}
		if got := keepOutFlags(re.testEnv); !slices.Equal(got, tt.want) {
			t.Errorf("backup of %s: flags %q, want %q", tt.path, got, tt.want)
		}
	}
	if got, want := includePattern("/srv/etc[1]/worker.yaml", "linux"), "/srv/etc[[]1]/worker.yaml"; got != want {
		t.Errorf("pattern %q, want %q", got, want)
	}

	setGOOS(t, re.testEnv, "windows")
	re.sup.called = 0
	_, _, err := re.run(backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{re.local()}}))
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED)
	if !strings.Contains(err.Error(), "on Windows a backup cannot be kept out of it exactly") || re.sup.called > 0 {
		t.Errorf("windows: %v (restic ran %d times)", err, re.sup.called)
	}
}

// TestRestoreKeepsOutOfReserved: a restore into a folder that holds a
// reserved file excludes the file's path relative to the target, like the
// folder of backups of remotes.
func TestRestoreKeepsOutOfReserved(t *testing.T) {
	t.Parallel()
	re := newReservedEnv(t)
	setGOOS(t, re.testEnv, "linux")
	re.sup.exited(0)
	if _, _, err := re.run(restoreSpec(re.testEnv, &syncrv1.ResticRestore{SnapshotId: "latest", Target: re.local("etc[1]")})); err != nil {
		t.Fatal(err)
	}
	if got, want := keepOutFlags(re.testEnv), []string{"--exclude=/worker.yaml"}; !slices.Equal(got, want) {
		t.Errorf("flags %q, want %q", got, want)
	}
}

// TestResticClaims: a restore takes its target exclusively and fails with
// TEMPORARY while another job uses an overlapping folder; a backup shares
// its paths and its local repository, and fails with TEMPORARY while a
// restore uses an overlapping folder. The folders are given back when the
// job ends.
func TestResticClaims(t *testing.T) {
	t.Parallel()
	claims := &localfs.Claims{}
	te := newTestEnv(t, func(c *Config) { c.Claims = claims })
	for _, d := range []string{te.local("target", "a"), te.local("docs"), te.local("repo")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	restore := restoreSpec(te, &syncrv1.ResticRestore{SnapshotId: "latest", Target: te.local("target")})
	backupOf := func(repo *syncrv1.ResticRepository, path string) *syncrv1.JobSpec {
		return backupSpec(repo, &syncrv1.ResticBackup{Paths: []string{path}})
	}
	temporary := func(name string, spec *syncrv1.JobSpec) {
		t.Helper()
		te.sup.called = 0
		_, _, err := te.run(spec)
		if runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_TEMPORARY || !errors.Is(err, localfs.ErrInUse) || te.sup.called > 0 {
			t.Errorf("%s: %v (restic ran %d times)", name, err, te.sup.called)
		}
	}

	other := claims.Take("rclone_transfer")
	if err := other.Add(te.local("target", "a"), false); err != nil {
		t.Fatal(err)
	}
	temporary("restore beside a transfer inside the target", restore)
	other.Release()

	held := claims.Take("restic_restore")
	if err := held.Add(te.local("target"), true); err != nil {
		t.Fatal(err)
	}
	temporary("backup of the restore's target", backupOf(remoteRepo(), te.local("target", "a")))
	temporary("backup into a local repository in the restore's target", backupOf(localRepo(te.local("target", "repo")), te.local("docs")))
	te.sup.exited(0)
	te.sup.stdout = fixture(t, "backup_summary.out")
	if _, _, err := te.run(backupOf(localRepo(te.local("repo")), te.local("docs"))); err != nil {
		t.Errorf("backup beside the restore: %v", err)
	}
	held.Release()

	te.sup.exited(0)
	te.sup.stdout = nil
	if _, _, err := te.run(restore); err != nil {
		t.Errorf("restore: %v", err)
	}
	check := claims.Take("check")
	defer check.Release()
	if err := check.Add(te.local(), true); err != nil {
		t.Errorf("after the jobs: %v", err)
	}
}

// TestLocalRepositoryLinks: restic writes into a local repository through
// the links to folders it finds there, so a job on a repository holding a
// link that leads outside local_roots fails with PATH_NOT_ALLOWED before
// restic starts. Reads with --no-lock (the snapshot and listing probes)
// write nothing and are not checked.
func TestLocalRepositoryLinks(t *testing.T) {
	t.Parallel()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	repo := filepath.Join(root, "repo")
	for _, d := range []string{repo, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(repo, "data")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks: %v", err)
		}
		t.Fatal(err)
	}
	policy, err := localfs.NewPolicy([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	te := newTestEnv(t, func(c *Config) { c.Paths = policy })
	te.sup.exited(0)
	_, _, err = te.run(backupSpec(localRepo(repo), &syncrv1.ResticBackup{Paths: []string{root}}))
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED)
	if !errors.Is(err, localfs.ErrLink) || te.sup.called > 0 {
		t.Errorf("backup: %v (restic ran %d times)", err, te.sup.called)
	}

	p, err := NewProber(te.cfg)
	if err != nil {
		t.Fatal(err)
	}
	te.sup.stdout = []string{"[]"}
	if _, err := p.Snapshots(t.Context(), "probe-1", nil, &syncrv1.ProbeResticSnapshots{Repository: localRepo(repo)}); err != nil {
		t.Errorf("snapshots probe: %v", err)
	}
}
