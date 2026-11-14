package devseed_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/link"
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

// TestSeedRemoteBackups: the backups of a remote: the Nextcloud photos'
// nightly mirror schedule on nas-01 with its runs (one failed with
// source_empty), the runs by hand (a mount, an allowed empty folder without
// a snapshot, a mount refused on backup-host), and the repository's
// snapshots tagged with the remote, in the cache and in the probes'
// answers.
func TestSeedRemoteBackups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	s := e.seed(t)
	repoID := s.Repositories["Nextcloud photos"]
	nextcloud := s.Remotes["Nextcloud"]
	tag := link.RemoteSourceTag(nextcloud)

	sc, err := e.store.GetSchedule(ctx, s.Schedules["Nightly Nextcloud photos backup"])
	if err != nil {
		t.Fatal(err)
	}
	def, err := jobkind.Unmarshal(sc.Definition)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Target.WorkerID != s.Workers["nas-01"] || sc.RepositoryID != repoID || !slices.Contains(sc.RemoteIDs, nextcloud) ||
		def.GetResticBackup().GetRemoteSource().GetMethod() != syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MIRROR {
		t.Errorf("schedule = %+v", sc)
	}
	if ok, _ := e.deps.Kinds.LabelTargetAllowed(def); ok {
		t.Error("the schedule's definition may target a label")
	}
	page, err := e.deps.Jobs.List(ctx, s.Admin, jobs.ListFilter{ScheduleID: sc.ID, Limit: jobs.MaxPageSize})
	if err != nil || len(page.Jobs) < 10 {
		t.Fatalf("%d scheduled jobs (%v)", len(page.Jobs), err)
	}
	var sourceEmpty int
	for _, j := range page.Jobs {
		if len(j.ConcurrencyKeys) != 2 {
			t.Errorf("job %s keys %v", j.ID, j.ConcurrencyKeys)
		}
		switch {
		case j.Status == domain.JobSucceeded:
			ds := e.deps.Kinds.Summarize(j.Kind, def, j.Output)
			if !slices.ContainsFunc(ds, func(d jobkind.Detail) bool { return d.Label == "Mirrored" }) {
				t.Errorf("job %s summary = %v", j.ID, ds)
			}
		case j.Error.Code == "source_empty":
			sourceEmpty++
		}
	}
	if sourceEmpty != 1 {
		t.Errorf("%d runs failed with source_empty, want 1", sourceEmpty)
	}

	all, err := e.deps.Jobs.List(ctx, s.Admin, jobs.ListFilter{RepositoryID: repoID, Limit: jobs.MaxPageSize})
	if err != nil {
		t.Fatal(err)
	}
	var mounted, empty, refused bool
	for _, j := range all.Jobs {
		if !j.ScheduleID.IsZero() {
			continue
		}
		d, err := jobkind.Unmarshal(j.Definition)
		if err != nil {
			t.Fatal(err)
		}
		details := e.deps.Kinds.Summarize(j.Kind, d, j.Output)
		switch {
		case j.Error.Code == jobs.CodeToolUnavailable:
			refused = j.WorkerID == s.Workers["backup-host"] && strings.Contains(j.Error.Message, "/dev/fuse is missing")
		case slices.Contains(details, jobkind.Detail{Label: "Read through", Value: "Mount"}):
			mounted = j.Status == domain.JobSucceeded
		case slices.Contains(details, jobkind.Detail{Label: "Snapshot", Value: jobkind.NoSnapshotEmptySource}):
			empty = j.Status == domain.JobSucceeded && len(j.Warnings) == 1
		}
	}
	if !mounted || !empty || !refused {
		t.Errorf("runs by hand: mounted %v, empty %v, refused %v", mounted, empty, refused)
	}

	// The snapshots carry the remote's tag, in the cache and when a worker
	// is asked.
	cached, err := e.store.ListSnapshots(ctx, repoID)
	if err != nil || len(cached.Snapshots) != 12 {
		t.Fatalf("cached snapshots: %d (%v)", len(cached.Snapshots), err)
	}
	for _, sn := range cached.Snapshots {
		if !slices.Equal(sn.Tags, []string{"phone", tag}) {
			t.Fatalf("cached snapshot tags %v, want [phone %s]", sn.Tags, tag)
		}
	}
	res, err := e.deps.Probes.Probe(ctx, s.Workers["nas-01"], &syncrv1.RunProbe{ProbeId: "p1", Spec: &syncrv1.ProbeSpec{
		Remotes: []*syncrv1.RemoteDefinition{{Alias: "r1", BackendType: "s3"}},
		Op: &syncrv1.ProbeSpec_ResticSnapshots{ResticSnapshots: &syncrv1.ProbeResticSnapshots{Repository: &syncrv1.ResticRepository{
			Location: &syncrv1.Location{Remote: "r1", Path: "archive/restic/nextcloud-photos"}, Password: "syncr-demo-restic",
		}}},
	}})
	snaps := res.GetSnapshots().GetSnapshots()
	if err != nil || len(snaps) != 12 || snaps[0].GetId() != cached.Snapshots[0].ID || !slices.Contains(snaps[0].GetTags(), tag) {
		t.Errorf("probed snapshots = %v (%v)", res, err)
	}
}
