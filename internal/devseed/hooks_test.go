package devseed_test

import (
	"context"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/link"
)

// TestSeedHooks: the demo objects of the hooks screens. nas-01 defines four
// hooks; the backups of its documents run them (stop the app, dump its
// database, back up, start the app again), one failed because the app did
// not come back, and the one that is running names them without runs yet.
// The worker that needs an upgrade cannot run hooks.
func TestSeedHooks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	s := e.seed(t)

	nas, err := e.deps.Workers.Get(ctx, s.Workers["nas-01"])
	if err != nil || !nas.Info.HasFeatures(link.FeatureJobHooks) {
		t.Fatalf("nas-01 = %+v (%v)", nas.Info, err)
	}
	if got, want := nas.Info.HookNames(), []string{"dump-paperless-db", "ping-healthcheck", "start-paperless", "stop-paperless"}; !slices.Equal(got, want) {
		t.Errorf("nas-01 hooks = %v, want %v", got, want)
	}
	for _, h := range nas.Info.Hooks {
		if h.Description == "" || h.Timeout <= 0 || !domain.ValidHookName(h.Name) {
			t.Errorf("hook %+v lacks a description or a timeout", h)
		}
	}
	attic, err := e.deps.Workers.Get(ctx, s.Workers["attic-nas"])
	if err != nil || attic.Info.HasFeatures(link.FeatureJobHooks) || len(attic.Info.Hooks) != 0 {
		t.Errorf("attic-nas = %+v (%v), want a worker that cannot run hooks", attic.Info, err)
	}
	for _, name := range []string{"office-pc", "backup-host"} {
		w, err := e.deps.Workers.Get(ctx, s.Workers[name])
		if err != nil || !w.Info.HasFeatures(link.FeatureJobHooks) || len(w.Info.Hooks) != 0 {
			t.Errorf("%s = %+v (%v), want a current worker without hooks", name, w.Info, err)
		}
	}

	page, err := e.deps.Jobs.List(ctx, s.Admin, jobs.ListFilter{RepositoryID: s.Repositories["NAS documents"], Kinds: []string{"restic_backup"}})
	if err != nil {
		t.Fatal(err)
	}
	want := &syncrv1.JobHooks{
		Before: []string{"stop-paperless", "dump-paperless-db"}, OnSuccess: []string{"ping-healthcheck"}, After: []string{"start-paperless"},
	}
	var succeeded, failed, running int
	for _, j := range page.Jobs {
		if j.WorkerID != nas.ID {
			// The backup queued on backup-host names none: that worker
			// defines none.
			if len(j.HookRuns) != 0 {
				t.Errorf("job %s on another worker has hook runs", j.ID)
			}
			continue
		}
		def, err := jobkind.Unmarshal(j.Definition)
		if err != nil || !proto.Equal(def.GetHooks(), want) {
			t.Fatalf("job %s names the hooks %v (%v), want %v", j.ID, def.GetHooks(), err, want)
		}
		if missing := jobkind.MissingHooks(def, nas.Info.HookNames()); len(missing) != 0 {
			t.Errorf("job %s names hooks its worker lacks: %v", j.ID, missing)
		}
		switch j.Status {
		case domain.JobRunning:
			running++
			if len(j.HookRuns) != 0 {
				t.Errorf("the running backup has hook runs: %+v", j.HookRuns)
			}
			continue
		case domain.JobSucceeded:
			succeeded++
		case domain.JobFailed:
			failed++
			if j.Error.Code != jobs.CodeHookFailed || j.Error.Message != `hook "start-paperless" (after) timed out after 2m0s` {
				t.Errorf("failed backup = %+v", j.Error)
			}
			// Its snapshot was written before the hook failed.
			var out syncrv1.JobOutput
			if err := proto.Unmarshal(j.Output, &out); err != nil || out.GetResticBackup().GetSnapshotId() == "" {
				t.Errorf("failed backup output = %v (%v), want its snapshot", &out, err)
			}
		default:
			t.Errorf("job %s is %s", j.ID, j.Status)
		}
		// The runs follow the order the worker runs them in, inside the
		// job's own time.
		var order []string
		for i, r := range j.HookRuns {
			order = append(order, r.Phase+":"+r.Name+"="+r.Status)
			if r.StartedAt.Before(j.StartedAt) || r.StartedAt.Add(r.Duration).After(j.FinishedAt) {
				t.Errorf("job %s: hook %s ran %s for %s, outside the job (%s to %s)", j.ID, r.Name, r.StartedAt, r.Duration, j.StartedAt, j.FinishedAt)
			}
			if i > 0 && r.StartedAt.Before(j.HookRuns[i-1].StartedAt) {
				t.Errorf("job %s: hook %s started before the one listed before it", j.ID, r.Name)
			}
			if !domain.ValidHookPhase(r.Phase) || !domain.ValidHookStatus(r.Status) || r.Output == "" {
				t.Errorf("job %s: hook run %+v", j.ID, r)
			}
		}
		last := "after:start-paperless=succeeded"
		if j.Status == domain.JobFailed {
			last = "after:start-paperless=timed_out"
		}
		if wantOrder := []string{"before:stop-paperless=succeeded", "before:dump-paperless-db=succeeded", "on_success:ping-healthcheck=succeeded", last}; !slices.Equal(order, wantOrder) {
			t.Errorf("job %s (%s): hook runs = %v, want %v", j.ID, j.Status, order, wantOrder)
		}
	}
	if succeeded != 2 || failed != 1 || running != 1 {
		t.Errorf("backups of the NAS documents on nas-01: %d succeeded, %d failed, %d running; want 2, 1 and 1", succeeded, failed, running)
	}
}
