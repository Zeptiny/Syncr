package scheduler_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/controller/scheduler"
	"github.com/Zeptiny/Syncr/internal/domain"
)

func TestNewRequiresDeps(t *testing.T) {
	t.Parallel()
	if _, err := scheduler.New(scheduler.Deps{}, scheduler.Config{}); err == nil {
		t.Error("New accepted missing dependencies")
	}
	e := newEnv(t)
	_, err := scheduler.New(scheduler.Deps{
		Schedules: e.svc, Jobs: e.jobs, Workers: e.workers, Kinds: e.kinds, Owners: e.owners, IDs: nil, Clock: e.clock,
	}, scheduler.Config{})
	if err == nil {
		t.Error("New accepted a nil ID generator")
	}
}

// TestByWorkerOfflineThenOnline: a run waits for its offline worker and
// launches exactly one job once the worker is back within the deadline.
func TestByWorkerOfflineThenOnline(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.addWorker(worker{name: "desktop"})
	sc := e.create(e.input("nightly", schedules.Target{WorkerID: w})) // 13:00, deadline +30m
	s := e.newScheduler(nil, nil)

	e.clock.Set(t0.Add(time.Hour))
	e.tick(s)
	r := e.onlyRun(sc.ID)
	if r.Status != schedules.RunPending || !r.ScheduledFor.Equal(t0.Add(time.Hour)) || len(e.jobsOf(sc.ID)) != 0 {
		t.Fatalf("with the worker offline: run %+v, jobs %d", r, len(e.jobsOf(sc.ID)))
	}
	e.clock.Advance(20 * time.Minute)
	e.tick(s)
	if r := e.onlyRun(sc.ID); r.Status != schedules.RunPending {
		t.Fatalf("still offline: run %+v", r)
	}
	e.online(w)
	e.tick(s)
	js := e.jobsOf(sc.ID)
	r = e.onlyRun(sc.ID)
	if len(js) != 1 || r.Status != schedules.RunLaunched || r.JobID != js[0].ID || r.WorkerID != w {
		t.Fatalf("after the worker came back: run %+v, jobs %+v", r, js)
	}
	j := js[0]
	if j.WorkerID != w || j.OwnerID != e.alice.UserID || j.ScheduleID != sc.ID || j.Status != domain.JobQueued ||
		j.Kind != "rclone_transfer" || string(j.Definition) != localDefinition {
		t.Errorf("job = %+v", j)
	}
	if r.JobStatus != domain.JobQueued {
		t.Errorf("run job status = %q", r.JobStatus)
	}
	for range 3 {
		e.tick(s)
	}
	if js := e.jobsOf(sc.ID); len(js) != 1 {
		t.Errorf("later ticks created %d jobs", len(js))
	}
}

// TestPastDeadlineIsMissed: a run whose worker stays offline past its
// deadline is missed; the worker coming back later starts nothing.
func TestPastDeadlineIsMissed(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		missed schedules.MissedPolicy
		late   time.Duration
	}{
		{"run late", schedules.MissedPolicy{Action: schedules.MissedRunLate, MaxDelay: 30 * time.Minute}, 31 * time.Minute},
		{"skip", schedules.MissedPolicy{Action: schedules.MissedSkip}, schedules.SkipGrace + time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			w := e.addWorker(worker{name: "desktop"})
			in := e.input("daily", schedules.Target{WorkerID: w})
			in.Cron, in.Missed = "0 13 * * *", tt.missed
			sc := e.create(in)
			s := e.newScheduler(nil, nil)
			e.clock.Set(t0.Add(time.Hour))
			e.tick(s)
			e.clock.Advance(tt.late)
			e.tick(s)
			r := e.onlyRun(sc.ID)
			if r.Status != schedules.RunMissed || r.Reason != schedules.ReasonWorkerOffline || r.Message == "" {
				t.Fatalf("run = %+v, want missed (worker offline)", r)
			}
			e.online(w)
			e.tick(s)
			if js := e.jobsOf(sc.ID); len(js) != 0 {
				t.Errorf("a missed run launched %d jobs", len(js))
			}
			if got := e.schedule(sc.ID); !got.Enabled {
				t.Error("a missed run paused the schedule")
			}
		})
	}
}

