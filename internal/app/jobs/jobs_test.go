package jobs_test

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/broker"
	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/adapters/memstore"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/clock"
)

// recorder implements jobs.Broker (forwarding to memstore), jobs.Notifier
// and jobs.Waker, and remembers every call.
type recorder struct {
	*broker.Broker
	mu      sync.Mutex
	updates []jobs.Update
	// finished are the jobs the notifier was told about; failed are those
	// of them that ended failed or interrupted.
	finished []jobs.Job
	failed   []jobs.Job
	wakes    []domain.WorkerID
}

func (r *recorder) Publish(u jobs.Update) {
	r.mu.Lock()
	r.updates = append(r.updates, u)
	r.mu.Unlock()
	r.Broker.Publish(u)
}

func (r *recorder) JobFinished(j jobs.Job) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished = append(r.finished, j)
	if j.Status == domain.JobFailed || j.Status == domain.JobInterrupted {
		r.failed = append(r.failed, j)
	}
}

func (r *recorder) Wake(id domain.WorkerID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.wakes = append(r.wakes, id)
}

// statuses returns the statuses published for job id in version order
// (concurrent changes may publish out of order), without repeats.
func (r *recorder) statuses(id domain.JobID) []domain.JobStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	var js []*jobs.Job
	for _, u := range r.updates {
		if u.JobID == id && u.Job != nil {
			js = append(js, u.Job)
		}
	}
	slices.SortStableFunc(js, func(a, b *jobs.Job) int { return cmp.Compare(a.Version, b.Version) })
	var out []domain.JobStatus
	for _, j := range js {
		if len(out) == 0 || out[len(out)-1] != j.Status {
			out = append(out, j.Status)
		}
	}
	return out
}

func (r *recorder) progressCount(id domain.JobID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, u := range r.updates {
		if u.JobID == id && u.Progress != nil {
			n++
		}
	}
	return n
}

type env struct {
	svc     *jobs.Service
	store   *memstore.Store
	clock   *clock.Fake
	rec     *recorder
	remotes *fakeRemotes
	worker  domain.WorkerID
	owner   domain.Actor
}

// testDefinition is an encoded job definition (the jobs service does not
// interpret it).
const testDefinition = `{"rclone_transfer":{"mode":"RCLONE_TRANSFER_MODE_COPY"}}`

const epoch = "epoch-1"

func newEnv(t testing.TB) *env {
	t.Helper()
	e := &env{
		store:   memstore.New(),
		clock:   clock.NewFake(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)),
		rec:     &recorder{Broker: broker.New()},
		remotes: &fakeRemotes{},
	}
	e.remotes.store = e.store
	var err error
	e.svc, err = jobs.New(jobs.Deps{
		Jobs: e.store, Samples: e.store, Clock: e.clock, IDs: &idgen.Sequential{},
		Broker: e.rec, Notifier: e.rec, Waker: e.rec, Remotes: e.remotes,
	}, jobs.Config{})
	if err != nil {
		t.Fatal(err)
	}
	e.owner = e.addUser(t, domain.RoleUser)
	e.worker = e.addWorker(t)
	return e
}

// addUser stores a user (jobs need an existing owner) and returns it as
// an actor.
func (e *env) addUser(t testing.TB, role domain.Role) domain.Actor {
	t.Helper()
	id := domain.NewUserID()
	u := users.User{ID: id, Username: "u" + id.String()[24:], Role: role, PasswordHash: "x", Version: 1}
	if err := e.store.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return domain.Actor{UserID: id, Role: role}
}

// addRemote stores a remote of owner (named name) and returns its ID.
func (e *env) addRemote(t *testing.T, owner domain.Actor, name string) domain.RemoteID {
	t.Helper()
	id := domain.NewRemoteID()
	r := remotes.Remote{ID: id, OwnerID: owner.UserID, Name: name, Type: "s3", Version: 1}
	if err := e.store.CreateRemote(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return id
}

// fakeRemotes is the jobs.RemoteChecker of the tests: the store's live
// remotes of the owner are usable (the rule of remotes.Service.Usable);
// err, if set, is returned instead.
type fakeRemotes struct {
	store *memstore.Store
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeRemotes) Usable(ctx context.Context, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]string, error) {
	f.mu.Lock()
	f.calls++
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := make(map[domain.RemoteID]string, len(ids))
	for _, id := range ids {
		r, err := f.store.GetRemote(ctx, id)
		if err != nil || r.OwnerID != owner || r.Deleted() {
			return nil, fmt.Errorf("remote %s: %w", id, domain.ErrNotFound)
		}
		out[id] = r.Name
	}
	return out, nil
}

func (e *env) addWorker(t testing.TB) domain.WorkerID {
	t.Helper()
	id := domain.NewWorkerID()
	// Names are unique among live workers.
	if err := e.store.CreateWorker(context.Background(), workers.Worker{ID: id, Name: "w-" + id.String(), Mode: domain.WorkerActive}); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) create(t testing.TB) jobs.Job {
	t.Helper()
	return e.createAs(t, e.owner, jobs.CreateParams{})
}

