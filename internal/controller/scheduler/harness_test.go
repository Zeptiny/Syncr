package scheduler_test

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/broker"
	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/adapters/memstore"
	"github.com/Zeptiny/Syncr/internal/adapters/secrets"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/controller/scheduler"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/platform/clock"
)

// t0 is the tests' start: Wednesday 2026-11-04 12:00 UTC.
var t0 = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

const localDefinition = `{"rclone_transfer":{"mode":"RCLONE_TRANSFER_MODE_COPY","source":{"path":"/data"},"destination":{"path":"/backup"}}}`

// remoteDefinition copies from a remote to the worker's disk.
func remoteDefinition(id domain.RemoteID) string {
	return fmt.Sprintf(`{"rclone_transfer":{"mode":"RCLONE_TRANSFER_MODE_COPY","source":{"remote_id":%q,"path":"photos"},"destination":{"path":"/backup"}}}`, id)
}

// env wires the real schedules, jobs and workers services over memstore
// with a fake clock; schedulers are made with newScheduler.
type env struct {
	t       *testing.T
	store   *memstore.Store
	clock   *clock.Fake
	jobs    *jobs.Service
	workers *workers.Service
	svc     *schedules.Service
	kinds   *jobkind.Registry
	remotes *fakeRemotes
	repos   *fakeRepos
	owners  *fakeOwners
	signal  *scheduler.Signal
	alice   domain.Actor
	// live are the workers that are online: heartbeat marks them seen.
	live []domain.WorkerID
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		t: t, store: memstore.New(), clock: clock.NewFake(t0),
		remotes: &fakeRemotes{owner: map[domain.RemoteID]domain.UserID{}},
		repos:   &fakeRepos{state: map[domain.ResticRepoID]error{}, remote: map[domain.ResticRepoID]domain.RemoteID{}},
		owners:  &fakeOwners{disabled: map[domain.UserID]bool{}},
		signal:  scheduler.NewSignal(),
		alice:   domain.Actor{UserID: domain.NewUserID(), Role: domain.RoleUser},
	}
	ctx := context.Background()
	u := users.User{ID: e.alice.UserID, Username: "alice", Role: domain.RoleUser, PasswordHash: "x", CreatedAt: t0, UpdatedAt: t0, Version: 1}
	if err := e.store.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	ids := &idgen.Sequential{}
	var err error
	e.jobs, err = jobs.New(jobs.Deps{
		Jobs: e.store, Samples: e.store, Clock: e.clock, IDs: ids, Broker: broker.New(),
		Notifier: jobs.NopNotifier{}, Waker: nopWorkerWaker{}, Remotes: e.remotes, Repositories: e.repos,
	}, jobs.Config{})
	if err != nil {
		t.Fatal(err)
	}
	key := secrets.Key{ID: 1, Bytes: make([]byte, secrets.KeySize)}
	if _, err := crand.Read(key.Bytes); err != nil {
		t.Fatal(err)
	}
	cipher, err := secrets.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	e.workers, err = workers.New(workers.Deps{
		Workers: e.store, Tokens: e.store, Cipher: cipher, Clock: e.clock, IDs: ids, Jobs: e.jobs, Random: crand.Reader,
	}, workers.Config{})
	if err != nil {
		t.Fatal(err)
	}
	e.svc, err = schedules.New(schedules.Deps{
		Repo: e.store, Remotes: e.remotes, Repositories: e.repos, Contacts: okContacts{}, Workers: workerChecker{e.workers},
		Waker: e.signal, Clock: e.clock, IDs: ids,
	}, schedules.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if e.kinds, err = jobkind.NewRegistry(jobkind.Builtin()...); err != nil {
		t.Fatal(err)
	}
	return e
}

