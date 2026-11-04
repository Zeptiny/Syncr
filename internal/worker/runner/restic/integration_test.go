//go:build integration

package restic

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// Integration tests run the real restic and rclone (SYNCR_TEST_RESTIC and
// SYNCR_TEST_RCLONE; make test-integration fetches the pinned tools) through the
// real supervisor, against a local directory exposed as an rclone remote.

func toolPath(t *testing.T, env string) string {
	t.Helper()
	path := os.Getenv(env)
	if path == "" {
		t.Skipf("%s not set", env)
	}
	return path
}

// throttle adds restic global flags to every run, e.g. --limit-upload so a
// small backup lasts long enough to be cancelled.
type throttle struct {
	Supervisor
	flags []string
}

func (s throttle) Run(ctx context.Context, spec proc.Spec) (proc.Result, error) {
	spec.Args = append(append([]string{}, s.flags...), spec.Args...)
	return s.Supervisor.Run(ctx, spec)
}

// localRemotes defines every remote as a local-filesystem rclone remote.
type localRemotes struct{}

func (localRemotes) Env(remotes []*syncrv1.RemoteDefinition) (map[string]string, error) {
	env := map[string]string{}
	for _, r := range remotes {
		prefix := "RCLONE_CONFIG_" + strings.ToUpper(r.GetAlias()) + "_"
		env[prefix+"TYPE"] = r.GetBackendType()
		for k, v := range r.GetOptions() {
			env[prefix+strings.ToUpper(k)] = v
		}
		for k, v := range r.GetSecrets() {
			env[prefix+strings.ToUpper(k)] = v
		}
	}
	return env, nil
}

type harness struct {
	t       *testing.T
	runners map[string]runner.Runner
	root    string // local_roots
	repoDir string // directory behind the "repo" remote
	defs    []*syncrv1.RemoteDefinition
	loc     *syncrv1.Location
}

func newHarness(t *testing.T, flags ...string) *harness {
	t.Helper()
	return newHarnessWith(t, nil, flags...)
}

// newHarnessWith is newHarness with a hook to replace dependencies of the
// runners' Config (for example fakes by the real implementations).
func newHarnessWith(t *testing.T, mutate func(h *harness, cfg *Config), flags ...string) *harness {
	t.Helper()
	restic := toolPath(t, "SYNCR_TEST_RESTIC")
	rclone := toolPath(t, "SYNCR_TEST_RCLONE")
	base := t.TempDir()
	h := &harness{t: t, root: filepath.Join(base, "roots"), repoDir: filepath.Join(base, "repo")}
	h.defs = []*syncrv1.RemoteDefinition{{Alias: "repo", BackendType: "local"}}
	h.loc = &syncrv1.Location{Remote: "repo", Path: h.repoDir}
	for _, d := range []string{h.root, h.repoDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{
		ResticPath:  restic,
		RclonePath:  rclone,
		Supervisor:  throttle{Supervisor: proc.New(proc.Options{GracePeriod: 15 * time.Second}), flags: flags},
		Remotes:     localRemotes{},
		Paths:       rootPolicy{root: h.root},
		ScratchRoot: filepath.Join(base, "scratch"),
		CacheDir:    filepath.Join(base, "cache"),
		// Diagnostics, such as a scratch directory that could not be
		// removed, show up in the test output.
		Logger: slog.New(slog.NewTextHandler(t.Output(), nil)),
	}
	if mutate != nil {
		mutate(h, &cfg)
	}
	rs, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.runners = map[string]runner.Runner{}
	for _, r := range rs {
		h.runners[r.Kind()] = r
	}
	return h
}

func (h *harness) local(elem ...string) string {
	return filepath.Join(append([]string{h.root}, elem...)...)
}

// repo is the repository on the local remote "repo" with the given password.
func (h *harness) repo(password string) *syncrv1.ResticRepository {
	return &syncrv1.ResticRepository{Location: h.loc, Password: password}
}

func (h *harness) remotes() []*syncrv1.RemoteDefinition { return h.defs }

const itPassword = "integration-test-password"

// run executes op and returns the result, the error and the emitted progress.
func (h *harness) run(ctx context.Context, op func(*syncrv1.JobSpec)) (runner.Result, []runner.Progress, error) {
	h.t.Helper()
	spec := &syncrv1.JobSpec{Remotes: h.remotes()}
	op(spec)
	var snaps []runner.Progress
	var mu sync.Mutex
	r := h.runners[kindOf(spec)]
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute) // restic retries backend errors for a long time
	defer cancel()
	res, err := r.Run(ctx, runner.Job{ID: "it-" + kindOf(spec), Spec: spec}, func(p runner.Progress) {
		mu.Lock()
		snaps = append(snaps, p)
		mu.Unlock()
	})
	return res, snaps, err
}