// createAs creates a job of actor; p's zero fields get the defaults.
func (e *env) createAs(t testing.TB, actor domain.Actor, p jobs.CreateParams) jobs.Job {
	t.Helper()
	p.WorkerID = cmp.Or(p.WorkerID, e.worker)
	p.Kind = cmp.Or(p.Kind, "rclone_transfer")
	if p.Definition == nil {
		p.Definition = []byte(testDefinition)
	}
	j, err := e.svc.Create(context.Background(), actor, p)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// at moves a new job to status st through the lifecycle API.
func (e *env) at(t testing.TB, st domain.JobStatus) jobs.Job {
	t.Helper()
	ctx := context.Background()
	j := e.create(t)
	steps := map[domain.JobStatus]func() (jobs.Job, bool, error){
		domain.JobDispatched: func() (jobs.Job, bool, error) { return e.svc.MarkDispatched(ctx, e.worker, j.ID, epoch) },
		domain.JobRunning:    func() (jobs.Job, bool, error) { return e.svc.MarkRunning(ctx, e.worker, j.ID, epoch, time.Time{}) },
	}
	var err error
	switch st {
	case domain.JobQueued:
	case domain.JobDispatched, domain.JobRunning:
		j, _, err = steps[st]()
	case domain.JobSucceeded, domain.JobFailed:
		j, _, err = e.svc.Finish(ctx, e.worker, j.ID, epoch, jobs.Outcome{Status: st})
	case domain.JobCancelled:
		j, err = e.svc.Cancel(ctx, e.owner, j.ID)
	case domain.JobInterrupted:
		if _, _, err = e.svc.MarkDispatched(ctx, e.worker, j.ID, epoch); err == nil {
			j, _, err = e.svc.Interrupt(ctx, e.worker, j.ID, epoch, jobs.JobError{Code: jobs.CodeWorkerLost})
		}
	}
	if err != nil || j.Status != st {
		t.Fatalf("moving job to %s: got %s, %v", st, j.Status, err)
	}
	return j
}

func TestCreate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	tests := []struct {
		name  string
		actor domain.Actor
		p     jobs.CreateParams
		want  error
	}{
		{"zero worker", e.owner, jobs.CreateParams{Kind: "k", Definition: []byte("{}")}, domain.ErrInvalidID},
		{"zero actor", domain.Actor{Role: domain.RoleAdmin}, jobs.CreateParams{WorkerID: e.worker, Kind: "k", Definition: []byte("{}")}, domain.ErrInvalidID},
		{"empty kind", e.owner, jobs.CreateParams{WorkerID: e.worker, Definition: []byte("{}")}, domain.ErrInvalidArgument},
		{"bad kind", e.owner, jobs.CreateParams{WorkerID: e.worker, Kind: "Rclone Transfer", Definition: []byte("{}")}, domain.ErrInvalidArgument},
		{"empty definition", e.owner, jobs.CreateParams{WorkerID: e.worker, Kind: "k"}, domain.ErrInvalidArgument},
		{"huge definition", e.owner, jobs.CreateParams{WorkerID: e.worker, Kind: "k", Definition: make([]byte, 1<<20+1)}, domain.ErrInvalidArgument},
		{"zero remote id", e.owner, jobs.CreateParams{WorkerID: e.worker, Kind: "k", Definition: []byte("{}"), RemoteIDs: []domain.RemoteID{{}}}, domain.ErrInvalidID},
		{"unknown worker", e.owner, jobs.CreateParams{WorkerID: domain.NewWorkerID(), Kind: "k", Definition: []byte("{}")}, jobs.ErrUnknownWorker},
		{"unknown owner", domain.Actor{UserID: domain.NewUserID(), Role: domain.RoleUser}, jobs.CreateParams{WorkerID: e.worker, Kind: "k", Definition: []byte("{}")}, domain.ErrNotFound},
	}
	for _, tt := range tests {
		if _, err := e.svc.Create(ctx, tt.actor, tt.p); !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}

	j := e.create(t)
	if j.Status != domain.JobQueued || j.Version != 1 || j.ExitCode != -1 || !j.QueuedAt.Equal(e.clock.Now()) || j.Epoch != "" {
		t.Errorf("created job = %+v", j)
	}
	if j.OwnerID != e.owner.UserID || string(j.Definition) != testDefinition || j.RemoteIDs != nil || j.SearchText != "rclone_transfer" {
		t.Errorf("owner %v, definition %q, remotes %v, search text %q", j.OwnerID, j.Definition, j.RemoteIDs, j.SearchText)
	}
	if e.remotes.calls != 0 {
		t.Errorf("a job without remotes checked %d remotes", e.remotes.calls)
	}
	if !slices.Equal(e.rec.wakes, []domain.WorkerID{e.worker}) {
		t.Errorf("wakes = %v, want the worker woken once", e.rec.wakes)
	}
	if got := e.rec.statuses(j.ID); !slices.Equal(got, []domain.JobStatus{domain.JobQueued}) {
		t.Errorf("published = %v", got)
	}

	// A removed worker takes no new jobs.
	if _, err := e.store.RemoveWorker(ctx, e.worker, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Create(ctx, e.owner, jobs.CreateParams{WorkerID: e.worker, Kind: "k", Definition: []byte("{}")}); !errors.Is(err, jobs.ErrUnknownWorker) {
		t.Errorf("Create on a removed worker: err = %v", err)
	}
}

func TestCancelAtEveryState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		from          domain.JobStatus
		wantStatus    domain.JobStatus
		wantRequested bool
		wantWake      bool
	}{
		{domain.JobQueued, domain.JobCancelled, false, false},
		{domain.JobDispatched, domain.JobDispatched, true, true},
		{domain.JobRunning, domain.JobRunning, true, true},
		{domain.JobSucceeded, domain.JobSucceeded, false, false},
		{domain.JobFailed, domain.JobFailed, false, false},
		{domain.JobCancelled, domain.JobCancelled, false, false},
		{domain.JobInterrupted, domain.JobInterrupted, false, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.from), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			e := newEnv(t)
			j := e.at(t, tt.from)
			wakes := len(e.rec.wakes)
			got, err := e.svc.Cancel(ctx, e.owner, j.ID)
			if err != nil || got.Status != tt.wantStatus || got.CancelRequested() != tt.wantRequested {
				t.Fatalf("Cancel = (%s, requested %v, %v), want (%s, %v)", got.Status, got.CancelRequested(), err, tt.wantStatus, tt.wantRequested)
			}
			if woke := len(e.rec.wakes) > wakes; woke != tt.wantWake {
				t.Errorf("worker woken = %v, want %v", woke, tt.wantWake)
			}
			if tt.from == domain.JobQueued && got.Error.Code != jobs.CodeCancelled {
				t.Errorf("error = %+v", got.Error)
			}
			// A second cancel changes nothing.
			again, err := e.svc.Cancel(ctx, e.owner, j.ID)
			if err != nil || again.Version != got.Version {
				t.Errorf("second Cancel changed the job: v%d -> v%d (%v)", got.Version, again.Version, err)
			}
		})
	}
	t.Run("missing", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		if _, err := e.svc.Cancel(context.Background(), e.owner, domain.NewJobID()); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestCancelRequestThenWorkerOutcome(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		name   string
		from   domain.JobStatus
		then   func(e *env, j jobs.Job) (jobs.Job, error)
		status domain.JobStatus
		code   string
	}{
		{"cancelled result", domain.JobRunning, func(e *env, j jobs.Job) (jobs.Job, error) {
			got, _, err := e.svc.Finish(ctx, e.worker, j.ID, epoch, jobs.Outcome{Status: domain.JobCancelled})
			return got, err
		}, domain.JobCancelled, jobs.CodeCancelled},
		{"cancelled result before it started", domain.JobDispatched, func(e *env, j jobs.Job) (jobs.Job, error) {
			got, _, err := e.svc.Finish(ctx, e.worker, j.ID, epoch, jobs.Outcome{Status: domain.JobCancelled,
				Error: jobs.JobError{Code: jobs.CodeWorkerShutdown, Message: "job cancelled because the worker is shutting down"}})
			return got, err
		}, domain.JobCancelled, jobs.CodeWorkerShutdown},
		{"worker finished first", domain.JobRunning, func(e *env, j jobs.Job) (jobs.Job, error) {
			got, _, err := e.svc.Finish(ctx, e.worker, j.ID, epoch, jobs.Outcome{Status: domain.JobSucceeded})
			return got, err
		}, domain.JobSucceeded, ""},
		{"worker restarted before starting", domain.JobDispatched, func(e *env, j jobs.Job) (jobs.Job, error) {
			got, _, err := e.svc.Requeue(ctx, e.worker, j.ID, epoch)
			return got, err
		}, domain.JobCancelled, jobs.CodeCancelled},
		{"worker restarted while running", domain.JobRunning, func(e *env, j jobs.Job) (jobs.Job, error) {
			got, _, err := e.svc.Interrupt(ctx, e.worker, j.ID, epoch, jobs.JobError{Code: jobs.CodeWorkerRestarted})
			return got, err
		}, domain.JobInterrupted, jobs.CodeWorkerRestarted},
	}
	for _, tt := range tests {
		e := newEnv(t)
		j := e.at(t, tt.from)
		if _, err := e.svc.Cancel(ctx, e.owner, j.ID); err != nil {
			t.Fatal(err)
		}
		got, err := tt.then(e, j)
		if err != nil || got.Status != tt.status || got.Error.Code != tt.code {
			t.Errorf("%s: (%s, %q, %v), want (%s, %q)", tt.name, got.Status, got.Error.Code, err, tt.status, tt.code)
		}
	}
}

func TestLifecycleIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	j := e.create(t)
	other := e.addWorker(t)

	if _, _, err := e.svc.MarkDispatched(ctx, other, j.ID, epoch); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("another worker's ack: err = %v, want ErrNotFound", err)
	}
	got, changed, err := e.svc.MarkDispatched(ctx, e.worker, j.ID, epoch)
	if err != nil || !changed || got.Status != domain.JobDispatched || got.Epoch != epoch || !got.DispatchedAt.Equal(e.clock.Now()) {
		t.Fatalf("MarkDispatched = (%+v, %v, %v)", got, changed, err)
	}
	if _, changed, err := e.svc.MarkDispatched(ctx, e.worker, j.ID, "epoch-2"); changed || err != nil {
		t.Errorf("duplicate ack: changed %v, err %v", changed, err)
	}
	if _, changed, _ := e.svc.MarkRunning(ctx, e.worker, j.ID, "epoch-2", time.Time{}); changed {
		t.Error("JobStarted of another epoch changed the job")
	}
	started := e.clock.Now().Add(-time.Second)
	got, changed, err = e.svc.MarkRunning(ctx, e.worker, j.ID, epoch, started)
	if err != nil || !changed || got.Status != domain.JobRunning || !got.StartedAt.Equal(started) {
		t.Fatalf("MarkRunning = (%+v, %v, %v)", got, changed, err)
	}
	if _, changed, _ := e.svc.MarkRunning(ctx, e.worker, j.ID, epoch, time.Time{}); changed {
		t.Error("duplicate JobStarted changed the job")
	}
	if _, _, err := e.svc.Finish(ctx, e.worker, j.ID, epoch, jobs.Outcome{Status: domain.JobRunning}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Finish(non-terminal) err = %v", err)
	}
	out := jobs.Outcome{Status: domain.JobSucceeded, Stats: jobs.Stats{Bytes: 5 << 30}, Warnings: []string{"w"}, ExitCode: 0, Output: []byte{1}}
	got, changed, err = e.svc.Finish(ctx, e.worker, j.ID, epoch, out)
	if err != nil || !changed || got.Status != domain.JobSucceeded || got.Stats.Bytes != 5<<30 || got.Warnings[0] != "w" || got.FinishedAt.IsZero() {
		t.Fatalf("Finish = (%+v, %v, %v)", got, changed, err)
	}
	if _, changed, err := e.svc.Finish(ctx, e.worker, j.ID, epoch, jobs.Outcome{Status: domain.JobFailed}); changed || err != nil {
		t.Errorf("duplicate result: changed %v, err %v", changed, err)
	}
	if got := e.rec.statuses(j.ID); !slices.Equal(got, []domain.JobStatus{domain.JobQueued, domain.JobDispatched, domain.JobRunning, domain.JobSucceeded}) {
		t.Errorf("published statuses = %v", got)
	}
}

func TestImplicitDispatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	// A JobStarted or JobResult for a queued job proves the worker accepted
	// it: the job passes through dispatched.
	a := e.create(t)
	if got, _, err := e.svc.MarkRunning(ctx, e.worker, a.ID, epoch, time.Time{}); err != nil || got.Status != domain.JobRunning || got.Epoch != epoch {
		t.Errorf("MarkRunning(queued) = (%s, %q, %v)", got.Status, got.Epoch, err)
	}
	b := e.create(t)
	if got, _, err := e.svc.Finish(ctx, e.worker, b.ID, epoch, jobs.Outcome{Status: domain.JobFailed, Error: jobs.JobError{Code: "invalid_spec"}}); err != nil || got.Status != domain.JobFailed {
		t.Errorf("Finish(queued) = (%s, %v)", got.Status, err)
	}
	for _, id := range []domain.JobID{a.ID, b.ID} {
		if st := e.rec.statuses(id); len(st) < 3 || st[1] != domain.JobDispatched {
			t.Errorf("statuses of %s = %v, want queued -> dispatched -> ...", id, st)
		}
	}
}

func TestUnrequestedCancelIsInterrupted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, code := range []string{"", jobs.CodeUnspecified, jobs.CodeWorkerShutdown, jobs.CodeCancelled} {
		e := newEnv(t)
		j := e.at(t, domain.JobRunning)
		got, _, err := e.svc.Finish(ctx, e.worker, j.ID, epoch, jobs.Outcome{
			Status: domain.JobCancelled, Error: jobs.JobError{Code: code, Message: "job cancelled because the worker is shutting down"},
		})
		if err != nil || got.Status != domain.JobInterrupted || got.Error.Code != jobs.CodeWorkerShutdown || !strings.Contains(got.Error.Message, "shutting down") {
			t.Errorf("code %q: (%s, %+v, %v)", code, got.Status, got.Error, err)
		}
		if len(e.rec.failed) != 1 {
			t.Errorf("code %q: notifier calls = %d", code, len(e.rec.failed))
		}
	}
}

// TestUnstartedJobStoppedByTheWorkerIsRequeued: a job the worker gives
// back without having started it (its JobResult is CANCELLED without a
// cancel request, e.g. it was waiting in the queue of a worker that shuts
// down) ran nowhere, so it goes back to the queue instead of ending
// interrupted, and nobody is notified.
func TestUnstartedJobStoppedByTheWorkerIsRequeued(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	shutdown := jobs.Outcome{Status: domain.JobCancelled, ExitCode: -1,
		Error: jobs.JobError{Code: jobs.CodeWorkerShutdown, Message: "job cancelled because the worker is shutting down"}}
	for _, from := range []domain.JobStatus{domain.JobQueued, domain.JobDispatched} {
		e := newEnv(t)
		j := e.at(t, from) // queued: the worker's ack has not arrived yet
		e.clock.Advance(time.Minute)
		wakes := len(e.rec.wakes)
		got, changed, err := e.svc.Finish(ctx, e.worker, j.ID, epoch, shutdown)
		if err != nil || !changed || got.Status != domain.JobQueued || got.Epoch != "" || got.Requeues != 1 ||
			!got.QueuedAt.Equal(e.clock.Now()) || !got.DispatchedAt.IsZero() || !got.FinishedAt.IsZero() || !got.Error.IsZero() {
			t.Errorf("from %s: Finish = (%+v, %v, %v), want re-queued", from, got, changed, err)
		}
		if len(e.rec.finished) != 0 {
			t.Errorf("from %s: notifier called for a re-queued job", from)
		}
		if len(e.rec.wakes) == wakes {
			t.Errorf("from %s: re-queue did not wake the worker", from)
		}
		if smp, ok, _ := e.svc.LatestSample(ctx, j.ID); ok {
			t.Errorf("from %s: a re-queued job got a final sample %+v", from, smp)
		}
		// The worker's next process (another epoch) takes it.
		if got, _, err := e.svc.MarkRunning(ctx, e.worker, j.ID, "epoch-2", time.Time{}); err != nil || got.Status != domain.JobRunning {
			t.Errorf("from %s: MarkRunning after the re-queue = (%s, %v)", from, got.Status, err)
		}
	}

	// A result with a start time comes from a job that ran: interrupted.
	e := newEnv(t)
	j := e.at(t, domain.JobDispatched)
	started := shutdown
	started.StartedAt = e.clock.Now()
	if got, _, err := e.svc.Finish(ctx, e.worker, j.ID, epoch, started); err != nil || got.Status != domain.JobInterrupted || got.Error.Code != jobs.CodeWorkerShutdown {
		t.Errorf("started without JobStarted: (%s, %+v, %v), want interrupted", got.Status, got.Error, err)
	}
	// Results of another epoch change nothing.
	o := e.at(t, domain.JobDispatched)
	if _, changed, err := e.svc.Finish(ctx, e.worker, o.ID, "epoch-2", shutdown); changed || err != nil {
		t.Errorf("another epoch's result: changed %v, err %v", changed, err)
	}
}

