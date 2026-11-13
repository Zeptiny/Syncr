package dispatch_test

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch"
	"github.com/Zeptiny/Syncr/internal/link"
)

// TestSpecsTwoWaySync: a two-way sync's spec carries its sides and the
// pair's state key; a worker without link.FeatureRcloneBisync (one
// released before two-way syncs) gets ErrWorkerOutdated, so the job fails
// with worker_outdated instead of being sent.
func TestSpecsTwoWaySync(t *testing.T) {
	t.Parallel()
	specs, err := dispatch.NewSpecs(newFakeRemotes(), &fakeRepos{}, kinds(t), dispatch.SpecsConfig{})
	if err != nil {
		t.Fatal(err)
	}
	def := &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneBisync{RcloneBisync: &syncrv1.RcloneBisyncDefinition{
		Path1:  &syncrv1.JobLocation{Path: "/data/shared"},
		Path2:  &syncrv1.JobLocation{RemoteId: testRemote().String(), Path: "shared"},
		Resync: true,
	}}}
	j := storedJob(t, def)
	spec, err := specs.Build(context.Background(), j, workers.Info{Features: link.Features()})
	b := spec.GetRcloneBisync()
	if err != nil || b.GetPath1().GetPath() != "/data/shared" || b.GetPath2().GetRemote() != "r1" || !b.GetResync() ||
		!regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(b.GetStateKey()) || len(spec.GetRemotes()) != 1 {
		t.Fatalf("Build = %v (%v)", spec, err)
	}
	old := slices.DeleteFunc(link.Features(), func(f string) bool { return f == link.FeatureRcloneBisync })
	if _, err := specs.Build(context.Background(), j, workers.Info{Features: old}); !errors.Is(err, dispatch.ErrWorkerOutdated) {
		t.Errorf("Build on a worker without %s = %v, want ErrWorkerOutdated", link.FeatureRcloneBisync, err)
	}
}