func (h *harness) mustRun(op func(*syncrv1.JobSpec)) (runner.Result, []runner.Progress) {
	h.t.Helper()
	res, snaps, err := h.run(h.t.Context(), op)
	if err != nil {
		h.t.Fatalf("job failed: %v", err)
	}
	return res, snaps
}

func (h *harness) init(password string) {
	h.t.Helper()
	h.mustRun(func(s *syncrv1.JobSpec) {
		s.Operation = &syncrv1.JobSpec_ResticInit{ResticInit: &syncrv1.ResticInit{Repository: h.repo(password)}}
	})
}

func (h *harness) backup(b *syncrv1.ResticBackup) (runner.Result, []runner.Progress) {
	h.t.Helper()
	b.Repository = h.repo(itPassword)
	return h.mustRun(func(s *syncrv1.JobSpec) { s.Operation = &syncrv1.JobSpec_ResticBackup{ResticBackup: b} })
}

func (h *harness) snapshots(sn *syncrv1.ResticSnapshots) []*syncrv1.ResticSnapshot {
	h.t.Helper()
	sn.Repository = h.repo(itPassword)
	res, _ := h.mustRun(func(s *syncrv1.JobSpec) { s.Operation = &syncrv1.JobSpec_ResticSnapshots{ResticSnapshots: sn} })
	return res.Output.GetResticSnapshots().GetSnapshots()
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// restoredPath is where restic puts src below the restore target.
func restoredPath(target, src string) string {
	vol := filepath.VolumeName(src)
	rest := src[len(vol):]
	if vol != "" {
		rest = filepath.Join(strings.TrimSuffix(vol, ":"), rest)
	}
	return filepath.Join(target, rest)
}

// sameTree fails unless the files under want and got have identical content.
func sameTree(t *testing.T, want, got string) {
	t.Helper()
	count := 0
	err := filepath.WalkDir(want, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(want, path)
		if err != nil {
			return err
		}
		a, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(filepath.Join(got, rel))
		if err != nil {
			return err
		}
		if !bytes.Equal(a, b) {
			t.Errorf("%s differs after restore", rel)
		}
		count++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("no files compared")
	}
}

func TestIntegrationLifecycle(t *testing.T) {
	runLifecycle(t, newHarness(t))
}

// runLifecycle runs init, backup, snapshots, restore, forget, prune and
// check against the harness's repository.
func runLifecycle(t *testing.T, h *harness) {
	t.Helper()
	src := h.local("data")
	writeFile(t, filepath.Join(src, "docs", "a.txt"), []byte("hello\n"))
	writeFile(t, filepath.Join(src, "docs", "b.txt"), bytes.Repeat([]byte("b"), 100_000))
	writeFile(t, filepath.Join(src, "media", "big.bin"), randomBytes(t, 3_000_000))
	writeFile(t, filepath.Join(src, "skip", "junk.tmp"), []byte("junk"))

	h.init(itPassword)

	// Re-initializing an existing repository fails.
	_, _, err := h.run(t.Context(), func(s *syncrv1.JobSpec) {
		s.Operation = &syncrv1.JobSpec_ResticInit{ResticInit: &syncrv1.ResticInit{Repository: h.repo(itPassword)}}
	})
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED)

	// A dry run creates nothing.
	dry, _ := h.backup(&syncrv1.ResticBackup{Paths: []string{src}, DryRun: true})
	if dry.Output.GetResticBackup().GetSnapshotId() != "" {
		t.Errorf("dry-run snapshot ID = %q", dry.Output.GetResticBackup().GetSnapshotId())
	}
	if got := h.snapshots(&syncrv1.ResticSnapshots{}); len(got) != 0 {
		t.Fatalf("dry run created snapshots: %v", got)
	}

	res1, progress := h.backup(&syncrv1.ResticBackup{
		Paths:    []string{src},
		Excludes: []string{"*.tmp"},
		Tags:     []string{"nightly", "docs"},
		Host:     "it-host",
	})
	b1 := res1.Output.GetResticBackup()
	if b1.GetSnapshotId() == "" || b1.GetFilesNew() != 3 || b1.GetDataAddedBytes() == 0 {
		t.Fatalf("backup output = %v, want 3 new files (the .tmp file is excluded)", b1)
	}
	if res1.ExitCode != 0 || res1.Stats.Files != 3 || res1.Stats.Bytes < 3_100_000 || res1.Stats.Errors != 0 {
		t.Errorf("backup result = %+v", res1)
	}
	if len(progress) == 0 || progress[len(progress)-1].Files != 3 {
		t.Errorf("progress = %+v", progress)
	}

	// snapshots, with filters
	snaps := h.snapshots(&syncrv1.ResticSnapshots{})
	if len(snaps) != 1 || snaps[0].GetId() != b1.GetSnapshotId() || snaps[0].GetHost() != "it-host" {
		t.Fatalf("snapshots = %v", snaps)
	}
	if !equalSets(snaps[0].GetTags(), []string{"nightly", "docs"}) || len(snaps[0].GetPaths()) != 1 {
		t.Errorf("snapshot = %v", snaps[0])
	}
	if time.Since(snaps[0].GetTime().AsTime()) > time.Hour {
		t.Errorf("snapshot time = %v", snaps[0].GetTime().AsTime())
	}
	if got := h.snapshots(&syncrv1.ResticSnapshots{Tags: []string{"nope"}}); len(got) != 0 {
		t.Errorf("tag filter matched %v", got)
	}
	if got := h.snapshots(&syncrv1.ResticSnapshots{Host: "other"}); len(got) != 0 {
		t.Errorf("host filter matched %v", got)
	}

	// restore the snapshot and compare content
	target := h.local("restored")
	restoreOp := func(r *syncrv1.ResticRestore) func(*syncrv1.JobSpec) {
		r.Repository = h.repo(itPassword)
		return func(s *syncrv1.JobSpec) { s.Operation = &syncrv1.JobSpec_ResticRestore{ResticRestore: r} }
	}
	rres, rprogress := h.mustRun(restoreOp(&syncrv1.ResticRestore{
		SnapshotId: b1.GetSnapshotId(), Target: target, Verify: true, Excludes: []string{"*.tmp"},
	}))
	if rres.Stats.Files < 3 || len(rprogress) == 0 {
		t.Errorf("restore stats = %+v, progress %d", rres.Stats, len(rprogress))
	}
	restored := restoredPath(target, src)
	if err := os.RemoveAll(filepath.Join(src, "skip")); err != nil {
		t.Fatal(err)
	}
	sameTree(t, src, restored)

	// restore "latest" with an include filter. restic matches include
	// patterns against the path as stored in the snapshot, which on Windows
	// is "/C/Users/..." rather than "C:\Users\...", so an OS path would not
	// match there; a drive-independent pattern does everywhere.
	only := h.local("restored-one")
	include := filepath.Join(src, "docs", "a.txt")
	if runtime.GOOS == "windows" {
		include = "**/docs/a.txt"
	}
	h.mustRun(restoreOp(&syncrv1.ResticRestore{
		SnapshotId: "latest", Target: only, Includes: []string{include},
	}))
	if got, err := os.ReadFile(filepath.Join(restoredPath(only, src), "docs", "a.txt")); err != nil || string(got) != "hello\n" {
		t.Errorf("included file = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(restoredPath(only, src), "media")); err == nil {
		t.Error("include filter restored an excluded directory")
	}

	// second backup, then forget with retention and prune
	writeFile(t, filepath.Join(src, "docs", "a.txt"), []byte("changed\n"))
	res2, _ := h.backup(&syncrv1.ResticBackup{Paths: []string{src}, Tags: []string{"nightly", "docs"}, Host: "it-host"})
	b2 := res2.Output.GetResticBackup()
	if b2.GetSnapshotId() == b1.GetSnapshotId() || b2.GetFilesChanged() != 1 || b2.GetFilesUnmodified() != 2 {
		t.Errorf("second backup = %v", b2)
	}

	forgetOp := func(f *syncrv1.ResticForget) runner.Result {
		f.Repository = h.repo(itPassword)
		res, _ := h.mustRun(func(s *syncrv1.JobSpec) { s.Operation = &syncrv1.JobSpec_ResticForget{ResticForget: f} })
		return res
	}
	policy := &syncrv1.ResticRetentionPolicy{KeepLast: 1}
	dryForget := forgetOp(&syncrv1.ResticForget{Policy: policy, DryRun: true})
	if ids := dryForget.Output.GetResticForget().GetRemovedSnapshotIds(); len(ids) != 1 || ids[0] != b1.GetSnapshotId() {
		t.Errorf("dry-run forget reports %v, want the first snapshot", ids)
	}
	if got := h.snapshots(&syncrv1.ResticSnapshots{}); len(got) != 2 {
		t.Fatalf("dry-run forget removed snapshots: %v", got)
	}
	fres := forgetOp(&syncrv1.ResticForget{Policy: policy, Host: "it-host", Tags: []string{"nightly"}})
	if ids := fres.Output.GetResticForget().GetRemovedSnapshotIds(); len(ids) != 1 || ids[0] != b1.GetSnapshotId() {
		t.Errorf("forget removed %v, want only %s", ids, b1.GetSnapshotId())
	}
	if got := h.snapshots(&syncrv1.ResticSnapshots{}); len(got) != 1 || got[0].GetId() != b2.GetSnapshotId() {
		t.Fatalf("snapshots after forget = %v", got)
	}

	h.mustRun(func(s *syncrv1.JobSpec) {
		s.Operation = &syncrv1.JobSpec_ResticPrune{ResticPrune: &syncrv1.ResticPrune{Repository: h.repo(itPassword)}}
	})
	// forget --prune in one job
	writeFile(t, filepath.Join(src, "docs", "a.txt"), []byte("third\n"))
	h.backup(&syncrv1.ResticBackup{Paths: []string{src}, Host: "it-host"})
	pres := forgetOp(&syncrv1.ResticForget{Policy: policy, Prune: true})
	if ids := pres.Output.GetResticForget().GetRemovedSnapshotIds(); len(ids) != 1 {
		t.Errorf("forget --prune removed %v", ids)
	}

	for _, pct := range []int32{0, 100} {
		cres, _ := h.mustRun(func(s *syncrv1.JobSpec) {
			s.Operation = &syncrv1.JobSpec_ResticCheck{ResticCheck: &syncrv1.ResticCheck{Repository: h.repo(itPassword), ReadDataPercent: pct}}
		})
		if cres.Stats.Errors != 0 || cres.ExitCode != 0 {
			t.Errorf("check(%d%%) = %+v", pct, cres)
		}
	}

	// No scratch directory is left behind.
	if entries, _ := os.ReadDir(filepath.Join(filepath.Dir(h.root), "scratch")); len(entries) != 0 {
		t.Errorf("scratch not cleaned: %v", entries)
	}
}

func equalSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}