// TestAbandonAndCancelUnconfirmed: the jobs of a worker out of contact end
// without the worker's word. A cancel request wins (cancelled); otherwise
// Abandon interrupts, CancelUnconfirmed leaves the job alone. Both apply
// only to active jobs of the given epoch and are idempotent, and the
// worker's late events no longer change the job.
func TestAbandonAndCancelUnconfirmed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lost := jobs.JobError{Code: jobs.CodeWorkerLost, Message: "out of contact"}
	tests := []struct {
		name   string
		from   domain.JobStatus
		cancel bool
		call   func(e *env, j jobs.Job, ep string) (jobs.Job, bool, error)
		status domain.JobStatus
		code   string
	}{
		{"abandon running", domain.JobRunning, false, func(e *env, j jobs.Job, ep string) (jobs.Job, bool, error) {
			return e.svc.Abandon(ctx, e.worker, j.ID, ep, lost)
		}, domain.JobInterrupted, jobs.CodeWorkerLost},
		{"abandon dispatched", domain.JobDispatched, false, func(e *env, j jobs.Job, ep string) (jobs.Job, bool, error) {
			return e.svc.Abandon(ctx, e.worker, j.ID, ep, lost)
		}, domain.JobInterrupted, jobs.CodeWorkerLost},
		{"abandon with a cancel request", domain.JobRunning, true, func(e *env, j jobs.Job, ep string) (jobs.Job, bool, error) {
			return e.svc.Abandon(ctx, e.worker, j.ID, ep, lost)
		}, domain.JobCancelled, jobs.CodeCancelled},
		{"cancel unconfirmed running", domain.JobRunning, true, func(e *env, j jobs.Job, ep string) (jobs.Job, bool, error) {
			return e.svc.CancelUnconfirmed(ctx, e.worker, j.ID, ep)
		}, domain.JobCancelled, jobs.CodeCancelled},
		{"cancel unconfirmed dispatched", domain.JobDispatched, true, func(e *env, j jobs.Job, ep string) (jobs.Job, bool, error) {
			return e.svc.CancelUnconfirmed(ctx, e.worker, j.ID, ep)
		}, domain.JobCancelled, jobs.CodeCancelled},
		{"cancel unconfirmed without a request", domain.JobRunning, false, func(e *env, j jobs.Job, ep string) (jobs.Job, bool, error) {
			return e.svc.CancelUnconfirmed(ctx, e.worker, j.ID, ep)
		}, domain.JobRunning, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			j := e.at(t, tt.from)
			if tt.cancel {
				if _, err := e.svc.Cancel(ctx, e.owner, j.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, changed, err := tt.call(e, j, "epoch-2"); err != nil || changed {
				t.Fatalf("call with the wrong epoch: changed %v, err %v", changed, err)
			}
			e.clock.Advance(time.Minute)
			got, changed, err := tt.call(e, j, epoch)
			if err != nil || got.Status != tt.status || got.Error.Code != tt.code || changed != (tt.status != tt.from) {
				t.Fatalf("got (%s, %+v, changed %v, %v), want (%s, %q)", got.Status, got.Error, changed, err, tt.status, tt.code)
			}
			if !tt.status.Terminal() {
				return
			}
			if !got.FinishedAt.Equal(e.clock.Now()) || got.ExitCode != -1 || got.Error.Message == "" {
				t.Errorf("finished job = %+v", got)
			}
			if len(e.rec.finished) != 1 {
				t.Errorf("notifier calls = %d, want 1", len(e.rec.finished))
			}
			// Idempotent; the worker's late outcome changes nothing.
			if _, changed, err := tt.call(e, j, epoch); changed || err != nil {
				t.Errorf("second call: changed %v, err %v", changed, err)
			}
			late, changed, err := e.svc.Finish(ctx, e.worker, j.ID, epoch, jobs.Outcome{Status: domain.JobSucceeded})
			if changed || err != nil || late.Status != tt.status {
				t.Errorf("late result: (%s, changed %v, %v)", late.Status, changed, err)
			}
			if ok, err := e.svc.RecordProgress(ctx, e.worker, j.ID, epoch, jobs.Sample{Time: e.clock.Now()}); ok || err != nil {
				t.Errorf("late progress accepted: (%v, %v)", ok, err)
			}
		})
	}
	t.Run("another worker", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		j := e.at(t, domain.JobRunning)
		if _, _, err := e.svc.Abandon(ctx, e.addWorker(t), j.ID, epoch, lost); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

func TestNotifier(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// Every terminal status is reported once, after it was stored; the
	// notifier decides what is noteworthy.
	for _, status := range []domain.JobStatus{domain.JobSucceeded, domain.JobFailed, domain.JobCancelled, domain.JobInterrupted} {
		e := newEnv(t)
		j := e.at(t, status)
		if n := len(e.rec.finished); n != 1 {
			t.Errorf("%s: notifier called %d times", status, n)
		} else if got := e.rec.finished[0]; got.ID != j.ID || got.Status != status {
			t.Errorf("%s: notifier got %s %s", status, got.ID, got.Status)
		} else if stored, err := e.store.GetJob(ctx, j.ID); err != nil || stored.Version != got.Version {
			t.Errorf("%s: notifier got version %d before it was stored (%v)", status, got.Version, err)
		}
		// Idempotent changes do not notify again.
		_, _, _ = e.svc.Finish(ctx, e.worker, j.ID, epoch, jobs.Outcome{Status: domain.JobFailed})
		_, _, _ = e.svc.Interrupt(ctx, e.worker, j.ID, epoch, jobs.JobError{})
		if n := len(e.rec.finished); n > 1 {
			t.Errorf("%s: notifier called %d times after duplicates", status, n)
		}
	}
	// Unfinished jobs are never reported.
	e := newEnv(t)
	e.at(t, domain.JobRunning)
	if n := len(e.rec.finished); n != 0 {
		t.Errorf("running job: notifier called %d times", n)
	}
}

func TestRequeue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	j := e.at(t, domain.JobDispatched)
	if _, changed, _ := e.svc.Requeue(ctx, e.worker, j.ID, "other-epoch"); changed {
		t.Error("Requeue with a different epoch changed the job")
	}
	e.clock.Advance(time.Minute)
	wakes := len(e.rec.wakes)
	got, changed, err := e.svc.Requeue(ctx, e.worker, j.ID, epoch)
	if err != nil || !changed || got.Status != domain.JobQueued || got.Epoch != "" || got.Requeues != 1 ||
		!got.QueuedAt.Equal(e.clock.Now()) || !got.DispatchedAt.IsZero() {
		t.Fatalf("Requeue = (%+v, %v, %v)", got, changed, err)
	}
	if len(e.rec.wakes) != wakes+1 {
		t.Error("re-queue did not wake the worker")
	}
	r := e.at(t, domain.JobRunning)
	if _, changed, _ := e.svc.Requeue(ctx, e.worker, r.ID, epoch); changed {
		t.Error("a running job was re-queued")
	}
}

func TestProgressIsRateLimitedBeforeStorage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	j := e.at(t, domain.JobRunning)
	t0 := e.clock.Now()
	for _, sec := range []int{0, 1, 4, 5, 6, 9, 10, 3} {
		ok, err := e.svc.RecordProgress(ctx, e.worker, j.ID, epoch, jobs.Sample{Time: t0.Add(time.Duration(sec) * time.Second), Stats: jobs.Stats{Bytes: int64(sec)}})
		if err != nil || !ok {
			t.Fatalf("RecordProgress(%ds) = (%v, %v)", sec, ok, err)
		}
	}
	stored, _ := e.svc.Samples(ctx, e.owner, j.ID)
	var secs []int
	for _, s := range stored {
		secs = append(secs, int(s.Time.Sub(t0)/time.Second))
	}
	if !slices.Equal(secs, []int{0, 5, 10}) {
		t.Errorf("stored samples at %v s, want [0 5 10]", secs)
	}
	if n := e.rec.progressCount(j.ID); n != 8 {
		t.Errorf("published %d live samples, want 8", n)
	}
	// The final counters become the last sample.
	fin := t0.Add(12 * time.Second)
	if _, _, err := e.svc.Finish(ctx, e.worker, j.ID, epoch, jobs.Outcome{Status: domain.JobSucceeded, FinishedAt: fin, Stats: jobs.Stats{Bytes: 99}}); err != nil {
		t.Fatal(err)
	}
	if last, ok, _ := e.svc.LatestSample(ctx, j.ID); !ok || !last.Time.Equal(fin) || last.Stats.Bytes != 99 {
		t.Errorf("latest sample = %+v", last)
	}
	// Progress of a finished job, of another epoch or of another worker is
	// not accepted.
	if ok, err := e.svc.RecordProgress(ctx, e.worker, j.ID, epoch, jobs.Sample{Time: fin.Add(time.Second)}); ok || err != nil {
		t.Errorf("progress of a finished job = (%v, %v)", ok, err)
	}
	r := e.at(t, domain.JobRunning)
	if ok, _ := e.svc.RecordProgress(ctx, e.worker, r.ID, "other", jobs.Sample{}); ok {
		t.Error("progress of another epoch accepted")
	}
	if _, err := e.svc.RecordProgress(ctx, e.addWorker(t), r.ID, epoch, jobs.Sample{}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("progress of another worker: err = %v", err)
	}
}

