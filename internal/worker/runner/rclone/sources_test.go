package rclone

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// TestTransferLeavesSourcesOut: a transfer whose local source or
// destination holds the worker's folder of backups of remotes gets
// --exclude-if-present with its marker before "--"; any other transfer is
// unchanged, and a path inside the folder is refused by the policy.
func TestTransferLeavesSourcesOut(t *testing.T) {
	t.Parallel()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "root")
	sources := filepath.Join(root, "syncr", "sources")
	if err := os.MkdirAll(filepath.Join(root, "srv"), 0o700); err != nil {
		t.Fatal(err)
	}
	policy, err := localfs.NewPolicy([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if policy, err = policy.Reserve(sources); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		src, dst *syncrv1.Location
		want     bool
		code     syncrv1.JobErrorCode
	}{
		{src: &syncrv1.Location{Path: filepath.Join(root, "srv")}, dst: &syncrv1.Location{Remote: "bk", Path: "b"}},
		{src: &syncrv1.Location{Path: root}, dst: &syncrv1.Location{Remote: "bk", Path: "b"}, want: true},
		{src: &syncrv1.Location{Remote: "bk", Path: "b"}, dst: &syncrv1.Location{Path: filepath.Join(root, "syncr")}, want: true},
		{src: &syncrv1.Location{Path: filepath.Join(sources, "mirror")}, dst: &syncrv1.Location{Remote: "bk", Path: "b"},
			code: syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED},
	} {
		sup := &fakeSupervisor{}
		r, err := New(Config{
			Binary: "/opt/syncr/rclone", Supervisor: sup, Env: rcloneenv.NewBuilder(policy), Paths: policy,
			ScratchDir: filepath.Join(base, "scratch"), SourcesDir: sources,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.Run(t.Context(), transferJob(tt.src, tt.dst, sftpRemote("bk", "pw")), nil)
		if tt.code != 0 {
			if code := runner.CodeOf(err); code != tt.code || sup.called {
				t.Errorf("%v -> %v: %v (ran %v)", tt.src, tt.dst, err, sup.called)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		args := sup.spec.Args
		i := slices.Index(args, "--exclude-if-present=.syncr-sources")
		if (i >= 0) != tt.want || (i >= 0 && args[i+1] != "--") {
			t.Errorf("%v -> %v: args %q", tt.src, tt.dst, args)
		}
	}
}