func TestIntegrationRepositoryErrors(t *testing.T) {
	h := newHarness(t)
	writeFile(t, h.local("data", "a.txt"), []byte("a"))
	snapshotsOp := func(repo *syncrv1.ResticRepository) func(*syncrv1.JobSpec) {
		return func(s *syncrv1.JobSpec) {
			s.Operation = &syncrv1.JobSpec_ResticSnapshots{ResticSnapshots: &syncrv1.ResticSnapshots{Repository: repo}}
		}
	}

	t.Run("not initialized", func(t *testing.T) {
		_, _, err := h.run(t.Context(), snapshotsOp(h.repo(itPassword)))
		if !errors.Is(err, ErrRepositoryNotFound) {
			t.Fatalf("err = %v, want ErrRepositoryNotFound", err)
		}
		mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_NOT_FOUND)
	})

	h.init(itPassword)

	t.Run("wrong password", func(t *testing.T) {
		wrong := "definitely-not-the-password"
		res, _, err := h.run(t.Context(), snapshotsOp(h.repo(wrong)))
		if !errors.Is(err, ErrWrongPassword) || res.ExitCode != 12 {
			t.Fatalf("err = %v, exit %d, want ErrWrongPassword/12", err, res.ExitCode)
		}
		mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_WRONG_PASSWORD)
		for _, secret := range []string{wrong, itPassword} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error leaks the password: %v", err)
			}
		}
	})

	t.Run("local repository", func(t *testing.T) {
		repo := &syncrv1.ResticRepository{Location: &syncrv1.Location{Path: h.local("localrepo")}, Password: itPassword}
		h.mustRun(func(s *syncrv1.JobSpec) {
			s.Operation = &syncrv1.JobSpec_ResticInit{ResticInit: &syncrv1.ResticInit{Repository: repo}}
		})
		h.mustRun(func(s *syncrv1.JobSpec) {
			s.Operation = &syncrv1.JobSpec_ResticBackup{ResticBackup: &syncrv1.ResticBackup{Repository: repo, Paths: []string{h.local("data")}}}
		})
		res, _ := h.mustRun(snapshotsOp(repo))
		if n := len(res.Output.GetResticSnapshots().GetSnapshots()); n != 1 {
			t.Errorf("local repository has %d snapshots, want 1", n)
		}
	})

	t.Run("repository outside local_roots", func(t *testing.T) {
		outside := filepath.Join(filepath.Dir(h.root), "elsewhere")
		repo := &syncrv1.ResticRepository{Location: &syncrv1.Location{Path: outside}, Password: itPassword}
		_, _, err := h.run(t.Context(), snapshotsOp(repo))
		mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED)
	})
}

