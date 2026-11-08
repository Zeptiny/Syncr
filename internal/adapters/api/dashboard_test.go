package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/adapters/broker"
	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// fakeDashSchedules is the schedules part of the dashboard: canned
// upcoming runs and problems, and a record of the calls.
type fakeDashSchedules struct {
	ScheduleService

	mu       sync.Mutex
	upcoming []schedules.Upcoming
	problems []schedules.Run
	missed   int64
	actors   []domain.Actor
	from     time.Time
	until    time.Time
}

func (f *fakeDashSchedules) Upcoming(_ context.Context, actor domain.Actor, limit int) ([]schedules.Upcoming, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actors = append(f.actors, actor)
	return f.upcoming[:min(limit, len(f.upcoming))], nil
}

func (f *fakeDashSchedules) RecentProblems(_ context.Context, actor domain.Actor, limit int) ([]schedules.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actors = append(f.actors, actor)
	return f.problems[:min(limit, len(f.problems))], nil
}

func (f *fakeDashSchedules) CountMissed(_ context.Context, actor domain.Actor, from, until time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actors = append(f.actors, actor)
	f.from, f.until = from, until
	return f.missed, nil
}

// dashEnv is a dashboard handler over the test env's store, with the jobs
// service's activity port wired and a fixed clock.
type dashEnv struct {
	*testEnv
	h     *dashboardHandler
	jobs  *jobs.Service
	sched *fakeDashSchedules
}

