package dispatch_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// TestSpecsRestoreItemsOfSubfolder: selected items of a restore's
// subfolder go only to a worker with
// link.FeatureResticRestoreSubfolderPaths; an older one (which refuses the
// combination) gets ErrWorkerOutdated, while a plain subfolder restore
// still goes to it.
func TestSpecsRestoreItemsOfSubfolder(t *testing.T) {
	t.Parallel()
	repoID := domain.NewResticRepoID()
	repos := &fakeRepos{repo: restic.Resolved{
		ID: repoID, OwnerID: testOwner, Name: "Photos", RemoteID: testRemote(), Path: "backups/photos",
		Password: testRepoPassword, ResticID: strings.Repeat("ab", 32),
	}}
	specs, err := dispatch.NewSpecs(newFakeRemotes(), repos, kinds(t), dispatch.SpecsConfig{})
	if err != nil {
		t.Fatal(err)
	}
	restore := func(paths ...string) *syncrv1.JobDefinition {
		return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticRestore{ResticRestore: &syncrv1.ResticRestoreDefinition{
			RepositoryId: repoID.String(), SnapshotId: strings.Repeat("cd", 32), Target: "/srv/photos",
			Subfolder: "/srv/photos", Paths: paths,
		}}}
	}
	items := storedJob(t, restore("/srv/photos/2026", "/srv/photos/README.txt"))
	spec, err := specs.Build(context.Background(), items, workers.Info{Features: link.Features()})
	if r := spec.GetResticRestore(); err != nil || r.GetSubfolder() != "/srv/photos" || len(r.GetPaths()) != 2 {
		t.Fatalf("Build = %v (%v)", spec, err)
	}
	old := slices.DeleteFunc(link.Features(), func(f string) bool { return f == link.FeatureResticRestoreSubfolderPaths })
	if _, err := specs.Build(context.Background(), items, workers.Info{Features: old}); !errors.Is(err, dispatch.ErrWorkerOutdated) ||
		!strings.Contains(err.Error(), link.FeatureResticRestoreSubfolderPaths) {
		t.Errorf("Build on a worker without %s = %v, want ErrWorkerOutdated naming it", link.FeatureResticRestoreSubfolderPaths, err)
	}
	if _, err := specs.Build(context.Background(), storedJob(t, restore()), workers.Info{Features: old}); err != nil {
		t.Errorf("Build of a subfolder restore on an older worker = %v", err)
	}
}