func TestByLabel(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	busy := e.addWorker(worker{name: "busy", online: true, labels: []string{"nas"}})
	e.busyJob(busy)
	e.busyJob(busy)
	idle := e.addWorker(worker{name: "idle", online: true, labels: []string{"nas"}})
	e.addWorker(worker{name: "restic-only", online: true, labels: []string{"nas"}, kinds: []string{"restic_backup"}})
	e.addWorker(worker{name: "asleep", labels: []string{"nas"}})
	e.addWorker(worker{name: "no-label", online: true})
	sc := e.create(e.input("by label", schedules.Target{Label: "nas"}))
	s := e.newScheduler(nil, nil)
	e.clock.Set(t0.Add(time.Hour))
	e.tick(s)
	js := e.jobsOf(sc.ID)
	if len(js) != 1 || js[0].WorkerID != idle {
		t.Fatalf("jobs = %+v, want one on the idle worker", js)
	}
	// Tied on unfinished jobs: by name.
	e2 := newEnv(t)
	b := e2.addWorker(worker{name: "Bravo", online: true, labels: []string{"nas"}})
	e2.addWorker(worker{name: "charlie", online: true, labels: []string{"nas"}})
	sc2 := e2.create(e2.input("tie", schedules.Target{Label: "nas"}))
	e2.clock.Set(t0.Add(time.Hour))
	e2.tick(e2.newScheduler(nil, nil))
	if js := e2.jobsOf(sc2.ID); len(js) != 1 || js[0].WorkerID != b {
		t.Errorf("tie: jobs = %+v, want the first by name", js)
	}
}

func TestByLabelWithoutAWorker(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		workers []worker
		want    schedules.RunStatus
	}{
		{"no worker has the label", []worker{{name: "other", online: true, labels: []string{"other"}}}, schedules.RunFailed},
		{"none runs the kind", []worker{{name: "restic", online: true, labels: []string{"nas"}, kinds: []string{"restic_backup"}}}, schedules.RunFailed},
		// Offline (or never reported) workers with the label may still
		// take it: the run waits.
		{"all offline", []worker{{name: "asleep", labels: []string{"nas"}}}, schedules.RunPending},
		{"never reported", []worker{{name: "new", online: true, labels: []string{"nas"}, kinds: []string{}}}, schedules.RunPending},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			for _, w := range tt.workers {
				e.addWorker(w)
			}
			sc := e.create(e.input("by label", schedules.Target{Label: "nas"}))
			e.clock.Set(t0.Add(time.Hour))
			e.tick(e.newScheduler(nil, nil))
			r := e.onlyRun(sc.ID)
			if r.Status != tt.want || len(e.jobsOf(sc.ID)) != 0 {
				t.Fatalf("run = %+v, want %s", r, tt.want)
			}
			if r.Status == schedules.RunFailed && r.Reason != schedules.ReasonNoWorkerWithLabel {
				t.Errorf("reason = %q", r.Reason)
			}
			if !e.schedule(sc.ID).Enabled {
				t.Error("a missing label worker paused the schedule")
			}
		})
	}
}

// TestRestartAfterMarkLaunching: a scheduler that fails after reserving
// the job ID is replaced by a new one over the same store; the job is
// created once, with the reserved ID.
func TestRestartAfterMarkLaunching(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.addWorker(worker{name: "nas", online: true})
	sc := e.create(e.input("nightly", schedules.Target{WorkerID: w}))
	e.clock.Set(t0.Add(time.Hour))

	broken := &faultyJobs{Jobs: e.jobs, failCreate: errInjected}
	e.heartbeat()
	if err := e.newScheduler(broken, nil).Tick(context.Background()); !errors.Is(err, errInjected) {
		t.Fatalf("Tick with a failing Create = %v", err)
	}
	r := e.onlyRun(sc.ID)
	if r.Status != schedules.RunLaunching || r.JobID.IsZero() || len(e.jobsOf(sc.ID)) != 0 {
		t.Fatalf("after the failure: run %+v", r)
	}
	reserved := r.JobID

	// The restarted controller's scheduler, past the deadline even: the
	// launch was decided in time.
	e.clock.Advance(time.Hour + 5*time.Minute)
	fresh := e.newScheduler(nil, nil)
	e.tick(fresh)
	js := e.jobsOf(sc.ID)
	if len(js) != 1 || js[0].ID != reserved {
		t.Fatalf("jobs after the restart = %+v, want one with ID %s", js, reserved)
	}
	if r := e.runs(sc.ID); r[len(r)-1].Status != schedules.RunLaunched || r[len(r)-1].JobID != reserved {
		t.Errorf("run after the restart = %+v", r[len(r)-1])
	}
	// A third tick creates nothing new. (The 14:00 occurrence, due when the
	// new scheduler started, was missed: the launching run was active.)
	e.tick(fresh)
	if n := countJob(e.jobsOf(sc.ID), reserved); n != 1 {
		t.Errorf("job %s exists %d times", reserved, n)
	}
}