// hookedSamples calls before ahead of every AppendJobSample.
type hookedSamples struct {
	jobs.SampleRepository
	before func()
}

func (h hookedSamples) AppendJobSample(ctx context.Context, s jobs.Sample) error {
	if h.before != nil {
		h.before()
	}
	return h.SampleRepository.AppendJobSample(ctx, s)
}

// TestSampleRateLimiterIsBounded: the rate limiter keeps an entry per job
// only while the job reports progress. It drops it when the job finishes
// and, whatever else happened (here a sample stored while the job
// finished, or a job that never ends), once it went unused for ten
// minutes, so it cannot grow over the process lifetime. Dropping an entry
// costs at most one extra stored sample.
func TestSampleRateLimiterIsBounded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	var finishing *jobs.Job
	samples := &hookedSamples{SampleRepository: e.store}
	svc, err := jobs.New(jobs.Deps{
		Jobs: e.store, Samples: samples, Clock: e.clock, IDs: &idgen.Sequential{},
		Broker: e.rec, Notifier: e.rec, Waker: e.rec,
	}, jobs.Config{})
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	sample := func(id domain.JobID) {
		t.Helper()
		if ok, err := svc.RecordProgress(ctx, e.worker, id, epoch, jobs.Sample{Time: e.clock.Now()}); !ok || err != nil {
			t.Fatalf("RecordProgress = (%v, %v)", ok, err)
		}
	}

	// Finishing drops the entry.
	a := e.at(t, domain.JobRunning)
	sample(a.ID)
	if n := jobs.ThrottledJobs(svc); n != 1 {
		t.Fatalf("entries = %d, want 1", n)
	}
	if _, _, err := svc.Finish(ctx, e.worker, a.ID, epoch, jobs.Outcome{Status: domain.JobSucceeded}); err != nil {
		t.Fatal(err)
	}
	if n := jobs.ThrottledJobs(svc); n != 0 {
		t.Errorf("entries after the job finished = %d", n)
	}

	// A sample stored while its job finishes leaves an entry behind; a job
	// that never finishes keeps one.
	b := e.at(t, domain.JobRunning)
	finishing = &b
	samples.before = func() {
		if j := finishing; j != nil {
			finishing = nil
			if _, _, err := svc.Finish(ctx, e.worker, j.ID, epoch, jobs.Outcome{Status: domain.JobFailed}); err != nil {
				t.Error(err)
			}
		}
	}
	sample(b.ID)
	samples.before = nil
	forever := e.at(t, domain.JobRunning)
	sample(forever.ID)
	if n := jobs.ThrottledJobs(svc); n != 2 {
		t.Fatalf("entries = %d, want 2 (a leftover and a job that never finishes)", n)
	}

	// Ten minutes later both are gone; a job still reporting keeps its own.
	c := e.at(t, domain.JobRunning)
	for range 10 {
		e.clock.Advance(time.Minute)
		sample(c.ID)
	}
	if n := jobs.ThrottledJobs(svc); n != 1 {
		t.Errorf("entries after ten minutes = %d, want 1 (the job still reporting)", n)
	}
	// The dropped entry costs at most one extra sample: forever's next
	// sample is stored, the one after it is throttled again.
	before, _ := svc.Samples(ctx, e.owner, forever.ID)
	sample(forever.ID)
	e.clock.Advance(time.Second)
	sample(forever.ID)
	after, _ := svc.Samples(ctx, e.owner, forever.ID)
	if len(after) != len(before)+1 {
		t.Errorf("samples of the job whose entry was dropped: %d -> %d, want one more", len(before), len(after))
	}
}

func TestWatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	j := e.create(t)

	updates := make(chan jobs.Update, 32)
	done := make(chan error, 1)
	go func() {
		done <- e.svc.Watch(ctx, e.owner, j.ID, func(u jobs.Update) error { updates <- u; return nil })
	}()
	next := func() jobs.Update {
		t.Helper()
		select {
		case u := <-updates:
			return u
		case <-time.After(5 * time.Second):
			t.Fatal("no update")
			return jobs.Update{}
		}
	}
	if u := next(); u.Job == nil || u.Job.Status != domain.JobQueued {
		t.Fatalf("first update = %+v", u)
	}
	_, _, _ = e.svc.MarkDispatched(ctx, e.worker, j.ID, epoch)
	_, _, _ = e.svc.MarkRunning(ctx, e.worker, j.ID, epoch, time.Time{})
	// A stale job update (old version) is filtered out.
	old := j
	e.rec.Publish(jobs.Update{JobID: j.ID, Job: &old})
	_, _ = e.svc.RecordProgress(ctx, e.worker, j.ID, epoch, jobs.Sample{Time: e.clock.Now(), Stats: jobs.Stats{Bytes: 42}})
	_, _, _ = e.svc.Finish(ctx, e.worker, j.ID, epoch, jobs.Outcome{Status: domain.JobSucceeded})

	var got []string
	for range 4 {
		u := next()
		if u.Job != nil {
			got = append(got, string(u.Job.Status))
		} else {
			got = append(got, fmt.Sprintf("progress %d", u.Progress.Stats.Bytes))
		}
	}
	if want := []string{"dispatched", "running", "progress 42", "succeeded"}; !slices.Equal(got, want) {
		t.Errorf("updates = %v, want %v", got, want)
	}
	if err := <-done; err != nil {
		t.Errorf("Watch = %v, want nil after the terminal status", err)
	}
	if n := e.rec.Subscribers(j.ID); n != 0 {
		t.Errorf("%d subscriptions left open", n)
	}

	// A finished job yields one update; a running one starts with its
	// latest stored sample.
	calls := 0
	if err := e.svc.Watch(ctx, e.owner, j.ID, func(jobs.Update) error { calls++; return nil }); err != nil || calls != 1 {
		t.Errorf("Watch(finished) = %v after %d calls", err, calls)
	}
	r := e.at(t, domain.JobRunning)
	_, _ = e.svc.RecordProgress(ctx, e.worker, r.ID, epoch, jobs.Sample{Time: e.clock.Now(), Stats: jobs.Stats{Bytes: 7}})
	stop := errors.New("stop")
	var seen []jobs.Update
	err := e.svc.Watch(ctx, e.owner, r.ID, func(u jobs.Update) error {
		seen = append(seen, u)
		if len(seen) == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || seen[1].Progress == nil || seen[1].Progress.Stats.Bytes != 7 {
		t.Errorf("Watch(running) = %v, updates %+v", err, seen)
	}
	if err := e.svc.Watch(ctx, e.owner, domain.NewJobID(), func(jobs.Update) error { return nil }); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Watch(missing) = %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := e.svc.Watch(cctx, e.owner, r.ID, func(jobs.Update) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("Watch(cancelled ctx) = %v", err)
	}
}

func TestList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	var ids []domain.JobID
	for range 5 {
		ids = append(ids, e.create(t).ID)
	}
	slices.Reverse(ids) // newest first
	p1, err := e.svc.List(ctx, e.owner, jobs.ListFilter{Limit: 2})
	if err != nil || len(p1.Jobs) != 2 || p1.Jobs[0].ID != ids[0] || p1.Next != ids[1] {
		t.Fatalf("page 1 = %+v (%v)", p1, err)
	}
	p2, _ := e.svc.List(ctx, e.owner, jobs.ListFilter{Limit: 2, Before: p1.Next})
	p3, _ := e.svc.List(ctx, e.owner, jobs.ListFilter{Limit: 2, Before: p2.Next})
	if len(p2.Jobs) != 2 || p2.Jobs[0].ID != ids[2] || len(p3.Jobs) != 1 || !p3.Next.IsZero() {
		t.Errorf("pages 2/3 = %+v / %+v", p2, p3)
	}
	all, _ := e.svc.List(ctx, e.owner, jobs.ListFilter{})
	if len(all.Jobs) != 5 || !all.Next.IsZero() {
		t.Errorf("default page = %d jobs, next %v", len(all.Jobs), all.Next)
	}
	for _, f := range []jobs.ListFilter{
		{Limit: -1}, {Limit: 501}, {Statuses: []domain.JobStatus{"done"}},
		{Kinds: []string{"Rclone Transfer"}}, {Kinds: slices.Repeat([]string{"k"}, jobs.MaxKinds+1)},
		{Query: strings.Repeat("x", jobs.MaxQueryLen+1)}, {Query: "bad \xff"}, {Query: "nul \x00"},
	} {
		if _, err := e.svc.List(ctx, e.owner, f); !errors.Is(err, domain.ErrInvalidArgument) && !errors.Is(err, domain.ErrInvalidJobStatus) {
			t.Errorf("List(%+v) = %v", f, err)
		}
	}
}