func newDashEnv(t *testing.T) *dashEnv {
	t.Helper()
	e := newEnv(t)
	js, err := jobs.New(jobs.Deps{
		Jobs: e.store, Samples: e.store, Clock: e.clock, IDs: &idgen.Sequential{},
		Broker: broker.New(), Notifier: jobs.NopNotifier{}, Waker: nopWaker{}, Remotes: e.remotes, Activity: e.store,
	}, jobs.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sched := &fakeDashSchedules{}
	return &dashEnv{
		testEnv: e, jobs: js, sched: sched,
		h: &dashboardHandler{
			jobs: js, activity: js, workers: e.wsvc, schedules: sched, remotes: e.remotes,
			errs: errorMapper{log: newTestLogger(e.logs)}, now: e.clock.Now,
		},
	}
}

// finish stores a job of owner that ended with status at the given time.
func (d *dashEnv) finish(t *testing.T, owner domain.Actor, status domain.JobStatus, at time.Time, bytes int64) jobs.Job {
	t.Helper()
	j, err := d.jobs.Create(context.Background(), owner, jobs.CreateParams{WorkerID: d.worker, Kind: "rclone_transfer", Definition: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if status.Terminal() {
		j.Status, j.FinishedAt, j.Stats = status, at, jobs.Stats{Bytes: bytes, Files: 2}
	} else {
		j.Status = status
	}
	if j, err = d.store.UpdateJob(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	return j
}

func (d *dashEnv) get(t *testing.T, actor domain.Actor, req *syncrv1.GetDashboardRequest) *syncrv1.GetDashboardResponse {
	t.Helper()
	ctx := WithCaller(context.Background(), Caller{Actor: actor, Method: AuthSession})
	resp, err := d.h.GetDashboard(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg
}

func TestDashboardDayRange(t *testing.T) {
	t.Parallel()
	d := newDashEnv(t)
	now := d.clock.Now() // 2026-11-04 12:00 UTC
	d.clock.Advance(17 * time.Minute)
	d.finish(t, testUser, domain.JobSucceeded, now.Add(-30*time.Minute), 100)
	d.finish(t, testUser, domain.JobSucceeded, now.Add(10*time.Minute), 50) // the current, partial hour
	d.finish(t, testUser, domain.JobFailed, now.Add(-5*time.Hour), 7)
	d.finish(t, testUser, domain.JobInterrupted, now.Add(-23*time.Hour), 1)
	d.finish(t, testUser, domain.JobCancelled, now.Add(-2*time.Hour), 0)
	d.finish(t, testUser, domain.JobSucceeded, now.Add(-30*time.Hour), 1000) // before the range
	d.finish(t, testOther, domain.JobSucceeded, now.Add(-time.Hour), 1000)   // another user's
	d.finish(t, testUser, domain.JobRunning, time.Time{}, 0)
	d.finish(t, testUser, domain.JobQueued, time.Time{}, 0)
	d.finish(t, testUser, domain.JobDispatched, time.Time{}, 0)
	d.sched.missed = 3

	msg := d.get(t, testUser, &syncrv1.GetDashboardRequest{})
	wantFrom := time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC)
	if !msg.GetFrom().AsTime().Equal(wantFrom) || !msg.GetUntil().AsTime().Equal(d.clock.Now()) || msg.GetBucketUnit() != "hour" {
		t.Errorf("range = [%v, %v) by %q", msg.GetFrom().AsTime(), msg.GetUntil().AsTime(), msg.GetBucketUnit())
	}
	if len(msg.GetActivity()) != 24 {
		t.Fatalf("%d buckets, want 24", len(msg.GetActivity()))
	}
	last := msg.GetActivity()[23]
	if !last.GetStart().AsTime().Equal(now) || last.GetSucceeded() != 1 || last.GetBytes() != 50 {
		t.Errorf("last bucket = %v", last)
	}
	tot := msg.GetTotals()
	if tot.GetSucceeded() != 2 || tot.GetFailed() != 1 || tot.GetInterrupted() != 1 || tot.GetCancelled() != 1 ||
		tot.GetBytes() != 158 || tot.GetFiles() != 10 || tot.GetSuccessRate() != 0.5 ||
		tot.GetRunning() != 1 || tot.GetQueued() != 2 || tot.GetMissedRuns() != 3 {
		t.Errorf("totals = %v", tot)
	}
	if !d.sched.from.Equal(wantFrom) || !d.sched.until.Equal(d.clock.Now()) {
		t.Errorf("CountMissed range = [%v, %v)", d.sched.from, d.sched.until)
	}
	if len(msg.GetRecentFailures()) != 2 || msg.GetRecentFailures()[0].GetStatus() != syncrv1.JobStatus_JOB_STATUS_FAILED &&
		msg.GetRecentFailures()[0].GetStatus() != syncrv1.JobStatus_JOB_STATUS_INTERRUPTED {
		t.Errorf("recent failures = %v", msg.GetRecentFailures())
	}
	for _, a := range d.sched.actors {
		if a != testUser {
			t.Errorf("schedules asked as %v", a)
		}
	}
}

func TestDashboardAllUsersIsForAdmins(t *testing.T) {
	t.Parallel()
	d := newDashEnv(t)
	now := d.clock.Now()
	d.finish(t, testUser, domain.JobSucceeded, now.Add(-time.Hour), 1)
	d.finish(t, testOther, domain.JobFailed, now.Add(-time.Hour), 2)

	user := d.get(t, testUser, &syncrv1.GetDashboardRequest{AllUsers: true})
	if tot := user.GetTotals(); tot.GetSucceeded() != 1 || tot.GetFailed() != 0 || len(user.GetRecentFailures()) != 0 {
		t.Errorf("a user's all_users dashboard = %v, %v", tot, user.GetRecentFailures())
	}
	admin := d.get(t, testAdmin, &syncrv1.GetDashboardRequest{AllUsers: true})
	if tot := admin.GetTotals(); tot.GetSucceeded() != 1 || tot.GetFailed() != 1 || len(admin.GetRecentFailures()) != 1 {
		t.Errorf("an admin's all_users dashboard = %v, %v", tot, admin.GetRecentFailures())
	}
	own := d.get(t, testAdmin, &syncrv1.GetDashboardRequest{})
	if tot := own.GetTotals(); tot.GetSucceeded()+tot.GetFailed() != 0 {
		t.Errorf("an admin's own dashboard = %v", tot)
	}
}

func TestDashboardDaysInTimeZone(t *testing.T) {
	t.Parallel()
	d := newDashEnv(t)
	// 2026-10-31 18:00 UTC is 19:00 CET; the week covers Oct 25 (a 25-hour
	// day in Berlin) to Oct 31.
	d.clock.Set(time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC))
	d.finish(t, testUser, domain.JobSucceeded, time.Date(2026, 10, 24, 22, 30, 0, 0, time.UTC), 1) // 00:30 CEST on Oct 25
	d.finish(t, testUser, domain.JobSucceeded, time.Date(2026, 10, 24, 21, 30, 0, 0, time.UTC), 1) // Oct 24: before the range

	msg := d.get(t, testUser, &syncrv1.GetDashboardRequest{Range: syncrv1.DashboardRange_DASHBOARD_RANGE_WEEK, Timezone: "Europe/Berlin"})
	if msg.GetBucketUnit() != "day" || len(msg.GetActivity()) != 7 {
		t.Fatalf("unit %q, %d buckets", msg.GetBucketUnit(), len(msg.GetActivity()))
	}
	first, second := msg.GetActivity()[0], msg.GetActivity()[1]
	if !first.GetStart().AsTime().Equal(time.Date(2026, 10, 24, 22, 0, 0, 0, time.UTC)) || first.GetSucceeded() != 1 ||
		!second.GetStart().AsTime().Equal(time.Date(2026, 10, 25, 23, 0, 0, 0, time.UTC)) {
		t.Errorf("first buckets = %v, %v", first, second)
	}
	if !msg.GetFrom().AsTime().Equal(first.GetStart().AsTime()) {
		t.Errorf("from = %v", msg.GetFrom().AsTime())
	}

	month := d.get(t, testUser, &syncrv1.GetDashboardRequest{Range: syncrv1.DashboardRange_DASHBOARD_RANGE_MONTH, Timezone: "Asia/Kolkata"})
	if len(month.GetActivity()) != 30 || !month.GetActivity()[29].GetStart().AsTime().Equal(time.Date(2026, 10, 30, 18, 30, 0, 0, time.UTC)) {
		t.Errorf("month: %d buckets, last %v", len(month.GetActivity()), month.GetActivity()[len(month.GetActivity())-1])
	}
}

func TestDashboardWorkersAndSchedules(t *testing.T) {
	t.Parallel()
	d := newDashEnv(t)
	ctx := context.Background()
	// The env's worker never connected (pending); nas-01 and nas-02 have
	// the label "nas", nas-02 is online, office is offline.
	nas1, nas2, office := d.addWorker(), d.addWorker(), d.addWorker()
	for id, name := range map[domain.WorkerID]string{nas1: "nas-01", nas2: "nas-02", office: "office"} {
		if _, err := d.wsvc.Rename(ctx, id, name); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []domain.WorkerID{nas1, nas2} {
		if _, err := d.wsvc.SetLabels(ctx, id, []string{"nas"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.store.TouchWorker(ctx, nas2, d.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := d.store.TouchWorker(ctx, office, d.clock.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	at := d.clock.Now().Add(time.Hour)
	sid := domain.NewScheduleID()
	d.sched.upcoming = []schedules.Upcoming{
		{ScheduleID: sid, Name: "Nightly photos", At: at, Target: schedules.Target{Label: "nas"}},
		{ScheduleID: domain.NewScheduleID(), Name: "Office docs", At: at.Add(time.Hour), Target: schedules.Target{WorkerID: office}},
		{ScheduleID: domain.NewScheduleID(), Name: "Nowhere", At: at.Add(2 * time.Hour), Target: schedules.Target{Label: "none"}},
	}
	runID, jobID := domain.NewScheduleRunID(), domain.NewJobID()
	d.sched.problems = []schedules.Run{
		{ID: runID, ScheduleID: sid, Trigger: schedules.TriggerCron, Status: schedules.RunMissed, Reason: schedules.ReasonWorkerOffline,
			ScheduledFor: at, Deadline: at.Add(time.Hour), Coalesced: 2},
		{ID: domain.NewScheduleRunID(), ScheduleID: sid, Trigger: schedules.TriggerManual, Status: schedules.RunFailed,
			WorkerID: nas1, JobID: jobID, JobStatus: domain.JobFailed},
	}

	msg := d.get(t, testUser, &syncrv1.GetDashboardRequest{})
	health := msg.GetWorkers()
	if health.GetOnline() != 1 || health.GetOffline() != 1 || health.GetPending() != 2 || len(health.GetWorkers()) != 4 {
		t.Errorf("worker health = %v", health)
	}
	up := msg.GetUpcoming()
	if len(up) != 3 {
		t.Fatalf("upcoming = %v", up)
	}
	if up[0].GetScheduleId() != sid.String() || up[0].GetScheduleName() != "Nightly photos" || !up[0].GetAt().AsTime().Equal(at) ||
		up[0].GetTarget().GetWorkerLabel() != "nas" || up[0].GetWorkerName() != "nas-02" ||
		up[0].GetWorkerStatus() != syncrv1.WorkerStatus_WORKER_STATUS_ONLINE {
		t.Errorf("label target = %v", up[0])
	}
	if up[1].GetTarget().GetWorkerId() != office.String() || up[1].GetWorkerName() != "office" ||
		up[1].GetWorkerStatus() != syncrv1.WorkerStatus_WORKER_STATUS_OFFLINE {
		t.Errorf("worker target = %v", up[1])
	}
	if up[2].GetWorkerName() != "" || up[2].GetWorkerStatus() != syncrv1.WorkerStatus_WORKER_STATUS_UNSPECIFIED {
		t.Errorf("unknown label target = %v", up[2])
	}
	runs := msg.GetRecentMissed()
	if len(runs) != 2 || runs[0].GetId() != runID.String() || runs[0].GetStatus() != syncrv1.ScheduleRunStatus_SCHEDULE_RUN_STATUS_MISSED ||
		runs[0].GetTrigger() != syncrv1.ScheduleRunTrigger_SCHEDULE_RUN_TRIGGER_CRON || runs[0].GetReason() != "worker_offline" ||
		runs[0].GetCoalesced() != 2 || runs[0].GetJobId() != "" ||
		runs[1].GetStatus() != syncrv1.ScheduleRunStatus_SCHEDULE_RUN_STATUS_FAILED || runs[1].GetJobId() != jobID.String() ||
		runs[1].GetWorkerId() != nas1.String() || runs[1].GetJobStatus() != syncrv1.JobStatus_JOB_STATUS_FAILED {
		t.Errorf("recent missed = %v", runs)
	}
}

func TestDashboardRejectsBadTimeZones(t *testing.T) {
	t.Parallel()
	d := newDashEnv(t)
	ctx := WithCaller(context.Background(), Caller{Actor: testUser, Method: AuthSession})
	for _, tz := range []string{"Local", "Mars/Olympus", "../../etc/passwd", "/etc/localtime", `Europe\Berlin`, string(make([]byte, 100))} {
		_, err := d.h.GetDashboard(ctx, connect.NewRequest(&syncrv1.GetDashboardRequest{Timezone: tz}))
		if codeOf(t, err) != connect.CodeInvalidArgument {
			t.Errorf("timezone %q: %v, want InvalidArgument", tz, err)
		}
	}
	for _, tz := range []string{"", "UTC", "America/New_York"} {
		if _, err := d.h.GetDashboard(ctx, connect.NewRequest(&syncrv1.GetDashboardRequest{Timezone: tz})); err != nil {
			t.Errorf("timezone %q: %v", tz, err)
		}
	}
}

// TestDashboardIsMounted checks the wiring through NewHandler: the service
// is served once the jobs service has its dashboard use cases and the
// schedules are wired, to every signed-in user.
func TestDashboardIsMounted(t *testing.T) {
	t.Parallel()
	sched := &fakeDashSchedules{}
	e := newEnv(t, func(d *Deps) { d.Schedules = sched })
	c := syncrv1connect.NewDashboardServiceClient(e.clientWith(testActorHeader, actorHeader(testUser, "session")), e.srv.URL)
	// The harness's jobs service has no activity repository: the call
	// reaches it and fails inside, not with Unimplemented.
	_, err := c.GetDashboard(t.Context(), connect.NewRequest(&syncrv1.GetDashboardRequest{}))
	if code := codeOf(t, err); code == connect.CodeUnimplemented || code == connect.CodePermissionDenied || code == connect.CodeUnauthenticated {
		t.Errorf("GetDashboard = %v; the service is not mounted for users", err)
	}
	anon := syncrv1connect.NewDashboardServiceClient(e.srv.Client(), e.srv.URL)
	_, err = anon.GetDashboard(t.Context(), connect.NewRequest(&syncrv1.GetDashboardRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)

	// Without schedules it is not mounted.
	bare := newEnv(t)
	c = syncrv1connect.NewDashboardServiceClient(bare.clientWith(testActorHeader, actorHeader(testUser, "session")), bare.srv.URL)
	_, err = c.GetDashboard(t.Context(), connect.NewRequest(&syncrv1.GetDashboardRequest{}))
	wantCode(t, err, connect.CodeUnimplemented)
}