func TestIntegrationCancelMidBackup(t *testing.T) {
	h := newHarness(t, "--limit-upload=500") // KiB/s: 4 MiB of random data takes ~8s
	h.init(itPassword)
	src := h.local("data")
	writeFile(t, filepath.Join(src, "big.bin"), randomBytes(t, 4_000_000))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	spec := &syncrv1.JobSpec{Remotes: h.remotes(), Operation: &syncrv1.JobSpec_ResticBackup{ResticBackup: &syncrv1.ResticBackup{
		Repository: h.repo(itPassword), Paths: []string{src},
	}}}
	var once sync.Once
	var cancelAt time.Time
	start := time.Now()
	res, err := h.runners[kindBackup].Run(ctx, runner.Job{ID: "it-cancel", Spec: spec}, func(runner.Progress) {
		once.Do(func() { cancelAt = time.Now(); cancel() }) // first status: the backup is under way
	})
	if !runner.Canceled(err) {
		t.Fatalf("err = %v, want a cancellation", err)
	}
	if res.Output != nil {
		t.Errorf("a cancelled backup returned output %v", res.Output)
	}
	if stop := time.Since(cancelAt); stop > 12*time.Second {
		t.Errorf("restic took %v to stop", stop)
	}
	if total := time.Since(start); total > 14*time.Second {
		t.Errorf("job took %v; the backup should have been cut short", total)
	}
	t.Logf("cancelled after %v, exit code %d", time.Since(start), res.ExitCode)

	// The repository stays usable: restic released its lock on the way out
	// (or the lock is stale-tolerant for non-exclusive operations), and no
	// snapshot was created.
	if got := h.snapshots(&syncrv1.ResticSnapshots{}); len(got) != 0 {
		t.Errorf("cancelled backup left snapshots: %v", got)
	}
	if runtime.GOOS == "windows" {
		// Cancellation kills restic there (no SIGINT for console programs),
		// so its lock file stays behind and a stale lock can make `check`
		// refuse to run; releasing it is a retention/maintenance concern of
		// the controller. Listing snapshots above already proved the
		// repository is readable.
		return
	}
	h.mustRun(func(s *syncrv1.JobSpec) {
		s.Operation = &syncrv1.JobSpec_ResticCheck{ResticCheck: &syncrv1.ResticCheck{Repository: h.repo(itPassword)}}
	})
}

