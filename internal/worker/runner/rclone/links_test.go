package rclone

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// symlinkOrSkip creates a link, skipping the test where links need
// privileges (Windows).
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks: %v", err)
		}
		t.Fatal(err)
	}
}

// TestTransferRefusesLinksOutOfRoots: rclone writes through a link to a
// folder it finds in a destination, so a transfer into a local folder (or
// a union's local upstream) that holds a link leading outside local_roots
// fails with PATH_NOT_ALLOWED before rclone starts, naming the link. A
// restore can plant such a link: this is the restore-then-copy escape. A
// link in a source is harmless (rclone does not follow it) and so is a
// link to a file (rclone replaces it).
func TestTransferRefusesLinksOutOfRoots(t *testing.T) {
	t.Parallel()
	f := newFixture(t, &fakeSupervisor{})
	outside := filepath.Join(filepath.Dir(f.data), "outside")
	dst, other := filepath.Join(f.data, "restored"), filepath.Join(f.data, "other")
	for _, d := range []string{dst, other, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "passwd"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, filepath.Join(outside, "passwd"), filepath.Join(dst, "file-link"))
	remote := &syncrv1.Location{Remote: "box", Path: "x"}
	union := localUnion("both", dst, other)
	run := func(from, to *syncrv1.Location) error {
		f.sup.called = false
		_, err := f.runner.Run(t.Context(), transferJob(from, to, sftpRemote("box", "pw"), union), nil)
		return err
	}
	if err := run(remote, &syncrv1.Location{Path: dst}); err != nil || !f.sup.called {
		t.Fatalf("a link to a file: %v", err)
	}

	evil := filepath.Join(dst, "evil")
	symlinkOrSkip(t, outside, evil)
	for _, tt := range []struct {
		name     string
		src, dst *syncrv1.Location
	}{
		{"local destination", remote, &syncrv1.Location{Path: dst}},
		{"destination above it", remote, &syncrv1.Location{Path: f.data}},
		{"union destination", remote, &syncrv1.Location{Remote: "both", Path: ""}},
	} {
		err := run(tt.src, tt.dst)
		if runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED || !errors.Is(err, localfs.ErrLink) ||
			!strings.Contains(err.Error(), evil) || f.sup.called {
			t.Errorf("%s: Run() = %v (code %v, rclone started %v)", tt.name, err, runner.CodeOf(err), f.sup.called)
		}
	}
	if err := run(&syncrv1.Location{Path: dst}, remote); err != nil || !f.sup.called {
		t.Errorf("a source holding the link: %v", err)
	}
}

// localUnion is a union of two local folders.
func localUnion(alias, a, b string) *syncrv1.RemoteDefinition {
	return &syncrv1.RemoteDefinition{
		Alias: alias, BackendType: "union",
		UnionUpstreams: []*syncrv1.UnionUpstream{{Location: &syncrv1.Location{Path: a}}, {Location: &syncrv1.Location{Path: b}}},
	}
}

// TestTransferClaims: a transfer records its local folders while it runs.
// It fails with TEMPORARY when a restore (an exclusive claim) uses an
// overlapping folder, including one a link in its destination leads to,
// and runs beside other jobs that share theirs; a move with a local
// source takes its folders exclusively; the folders are given back when
// it ends.
func TestTransferClaims(t *testing.T) {
	t.Parallel()
	f := newFixture(t, &fakeSupervisor{})
	claims := &localfs.Claims{}
	f.runner.cfg.Claims = claims
	dst, target, other := filepath.Join(f.data, "dst"), filepath.Join(f.data, "target"), filepath.Join(f.data, "other")
	for _, d := range []string{dst, target, other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	remote := &syncrv1.Location{Remote: "box", Path: "x"}
	run := func(mode syncrv1.RcloneTransferMode, src, dst *syncrv1.Location) error {
		f.sup.called = false
		job := transferJob(src, dst, sftpRemote("box", "pw"))
		job.Spec.GetRcloneTransfer().Mode = mode
		_, err := f.runner.Run(t.Context(), job, nil)
		return err
	}
	temporary := func(name string, err error) {
		t.Helper()
		if runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_TEMPORARY || !errors.Is(err, localfs.ErrInUse) || f.sup.called {
			t.Errorf("%s: Run() = %v (code %v, rclone started %v)", name, err, runner.CodeOf(err), f.sup.called)
		}
	}

	restore := claims.Take("restic_restore")
	if err := restore.Add(target, true); err != nil {
		t.Fatal(err)
	}
	temporary("into the restore's target", run(modeCopy, remote, &syncrv1.Location{Path: filepath.Join(target, "sub")}))
	temporary("from the restore's target", run(modeCopy, &syncrv1.Location{Path: target}, remote))
	symlinkOrSkip(t, target, filepath.Join(dst, "to-target"))
	temporary("through a link to the restore's target", run(modeCopy, remote, &syncrv1.Location{Path: dst}))
	if err := run(modeCopy, remote, &syncrv1.Location{Path: other}); err != nil || !f.sup.called {
		t.Errorf("beside the restore: %v", err)
	}
	restore.Release()

	backup := claims.Take("restic_backup")
	if err := backup.Add(other, false); err != nil {
		t.Fatal(err)
	}
	if err := run(modeCopy, &syncrv1.Location{Path: other}, &syncrv1.Location{Path: dst}); err != nil || !f.sup.called {
		t.Errorf("a copy from a folder being backed up: %v", err)
	}
	temporary("a move from a folder being backed up", run(modeMove, &syncrv1.Location{Path: other}, &syncrv1.Location{Path: dst}))
	if err := run(modeMove, remote, &syncrv1.Location{Path: other}); err != nil || !f.sup.called {
		t.Errorf("a move from a remote into a folder being backed up: %v", err)
	}
	backup.Release()

	// Nothing is left claimed.
	check := claims.Take("restic_restore")
	defer check.Release()
	if err := check.Add(f.data, true); err != nil {
		t.Errorf("after the jobs: %v", err)
	}
}