// newScheduler returns a scheduler over the env's services; jobs and
// scheds, if not nil, wrap them (fault injection).
func (e *env) newScheduler(jobsPort scheduler.Jobs, scheds scheduler.Schedules) *scheduler.Scheduler {
	e.t.Helper()
	if jobsPort == nil {
		jobsPort = e.jobs
	}
	if scheds == nil {
		scheds = e.svc
	}
	s, err := scheduler.New(scheduler.Deps{
		Schedules: scheds, Jobs: jobsPort, Workers: e.workers, Kinds: e.kinds, Owners: e.owners,
		IDs: idgen.UUIDv7{}, Clock: e.clock, Signal: e.signal,
	}, scheduler.Config{})
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// worker describes a worker to add.
type worker struct {
	name   string
	online bool
	labels []string
	// kinds are the reported job kinds (nil: rclone_transfer; empty
	// non-nil: never reported).
	kinds []string
	// features are the reported link features (nil: a worker older than
	// Milestone 5).
	features []string
	// os is the reported operating system ("": not reported).
	os string
}

func (e *env) addWorker(w worker) domain.WorkerID {
	e.t.Helper()
	kinds := w.kinds
	if kinds == nil {
		kinds = []string{"rclone_transfer"}
	}
	wk := workers.Worker{
		ID: domain.NewWorkerID(), Name: w.name, Mode: domain.WorkerActive, CreatedAt: t0, Labels: w.labels,
		Info: workers.Info{JobKinds: kinds, Features: w.features, OS: w.os},
	}
	if len(kinds) > 0 {
		wk.Info.ReportedAt = t0
	}
	if w.online {
		wk.Link.LastSeen = e.clock.Now()
		e.live = append(e.live, wk.ID)
	} else {
		wk.Link.LastSeen = t0.Add(-24 * time.Hour)
	}
	if err := e.store.CreateWorker(context.Background(), wk); err != nil {
		e.t.Fatal(err)
	}
	return wk.ID
}

// online brings a worker online (it keeps sending heartbeats).
func (e *env) online(id domain.WorkerID) {
	e.t.Helper()
	e.live = append(e.live, id)
	e.heartbeat()
}

// heartbeat marks the live workers seen now, as their polls would.
func (e *env) heartbeat() {
	e.t.Helper()
	for _, id := range e.live {
		if err := e.store.TouchWorker(context.Background(), id, e.clock.Now()); err != nil && !errors.Is(err, domain.ErrNotFound) {
			e.t.Fatal(err)
		}
	}
}

// input is a valid hourly schedule on target.
func (e *env) input(name string, target schedules.Target) schedules.Input {
	return schedules.Input{
		Name: name, Cron: "0 * * * *", Timezone: "UTC", Target: target,
		Missed:  schedules.MissedPolicy{Action: schedules.MissedRunLate, MaxDelay: 30 * time.Minute},
		Overlap: schedules.OverlapSkip, Enabled: true,
		Kind: "rclone_transfer", Definition: []byte(localDefinition), SearchTerms: []string{"copy"},
	}
}

func (e *env) create(in schedules.Input) schedules.Schedule {
	e.t.Helper()
	sc, err := e.svc.Create(context.Background(), e.alice, in)
	if err != nil {
		e.t.Fatal(err)
	}
	return sc
}

func (e *env) tick(s *scheduler.Scheduler) {
	e.t.Helper()
	e.heartbeat()
	if err := s.Tick(context.Background()); err != nil {
		e.t.Fatalf("Tick: %v", err)
	}
}

func (e *env) runs(id domain.ScheduleID) []schedules.Run {
	e.t.Helper()
	rs, err := e.store.ListRuns(context.Background(), schedules.RunFilter{ScheduleID: id, Limit: -1})
	if err != nil {
		e.t.Fatal(err)
	}
	return rs
}

// onlyRun returns the schedule's one run.
func (e *env) onlyRun(id domain.ScheduleID) schedules.Run {
	e.t.Helper()
	rs := e.runs(id)
	if len(rs) != 1 {
		e.t.Fatalf("runs = %+v, want one", rs)
	}
	return rs[0]
}

func (e *env) jobsOf(id domain.ScheduleID) []jobs.Job {
	e.t.Helper()
	js, err := e.store.ListJobs(context.Background(), jobs.ListFilter{ScheduleID: id, Limit: -1})
	if err != nil {
		e.t.Fatal(err)
	}
	return js
}

func (e *env) schedule(id domain.ScheduleID) schedules.Schedule {
	e.t.Helper()
	sc, err := e.store.GetSchedule(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return sc
}

// finish makes a job succeed (the store directly: no worker link here).
func (e *env) finish(j jobs.Job) {
	e.t.Helper()
	j.Status, j.FinishedAt = domain.JobSucceeded, e.clock.Now()
	if _, err := e.store.UpdateJob(context.Background(), j); err != nil {
		e.t.Fatal(err)
	}
}

// busyJob adds an unfinished manual job on a worker.
func (e *env) busyJob(w domain.WorkerID) {
	e.t.Helper()
	if _, err := e.jobs.Create(context.Background(), e.alice, jobs.CreateParams{
		WorkerID: w, Kind: "rclone_transfer", Definition: []byte(localDefinition),
	}); err != nil {
		e.t.Fatal(err)
	}
}

// addRemote makes a live remote of alice.
func (e *env) addRemote(name string) domain.RemoteID {
	e.t.Helper()
	r := remotes.Remote{ID: domain.NewRemoteID(), OwnerID: e.alice.UserID, Name: name, Type: "s3", CreatedAt: t0, UpdatedAt: t0, Version: 1}
	if err := e.store.CreateRemote(context.Background(), r); err != nil {
		e.t.Fatal(err)
	}
	e.remotes.add(r.ID, e.alice.UserID)
	return r.ID
}

// fakeRemotes is the remotes checker of both the jobs and the schedules
// services.
type fakeRemotes struct {
	mu    sync.Mutex
	owner map[domain.RemoteID]domain.UserID
	gone  []domain.RemoteID
}

func (f *fakeRemotes) add(id domain.RemoteID, owner domain.UserID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.owner[id] = owner
}

func (f *fakeRemotes) remove(id domain.RemoteID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gone = append(f.gone, id)
}

func (f *fakeRemotes) Usable(_ context.Context, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[domain.RemoteID]string{}
	for _, id := range ids {
		if o, ok := f.owner[id]; !ok || o != owner || slices.Contains(f.gone, id) {
			return nil, fmt.Errorf("remote %s: %w", id, domain.ErrNotFound)
		}
		out[id] = "remote"
	}
	return out, nil
}

// fakeRepos is the repository checker of both the jobs and the schedules
// services: a repository is usable (nil), gone (an error matching
// ErrNotFound) or deleting (one matching ErrFailedPrecondition).
type fakeRepos struct {
	mu     sync.Mutex
	state  map[domain.ResticRepoID]error
	remote map[domain.ResticRepoID]domain.RemoteID
}

func (f *fakeRepos) set(id domain.ResticRepoID, remote domain.RemoteID, state error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state[id], f.remote[id] = state, remote
}

func (f *fakeRepos) UsableRepository(_ context.Context, _ domain.UserID, id domain.ResticRepoID, _ domain.JobID) (string, domain.RemoteID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state, ok := f.state[id]
	switch {
	case !ok:
		return "", domain.RemoteID{}, fmt.Errorf("repository %s: %w", id, domain.ErrNotFound)
	case state != nil:
		return "", domain.RemoteID{}, state
	}
	return "Photos vault", f.remote[id], nil
}

// fakeOwners is the Owners port (users.Service.Active).
type fakeOwners struct {
	mu       sync.Mutex
	disabled map[domain.UserID]bool
}

func (f *fakeOwners) disable(id domain.UserID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disabled[id] = true
}

func (f *fakeOwners) Active(_ context.Context, id domain.UserID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.disabled[id], nil
}

// okContacts accepts any contact (the tests link none).
type okContacts struct{}

func (okContacts) Usable(context.Context, domain.UserID, []domain.ContactID) (map[domain.ContactID]string, error) {
	return map[domain.ContactID]string{}, nil
}

// workerChecker adapts the workers service, as the composition root does.
type workerChecker struct{ w *workers.Service }

func (c workerChecker) CheckWorker(ctx context.Context, id domain.WorkerID) error {
	_, err := c.w.Get(ctx, id)
	return err
}

type nopWorkerWaker struct{}

func (nopWorkerWaker) Wake(domain.WorkerID) {}

// errInjected is the fault of the fault-injection wrappers.
var errInjected = errors.New("injected failure")

// faultyJobs fails Create (before creating) while failCreate is set.
type faultyJobs struct {
	scheduler.Jobs
	mu         sync.Mutex
	failCreate error
	creates    int
}

func (f *faultyJobs) Create(ctx context.Context, actor domain.Actor, p jobs.CreateParams) (jobs.Job, error) {
	f.mu.Lock()
	f.creates++
	err := f.failCreate
	f.mu.Unlock()
	if err != nil {
		return jobs.Job{}, err
	}
	return f.Jobs.Create(ctx, actor, p)
}

// faultySchedules fails MarkLaunched (after the job exists): a crash
// between creating the job and closing the run.
type faultySchedules struct {
	scheduler.Schedules
}

func (faultySchedules) MarkLaunched(context.Context, schedules.Run) (schedules.Run, error) {
	return schedules.Run{}, errInjected
}

// pausingSchedules runs pause right after a run is marked launching: the
// user pauses the schedule while the scheduler creates the job.
type pausingSchedules struct {
	scheduler.Schedules
	pause func()
}

func (p pausingSchedules) MarkLaunching(ctx context.Context, r schedules.Run, job domain.JobID, w domain.WorkerID) (schedules.Run, error) {
	out, err := p.Schedules.MarkLaunching(ctx, r, job, w)
	if err == nil {
		p.pause()
	}
	return out, err
}

// countingSchedules reports each FireDue (one per pass) without blocking.
type countingSchedules struct {
	scheduler.Schedules
	passes chan struct{}
}

func (c countingSchedules) FireDue(ctx context.Context) (int, error) {
	n, err := c.Schedules.FireDue(ctx)
	select {
	case c.passes <- struct{}{}:
	default:
	}
	return n, err
}

// signallingJobs reports each job it created.
type signallingJobs struct {
	scheduler.Jobs
	created chan domain.JobID
}

func (s signallingJobs) Create(ctx context.Context, actor domain.Actor, p jobs.CreateParams) (jobs.Job, error) {
	j, err := s.Jobs.Create(ctx, actor, p)
	if err == nil {
		s.created <- j.ID
	}
	return j, err
}