// TestRestartAfterCreate: a crash after the job was created but before the
// run was closed; the next scheduler closes the run without a second job.
func TestRestartAfterCreate(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.addWorker(worker{name: "nas", online: true})
	sc := e.create(e.input("nightly", schedules.Target{WorkerID: w}))
	e.clock.Set(t0.Add(time.Hour))
	e.heartbeat()
	if err := e.newScheduler(nil, faultySchedules{e.svc}).Tick(context.Background()); !errors.Is(err, errInjected) {
		t.Fatalf("Tick with a failing MarkLaunched = %v", err)
	}
	r := e.onlyRun(sc.ID)
	if js := e.jobsOf(sc.ID); r.Status != schedules.RunLaunching || len(js) != 1 || js[0].ID != r.JobID {
		t.Fatalf("after the crash: run %+v, jobs %+v", r, js)
	}
	s := e.newScheduler(nil, nil)
	e.tick(s)
	e.tick(s)
	if r2 := e.onlyRun(sc.ID); r2.Status != schedules.RunLaunched || r2.JobID != r.JobID || len(e.jobsOf(sc.ID)) != 1 {
		t.Errorf("after the restart: run %+v, jobs %d", r2, len(e.jobsOf(sc.ID)))
	}
}

// TestConcurrentSchedulers: two schedulers (two controllers) ticking at
// once fire the occurrence once and create one job.
func TestConcurrentSchedulers(t *testing.T) {
	t.Parallel()
	for _, overlap := range []schedules.Overlap{schedules.OverlapSkip, schedules.OverlapAllow} {
		t.Run(string(overlap), func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			w := e.addWorker(worker{name: "nas", online: true})
			in := e.input("nightly", schedules.Target{WorkerID: w})
			in.Overlap = overlap
			sc := e.create(in)
			a, b := e.newScheduler(nil, nil), e.newScheduler(nil, nil)
			e.clock.Set(t0.Add(time.Hour))
			e.heartbeat()
			var wg sync.WaitGroup
			for i := range 8 {
				s := a
				if i%2 == 1 {
					s = b
				}
				wg.Go(func() {
					if err := s.Tick(context.Background()); err != nil {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			r := e.onlyRun(sc.ID)
			js := e.jobsOf(sc.ID)
			if r.Status != schedules.RunLaunched || len(js) != 1 || js[0].ID != r.JobID {
				t.Errorf("run %+v, jobs %+v; want one launched run and one job", r, js)
			}
		})
	}
}

func TestRunNowWithSkipOverlapWhileActive(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	w := e.addWorker(worker{name: "nas", online: true})
	sc := e.create(e.input("nightly", schedules.Target{WorkerID: w}))
	s := e.newScheduler(nil, nil)
	r, err := e.svc.RunNow(ctx, e.alice, sc.ID)
	if err != nil || r.Trigger != schedules.TriggerManual {
		t.Fatalf("RunNow = (%+v, %v)", r, err)
	}
	e.tick(s)
	if js := e.jobsOf(sc.ID); len(js) != 1 {
		t.Fatalf("jobs = %d", len(js))
	}
	_, err = e.svc.RunNow(ctx, e.alice, sc.ID)
	if !errors.Is(err, domain.ErrFailedPrecondition) {
		t.Fatalf("RunNow while the job is queued = %v, want ErrFailedPrecondition", err)
	}
	// Due while active: missed, reason overlap.
	e.clock.Set(t0.Add(time.Hour))
	e.tick(s)
	rs := e.runs(sc.ID)
	if len(rs) != 2 || rs[0].Status != schedules.RunMissed || rs[0].Reason != schedules.ReasonOverlap || len(e.jobsOf(sc.ID)) != 1 {
		t.Errorf("runs = %+v", rs)
	}
}

func TestOverlapQueueWaits(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	w := e.addWorker(worker{name: "nas", online: true})
	in := e.input("queued", schedules.Target{WorkerID: w})
	in.Overlap = schedules.OverlapQueue
	sc := e.create(in)
	s := e.newScheduler(nil, nil)
	if _, err := e.svc.RunNow(ctx, e.alice, sc.ID); err != nil {
		t.Fatal(err)
	}
	e.tick(s)
	first := e.jobsOf(sc.ID)
	if len(first) != 1 {
		t.Fatal("the first run did not launch")
	}
	e.clock.Set(t0.Add(time.Hour))
	e.tick(s)
	if r := e.runs(sc.ID)[0]; r.Status != schedules.RunPending || len(e.jobsOf(sc.ID)) != 1 {
		t.Fatalf("behind an active job: run %+v", r)
	}
	e.finish(first[0])
	e.tick(s)
	if js := e.jobsOf(sc.ID); len(js) != 2 {
		t.Fatalf("after the active job finished: %d jobs", len(js))
	}
	// A run still behind an active job at its deadline is missed (overlap).
	e.clock.Set(t0.Add(2 * time.Hour))
	e.tick(s)
	e.clock.Set(t0.Add(2*time.Hour + 31*time.Minute))
	e.tick(s)
	if r := e.runs(sc.ID)[0]; r.Status != schedules.RunMissed || r.Reason != schedules.ReasonOverlap {
		t.Errorf("run behind an active job at its deadline = %+v", r)
	}
}

func TestOverlapAllowLaunchesAlongside(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	w := e.addWorker(worker{name: "nas", online: true})
	in := e.input("parallel", schedules.Target{WorkerID: w})
	in.Overlap = schedules.OverlapAllow
	sc := e.create(in)
	s := e.newScheduler(nil, nil)
	for range 2 {
		if _, err := e.svc.RunNow(ctx, e.alice, sc.ID); err != nil {
			t.Fatal(err)
		}
		e.tick(s)
	}
	if js := e.jobsOf(sc.ID); len(js) != 2 {
		t.Errorf("jobs = %d, want 2 running alongside", len(js))
	}
}

// TestPermanentFailuresPause: launch failures that every later run would
// hit too fail the run and pause the schedule.
func TestPermanentFailuresPause(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		breakIt func(e *env, sc schedules.Schedule, w domain.WorkerID, remote domain.RemoteID)
		reason  string
	}{
		{"remote deleted before launch", func(e *env, _ schedules.Schedule, _ domain.WorkerID, r domain.RemoteID) {
			e.remotes.remove(r)
		}, schedules.ReasonRemoteUnavailable},
		{"worker removed", func(e *env, _ schedules.Schedule, w domain.WorkerID, _ domain.RemoteID) {
			if err := e.workers.Remove(context.Background(), w); err != nil {
				e.t.Fatal(err)
			}
		}, schedules.ReasonWorkerRemoved},
		{"owner disabled", func(e *env, sc schedules.Schedule, _ domain.WorkerID, _ domain.RemoteID) {
			e.owners.disable(sc.OwnerID)
		}, schedules.ReasonOwnerDisabled},
		{"definition no longer valid", func(e *env, sc schedules.Schedule, _ domain.WorkerID, _ domain.RemoteID) {
			bad := e.schedule(sc.ID)
			bad.Definition = []byte(`{"rclone_transfer":{"mode":"RCLONE_TRANSFER_MODE_COPY","source":{"path":"relative"},"destination":{"path":"/backup"}}}`)
			if _, err := e.store.UpdateSchedule(context.Background(), bad, false); err != nil {
				e.t.Fatal(err)
			}
		}, schedules.ReasonInvalidDefinition},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			w := e.addWorker(worker{name: "nas", online: true})
			remote := e.addRemote("photos")
			in := e.input("nightly", schedules.Target{WorkerID: w})
			in.Definition, in.RemoteIDs = []byte(remoteDefinition(remote)), []domain.RemoteID{remote}
			sc := e.create(in)
			e.clock.Set(t0.Add(time.Hour))
			e.tick(e.newScheduler(nil, nil)) // nothing broken yet: one job
			if js := e.jobsOf(sc.ID); len(js) != 1 {
				t.Fatalf("jobs = %d", len(js))
			}
			e.finish(e.jobsOf(sc.ID)[0])
			tt.breakIt(e, sc, w, remote)
			e.clock.Set(t0.Add(2 * time.Hour))
			e.tick(e.newScheduler(nil, nil))
			r := e.runs(sc.ID)[0]
			if r.Status != schedules.RunFailed || r.Reason != tt.reason || r.Message == "" {
				t.Fatalf("run = %+v, want failed %s", r, tt.reason)
			}
			got := e.schedule(sc.ID)
			if got.Enabled || got.PausedReason != tt.reason || !got.NextRunAt.IsZero() {
				t.Errorf("schedule = %+v, want paused (%s)", got, tt.reason)
			}
			if js := e.jobsOf(sc.ID); len(js) != 1 {
				t.Errorf("jobs = %d, want the first only", len(js))
			}
		})
	}
}