func TestIntegrationLockedRepository(t *testing.T) {
	h := newHarness(t, "--limit-upload=500")
	h.init(itPassword)
	src := h.local("data")
	writeFile(t, filepath.Join(src, "big.bin"), randomBytes(t, 4_000_000))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		spec := &syncrv1.JobSpec{Remotes: h.remotes(), Operation: &syncrv1.JobSpec_ResticBackup{ResticBackup: &syncrv1.ResticBackup{
			Repository: h.repo(itPassword), Paths: []string{src},
		}}}
		var once sync.Once
		_, err := h.runners[kindBackup].Run(ctx, runner.Job{ID: "it-lock-backup", Spec: spec}, func(runner.Progress) {
			once.Do(func() { close(started) })
		})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("backup did not start")
	}

	// prune needs the exclusive lock and so conflicts with the running backup.
	res, _, err := h.run(t.Context(), func(s *syncrv1.JobSpec) {
		s.Operation = &syncrv1.JobSpec_ResticPrune{ResticPrune: &syncrv1.ResticPrune{Repository: h.repo(itPassword)}}
	})
	if !errors.Is(err, ErrRepositoryLocked) || res.ExitCode != 11 {
		t.Errorf("prune during backup: err = %v, exit %d, want ErrRepositoryLocked/11", err, res.ExitCode)
	} else {
		mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_LOCKED)
	}
	cancel()
	if err := <-done; !runner.Canceled(err) {
		t.Errorf("backup err = %v, want cancellation", err)
	}
}