func TestFailQueuedAndRelease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	q := e.create(t)
	e.clock.Advance(time.Minute)
	if _, changed, _ := e.svc.FailQueued(ctx, q.ID, q.QueuedAt, jobs.JobError{Code: jobs.CodeDispatchTimeout}); changed {
		t.Error("FailQueued failed a job queued exactly at the bound")
	}
	got, changed, err := e.svc.FailQueued(ctx, q.ID, e.clock.Now(), jobs.JobError{Code: jobs.CodeDispatchTimeout, Message: "m"})
	if err != nil || !changed || got.Status != domain.JobFailed || got.Error.Code != jobs.CodeDispatchTimeout {
		t.Errorf("FailQueued = (%+v, %v, %v)", got, changed, err)
	}

	queued, dispatched, running, done := e.create(t), e.at(t, domain.JobDispatched), e.at(t, domain.JobRunning), e.at(t, domain.JobSucceeded)
	for range 2 { // idempotent
		if err := e.svc.ReleaseWorkerJobs(ctx, e.worker); err != nil {
			t.Fatal(err)
		}
	}
	want := map[domain.JobID]domain.JobStatus{
		queued.ID: domain.JobFailed, dispatched.ID: domain.JobInterrupted, running.ID: domain.JobInterrupted, done.ID: domain.JobSucceeded,
	}
	for id, st := range want {
		j, _ := e.svc.Get(ctx, e.owner, id)
		if j.Status != st || (id != done.ID && j.Error.Code != jobs.CodeWorkerRemoved) {
			t.Errorf("job %s: %s %q, want %s worker_removed", id, j.Status, j.Error.Code, st)
		}
	}
}

// TestConcurrentChanges races API cancels against the lifecycle calls of
// the link on the same jobs. Every published status sequence must follow
// the state machine, and every job must end terminal.
func TestConcurrentChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	var all []domain.JobID
	for range 30 {
		all = append(all, e.create(t).ID)
	}
	var wg sync.WaitGroup
	for _, id := range all {
		wg.Go(func() { _, _ = e.svc.Cancel(ctx, e.owner, id) })
		wg.Go(func() {
			_, _, _ = e.svc.MarkDispatched(ctx, e.worker, id, epoch)
			_, _, _ = e.svc.MarkRunning(ctx, e.worker, id, epoch, time.Time{})
			_, _ = e.svc.RecordProgress(ctx, e.worker, id, epoch, jobs.Sample{Time: e.clock.Now()})
			_, _, _ = e.svc.Finish(ctx, e.worker, id, epoch, jobs.Outcome{Status: domain.JobCancelled})
		})
		wg.Go(func() { _, _ = e.svc.Get(ctx, e.owner, id) })
	}
	wg.Wait()
	for _, id := range all {
		seq := e.rec.statuses(id)
		for i := 1; i < len(seq); i++ {
			if !seq[i-1].CanTransitionTo(seq[i]) {
				t.Errorf("job %s published %v: %s -> %s is not a transition", id, seq, seq[i-1], seq[i])
			}
		}
		j, _ := e.svc.Get(ctx, e.owner, id)
		if !j.Status.Terminal() {
			t.Errorf("job %s ended %s", id, j.Status)
		}
	}
}