// TestWorkerGoneDuringLaunch: the worker disappears between the decision
// and the job's creation; the run goes back to pending, keeps its job ID
// and launches with it once possible.
func TestWorkerGoneDuringLaunch(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.addWorker(worker{name: "nas", online: true})
	sc := e.create(e.input("nightly", schedules.Target{WorkerID: w}))
	e.clock.Set(t0.Add(time.Hour))
	gone := &faultyJobs{Jobs: e.jobs, failCreate: jobs.ErrUnknownWorker}
	e.tick(e.newScheduler(gone, nil))
	r := e.onlyRun(sc.ID)
	if r.Status != schedules.RunPending || r.JobID.IsZero() {
		t.Fatalf("run = %+v, want pending with its reserved job", r)
	}
	e.tick(e.newScheduler(nil, nil))
	if r2 := e.onlyRun(sc.ID); r2.Status != schedules.RunLaunched || r2.JobID != r.JobID {
		t.Errorf("run = %+v, want launched with job %s", r2, r.JobID)
	}
}

// TestTransientCreateErrorRetries: other errors keep the run launching and
// the next pass retries with the same job ID.
func TestTransientCreateErrorRetries(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.addWorker(worker{name: "nas", online: true})
	sc := e.create(e.input("nightly", schedules.Target{WorkerID: w}))
	e.clock.Set(t0.Add(time.Hour))
	flaky := &faultyJobs{Jobs: e.jobs, failCreate: errInjected}
	s := e.newScheduler(flaky, nil)
	e.heartbeat()
	for range 3 {
		if err := s.Tick(context.Background()); !errors.Is(err, errInjected) {
			t.Fatalf("Tick = %v", err)
		}
	}
	reserved := e.onlyRun(sc.ID).JobID
	flaky.mu.Lock()
	flaky.failCreate = nil
	flaky.mu.Unlock()
	e.tick(s)
	if r := e.onlyRun(sc.ID); r.Status != schedules.RunLaunched || r.JobID != reserved || len(e.jobsOf(sc.ID)) != 1 {
		t.Errorf("run = %+v", r)
	}
}

