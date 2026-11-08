package e2e

import (
	"context"
	"errors"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/controller/scheduler"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
)

// noContacts is the schedules' contact checker of the tests: the test
// schedules link no contacts.
type noContacts struct{}

func (noContacts) Usable(context.Context, domain.UserID, []domain.ContactID) (map[domain.ContactID]string, error) {
	return map[domain.ContactID]string{}, nil
}

// activeOwners says every schedule owner may run jobs.
type activeOwners struct{}

func (activeOwners) Active(context.Context, domain.UserID) (bool, error) { return true, nil }

// workerCheck adapts the workers service to schedules.WorkerChecker, as
// the composition root does.
type workerCheck struct{ ws *workers.Service }

func (c workerCheck) CheckWorker(ctx context.Context, id domain.WorkerID) error {
	_, err := c.ws.Get(ctx, id)
	return err
}

// scheduling is the schedules service and the scheduler over the test
// controller's store and services.
type scheduling struct {
	svc   *schedules.Service
	sched *scheduler.Scheduler
}

func newScheduling(t *testing.T, c *controller) *scheduling {
	t.Helper()
	js, ws := c.svc()
	sig := scheduler.NewSignal()
	svc, err := schedules.New(schedules.Deps{
		Repo: c.store, Remotes: c.remotes, Contacts: noContacts{}, Workers: workerCheck{ws}, Waker: sig, Clock: c.clock, IDs: c.ids,
	}, schedules.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sched, err := scheduler.New(scheduler.Deps{
		Schedules: svc, Jobs: js, Workers: ws, Kinds: kinds(t), Owners: activeOwners{}, IDs: c.ids, Clock: c.clock, Signal: sig,
	}, scheduler.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return &scheduling{svc: svc, sched: sched}
}

// create creates an enabled daily schedule of the owner running def on
// worker w, skipping overlapping runs.
func (s *scheduling) create(t *testing.T, w domain.WorkerID, name string, def *syncrv1.JobDefinition) schedules.Schedule {
	t.Helper()
	kind, refs, err := kinds(t).Check(def)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jobkind.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := s.svc.Create(context.Background(), testActor(), schedules.Input{
		Name: name, Cron: "0 3 * * *", Timezone: "UTC", Target: schedules.Target{WorkerID: w},
		Missed: schedules.MissedPolicy{Action: schedules.MissedRunLate}, Overlap: schedules.OverlapSkip, Enabled: true,
		Kind: kind, Definition: raw, RemoteIDs: refs.RemoteIDs, SearchTerms: refs.SearchTerms,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// runNow requests a run and makes one scheduler pass, which launches it on
// the online worker; it returns the launched run.
func (s *scheduling) runNow(t *testing.T, id domain.ScheduleID) schedules.Run {
	t.Helper()
	ctx := context.Background()
	r, err := s.svc.RunNow(ctx, testActor(), id)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != schedules.RunPending || r.Trigger != schedules.TriggerManual {
		t.Fatalf("RunNow = %+v", r)
	}
	if err := s.sched.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := s.svc.Runs(ctx, testActor(), id, domain.ScheduleRunID{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range page.Runs {
		if got.ID == r.ID {
			if got.Status != schedules.RunLaunched || got.JobID.IsZero() {
				t.Fatalf("run after a pass = %+v", got)
			}
			return got
		}
	}
	t.Fatalf("run %s is gone", r.ID)
	return schedules.Run{}
}

// checkScheduledRuns drives a schedule over a connected worker: Run now
// launches a job of the schedule that the worker runs to success; while a
// run's job is active, Run now is refused (overlap skip); afterwards it
// works again.
func checkScheduledRuns(t *testing.T, c *controller, w domain.WorkerID, rn *fakeRunner) {
	t.Helper()
	ctx := context.Background()
	s := newScheduling(t, c)

	sc := s.create(t, w, "Nightly", testDefinition())
	r := s.runNow(t, sc.ID)
	j := c.waitStatus(r.JobID, domain.JobSucceeded)
	if j.ScheduleID != sc.ID || j.WorkerID != w || rn.runCount(r.JobID) != 1 || j.Stats.Bytes != 100 {
		t.Errorf("scheduled job = %+v, runs %d", j, rn.runCount(r.JobID))
	}
	page, err := s.svc.Runs(ctx, testActor(), sc.ID, domain.ScheduleRunID{}, 0)
	if err != nil || len(page.Runs) != 1 || page.Runs[0].JobStatus != domain.JobSucceeded {
		t.Fatalf("runs = %+v (%v)", page.Runs, err)
	}
	js, _ := c.svc()
	list, err := js.List(ctx, testActor(), jobs.ListFilter{ScheduleID: sc.ID})
	if err != nil || len(list.Jobs) != 1 || list.Jobs[0].ID != r.JobID {
		t.Errorf("jobs of the schedule = %+v (%v)", list.Jobs, err)
	}

	held := s.create(t, w, "Held", heldDefinition())
	hr := s.runNow(t, held.ID)
	rn.waitStarted(t, hr.JobID)
	c.waitStatus(hr.JobID, domain.JobRunning)
	if _, err := s.svc.RunNow(ctx, testActor(), held.ID); !errors.Is(err, domain.ErrFailedPrecondition) {
		t.Errorf("RunNow while the run's job is active = %v, want failed precondition", err)
	}
	rn.release(hr.JobID)
	c.waitStatus(hr.JobID, domain.JobSucceeded)
	again := s.runNow(t, held.ID)
	rn.release(again.JobID)
	c.waitStatus(again.JobID, domain.JobSucceeded)
	if again.JobID == hr.JobID {
		t.Error("the second run reused the first run's job")
	}
}

// TestActiveScheduleRunNow: a schedule's Run now launches its job on an
// active worker over the link.
func TestActiveScheduleRunNow(t *testing.T) {
	t.Parallel()
	c := newController(t)
	id, secret := c.enrollActive("desktop")
	rn := newFakeRunner()
	startActive(t, rn, activeOptions(c, id, secret))
	c.waitWorker(id, "hello", func(w workers.Worker) bool { return w.Info.Hostname == "e2e" })
	checkScheduledRuns(t, c, id, rn)
}

// TestPassiveScheduleRunNow: the same over a passive worker, which the
// controller drives through its pinned TLS endpoint.
func TestPassiveScheduleRunNow(t *testing.T) {
	t.Parallel()
	c := newController(t)
	rn := newFakeRunner()
	secret := pairingSecret()
	pw := startPassive(t, rn, secret)
	_, ws := c.svc()
	reg, err := ws.RegisterPassive(context.Background(), workers.PassiveParams{
		Name: "server", URL: pw.url, CertFingerprint: pw.fingerprint, Secret: secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.wakeSupervisor()
	c.waitWorker(reg.ID, "hello", func(w workers.Worker) bool { return w.Info.Hostname == "e2e" })
	checkScheduledRuns(t, c, reg.ID, rn)
}
