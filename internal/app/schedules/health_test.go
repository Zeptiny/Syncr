package schedules_test

import (
	"context"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

func TestHealth(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	nightly := e.create(e.alice, e.input("Nightly"))
	backup := e.create(e.alice, e.input("Backup"))
	e.create(e.bob, e.input("Bobs"))
	run, err := e.svc.RunNow(ctx, e.alice, nightly.ID)
	if err != nil {
		t.Fatal(err)
	}
	finished := t0.Add(-time.Hour)
	for i, st := range []domain.JobStatus{domain.JobSucceeded, domain.JobFailed} {
		j := jobs.Job{
			ID: domain.NewJobID(), WorkerID: e.worker, OwnerID: e.alice.UserID, Kind: "rclone_transfer", Status: st,
			Definition: []byte(testDefinition), ScheduleID: nightly.ID, CreatedAt: t0, QueuedAt: t0,
			FinishedAt: finished.Add(time.Duration(i) * time.Minute), ExitCode: -1, Version: 1,
		}
		if err := e.store.CreateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}

	got, err := e.svc.Health(ctx, e.alice)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Schedule.ID != backup.ID || got[1].Schedule.ID != nightly.ID {
		t.Fatalf("Health(alice) = %+v, want Backup and Nightly, by name", got)
	}
	if b := got[0]; b.HasRun || !b.LastSuccessAt.IsZero() {
		t.Errorf("Backup = %+v, want no run and no success", b)
	}
	if n := got[1]; !n.HasRun || n.LatestRun.ID != run.ID || !n.LastSuccessAt.Equal(finished) {
		t.Errorf("Nightly = %+v, want run %s and the success at %v (the later failure does not count)", n, run.ID, finished)
	}
	// Schedules are owner-only, admins included.
	if got, err := e.svc.Health(ctx, e.admin); err != nil || len(got) != 0 {
		t.Errorf("Health(admin) = (%+v, %v), want none", got, err)
	}
	if got, err := e.svc.Health(ctx, e.bob); err != nil || len(got) != 1 || got[0].Schedule.Name != "Bobs" {
		t.Errorf("Health(bob) = (%+v, %v), want Bobs only", got, err)
	}
}