// TestIntegrationRcloneInAwkwardDirectory runs restic's rclone backend with
// the rclone binary in a directory whose name restic's own argument splitter
// or its CSV reading of --option would break apart unless the runner quotes
// it: spaces, commas, both quote characters and backslashes ('$', '%' and
// parentheses are not special to restic but are to shells). Backslashes and double quotes are only valid in
// file names outside Windows; on Windows the regular tests cover backslashes
// (every path has them).
func TestIntegrationRcloneInAwkwardDirectory(t *testing.T) {
	rclone := toolPath(t, "SYNCR_TEST_RCLONE")
	toolPath(t, "SYNCR_TEST_RESTIC")
	names := []string{`syncr it's, $HOME %TEMP% (x86) dir`}
	if runtime.GOOS != "windows" {
		names = append(names, `syncr "dq", \back\slash dir`, "syncr\ttab dir")
	}
	base := awkwardBase(t)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			prog := filepath.Join(base, name, filepath.Base(rclone))
			copyExecutable(t, rclone, prog)
			h := newHarnessWith(t, func(_ *harness, cfg *Config) { cfg.RclonePath = prog })
			src := h.local("data")
			writeFile(t, filepath.Join(src, "a.txt"), []byte("awkward\n"))
			h.init(itPassword)
			res, _ := h.backup(&syncrv1.ResticBackup{Paths: []string{src}})
			if b := res.Output.GetResticBackup(); b.GetSnapshotId() == "" || b.GetFilesNew() != 1 {
				t.Fatalf("backup output = %v, want one new file", b)
			}
		})
	}
}

// awkwardBase returns a fresh directory for copies of rclone: under /dev/shm
// where available (tmpfs, so the copy costs no disk), else under the test's
// temp dir.
func awkwardBase(t *testing.T) string {
	t.Helper()
	if fi, err := os.Stat("/dev/shm"); err == nil && fi.IsDir() {
		dir, err := os.MkdirTemp("/dev/shm", "syncr-it-")
		if err == nil {
			t.Cleanup(func() {
				if err := os.RemoveAll(dir); err != nil {
					t.Errorf("remove %s: %v", dir, err)
				}
			})
			return dir
		}
	}
	return t.TempDir()
}

func copyExecutable(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