// TestRunAndWake: Run passes at once, again when woken, and stops with its
// context.
func TestRunAndWake(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	passes := make(chan struct{}, 4)
	created := make(chan domain.JobID, 1)
	s, err := scheduler.New(scheduler.Deps{
		Schedules: countingSchedules{Schedules: e.svc, passes: passes}, Jobs: signallingJobs{Jobs: e.jobs, created: created},
		Workers: e.workers, Kinds: e.kinds, Owners: e.owners, IDs: idgen.UUIDv7{}, Clock: e.clock, Signal: e.signal,
	}, scheduler.Config{Tick: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	<-passes // at once; the ticker is an hour away
	w := e.addWorker(worker{name: "nas", online: true})
	sc := e.create(e.input("now", schedules.Target{WorkerID: w})) // wakes through the shared Signal
	<-passes
	if _, err := e.svc.RunNow(ctx, e.alice, sc.ID); err != nil {
		t.Fatal(err)
	}
	id := <-created // by the pass RunNow woke
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run = %v", err)
	}
	if js := e.jobsOf(sc.ID); len(js) != 1 || js[0].ID != id {
		t.Errorf("jobs = %+v", js)
	}
	s.Wake() // never blocks, even with nobody running
	s.Wake()
}

func countJob(js []jobs.Job, id domain.JobID) int {
	return len(slices.DeleteFunc(slices.Clone(js), func(j jobs.Job) bool { return j.ID != id }))
}
