package devseed_test

import (
	"context"
	"testing"

	"github.com/Zeptiny/Syncr/internal/app/workers"
)

// TestSeedRemoteSources: the demo workers report one of each answer to
// "can it back up a remote's folder": both methods, the mirror only (with
// the mount's reason), neither (with both reasons); attic-nas, an old
// build, and the pending laptop report nothing.
func TestSeedRemoteSources(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	s := e.seed(t)

	for name, want := range map[string]workers.RemoteSources{
		"nas-01":      {Mirror: true, Mount: true},
		"backup-host": {Mirror: true, MountReason: "/dev/fuse is missing"},
		"office-pc":   {MirrorReason: "remote_sources.mirror is off in worker.yaml", MountReason: "WinFsp is not installed"},
		"attic-nas":   {},
		"laptop":      {},
	} {
		w, err := e.deps.Workers.Get(ctx, s.Workers[name])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if w.Info.RemoteSources != want {
			t.Errorf("%s: RemoteSources = %+v, want %+v", name, w.Info.RemoteSources, want)
		}
	}
}
