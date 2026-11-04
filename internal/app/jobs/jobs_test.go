package jobs_test

import (
	"cmp"
	"context"
	"crypto/rand"
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
	"github.com/Zeptiny/Syncr/internal/adapters/secrets"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/clock"
)

// recorder implements jobs.Broker (forwarding to memstore), jobs.Notifier
// and jobs.Waker, and remembers every call.
type recorder struct {
	*broker.Broker
	mu        sync.Mutex
	updates   []jobs.Update
	failed    []jobs.Job
	wakes     []domain.WorkerID
	notifyErr error
}

func (r *recorder) Publish(u jobs.Update) {
	r.mu.Lock()
	r.updates = append(r.updates, u)
	r.mu.Unlock()
	r.Broker.Publish(u)
}

func (r *recorder) JobFailed(_ context.Context, j jobs.Job) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = append(r.failed, j)
	return r.notifyErr
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
	svc    *jobs.Service
	store  *memstore.Store
	clock  *clock.Fake
	rec    *recorder
	worker domain.WorkerID
}

const epoch = "epoch-1"

func newEnv(t *testing.T) *env {
	t.Helper()
	key := secrets.Key{ID: 1, Bytes: make([]byte, secrets.KeySize)}
	_, _ = rand.Read(key.Bytes)
	cipher, err := secrets.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		store: memstore.New(),
		clock: clock.NewFake(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)),
		rec:   &recorder{Broker: broker.New()},
	}
	e.svc, err = jobs.New(jobs.Deps{
		Jobs: e.store, Samples: e.store, Cipher: cipher, Clock: e.clock, IDs: &idgen.Sequential{},
		Broker: e.rec, Notifier: e.rec, Waker: e.rec,
	}, jobs.Config{})
	if err != nil {
		t.Fatal(err)
	}
	e.worker = e.addWorker(t)
	return e
}

func (e *env) addWorker(t *testing.T) domain.WorkerID {
	t.Helper()
	id := domain.NewWorkerID()
	if err := e.store.CreateWorker(context.Background(), workers.Worker{ID: id, Name: "w", Mode: domain.WorkerActive}); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) create(t *testing.T) jobs.Job {
	t.Helper()
	j, err := e.svc.Create(context.Background(), jobs.CreateParams{WorkerID: e.worker, Kind: "rclone_transfer", Spec: []byte("spec with secrets")})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// at moves a new job to status st through the lifecycle API.
func (e *env) at(t *testing.T, st domain.JobStatus) jobs.Job {
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
		j, err = e.svc.Cancel(ctx, j.ID)
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
		name string
		p    jobs.CreateParams
		want error
	}{
		{"zero worker", jobs.CreateParams{Kind: "k", Spec: []byte("s")}, domain.ErrInvalidID},
		{"empty kind", jobs.CreateParams{WorkerID: e.worker, Spec: []byte("s")}, domain.ErrInvalidArgument},
		{"bad kind", jobs.CreateParams{WorkerID: e.worker, Kind: "Rclone Transfer", Spec: []byte("s")}, domain.ErrInvalidArgument},
		{"empty spec", jobs.CreateParams{WorkerID: e.worker, Kind: "k"}, domain.ErrInvalidArgument},
		{"huge spec", jobs.CreateParams{WorkerID: e.worker, Kind: "k", Spec: make([]byte, 1<<20+1)}, domain.ErrInvalidArgument},
		{"unknown worker", jobs.CreateParams{WorkerID: domain.NewWorkerID(), Kind: "k", Spec: []byte("s")}, domain.ErrNotFound},
	}
	for _, tt := range tests {
		if _, err := e.svc.Create(ctx, tt.p); !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}

	j := e.create(t)
	if j.Status != domain.JobQueued || j.Version != 1 || j.ExitCode != -1 || !j.QueuedAt.Equal(e.clock.Now()) || j.Epoch != "" {
		t.Errorf("created job = %+v", j)
	}
	if strings.Contains(string(j.SpecEnc), "secrets") {
		t.Error("the stored spec is not encrypted")
	}
	if spec, err := e.svc.Spec(j); err != nil || string(spec) != "spec with secrets" {
		t.Errorf("Spec = (%q, %v)", spec, err)
	}
	if !slices.Equal(e.rec.wakes, []domain.WorkerID{e.worker}) {
		t.Errorf("wakes = %v, want the worker woken once", e.rec.wakes)
	}
	if got := e.rec.statuses(j.ID); !slices.Equal(got, []domain.JobStatus{domain.JobQueued}) {
		t.Errorf("published = %v", got)
	}
	for _, u := range e.rec.updates {
		if u.Job != nil && u.Job.SpecEnc != nil {
			t.Error("a published job carries its encrypted spec")
		}
	}

	// A removed worker takes no new jobs.
	if _, err := e.store.RemoveWorker(ctx, e.worker, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Create(ctx, jobs.CreateParams{WorkerID: e.worker, Kind: "k", Spec: []byte("s")}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Create on a removed worker: err = %v", err)
	}
}

func TestSpecIsBoundToItsJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a, b := e.create(t), e.create(t)
	b.SpecEnc = a.SpecEnc
	if _, err := e.svc.Spec(b); err == nil {
		t.Error("a spec copied to another job decrypted")
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
			got, err := e.svc.Cancel(ctx, j.ID)
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
			again, err := e.svc.Cancel(ctx, j.ID)
			if err != nil || again.Version != got.Version {
				t.Errorf("second Cancel changed the job: v%d -> v%d (%v)", got.Version, again.Version, err)
			}
		})
	}
	t.Run("missing", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		if _, err := e.svc.Cancel(context.Background(), domain.NewJobID()); !errors.Is(err, domain.ErrNotFound) {
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
		if _, err := e.svc.Cancel(ctx, j.ID); err != nil {
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

func TestNotifier(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		status domain.JobStatus
		notify bool
	}{
		{domain.JobSucceeded, false},
		{domain.JobFailed, true},
		{domain.JobCancelled, false},
		{domain.JobInterrupted, true},
	}
	for _, tt := range tests {
		e := newEnv(t)
		e.rec.notifyErr = errors.New("discord down") // never fails the change
		j := e.at(t, tt.status)
		if n := len(e.rec.failed); n != map[bool]int{true: 1, false: 0}[tt.notify] {
			t.Errorf("%s: notifier called %d times", tt.status, n)
		}
		// Idempotent changes do not notify again.
		_, _, _ = e.svc.Finish(ctx, e.worker, j.ID, epoch, jobs.Outcome{Status: domain.JobFailed})
		_, _, _ = e.svc.Interrupt(ctx, e.worker, j.ID, epoch, jobs.JobError{})
		if n := len(e.rec.failed); n > 1 {
			t.Errorf("%s: notifier called %d times after duplicates", tt.status, n)
		}
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
	stored, _ := e.svc.Samples(ctx, j.ID)
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

func TestWatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	j := e.create(t)

	updates := make(chan jobs.Update, 32)
	done := make(chan error, 1)
	go func() { done <- e.svc.Watch(ctx, j.ID, func(u jobs.Update) error { updates <- u; return nil }) }()
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
	if err := e.svc.Watch(ctx, j.ID, func(jobs.Update) error { calls++; return nil }); err != nil || calls != 1 {
		t.Errorf("Watch(finished) = %v after %d calls", err, calls)
	}
	r := e.at(t, domain.JobRunning)
	_, _ = e.svc.RecordProgress(ctx, e.worker, r.ID, epoch, jobs.Sample{Time: e.clock.Now(), Stats: jobs.Stats{Bytes: 7}})
	stop := errors.New("stop")
	var seen []jobs.Update
	err := e.svc.Watch(ctx, r.ID, func(u jobs.Update) error {
		seen = append(seen, u)
		if len(seen) == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || seen[1].Progress == nil || seen[1].Progress.Stats.Bytes != 7 {
		t.Errorf("Watch(running) = %v, updates %+v", err, seen)
	}
	if err := e.svc.Watch(ctx, domain.NewJobID(), func(jobs.Update) error { return nil }); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Watch(missing) = %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := e.svc.Watch(cctx, r.ID, func(jobs.Update) error { return nil }); !errors.Is(err, context.Canceled) {
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
	p1, err := e.svc.List(ctx, jobs.ListFilter{Limit: 2})
	if err != nil || len(p1.Jobs) != 2 || p1.Jobs[0].ID != ids[0] || p1.Next != ids[1] {
		t.Fatalf("page 1 = %+v (%v)", p1, err)
	}
	p2, _ := e.svc.List(ctx, jobs.ListFilter{Limit: 2, Before: p1.Next})
	p3, _ := e.svc.List(ctx, jobs.ListFilter{Limit: 2, Before: p2.Next})
	if len(p2.Jobs) != 2 || p2.Jobs[0].ID != ids[2] || len(p3.Jobs) != 1 || !p3.Next.IsZero() {
		t.Errorf("pages 2/3 = %+v / %+v", p2, p3)
	}
	all, _ := e.svc.List(ctx, jobs.ListFilter{})
	if len(all.Jobs) != 5 || !all.Next.IsZero() {
		t.Errorf("default page = %d jobs, next %v", len(all.Jobs), all.Next)
	}
	for _, f := range []jobs.ListFilter{{Limit: -1}, {Limit: 501}, {Statuses: []domain.JobStatus{"done"}}} {
		if _, err := e.svc.List(ctx, f); err == nil {
			t.Errorf("List(%+v) succeeded", f)
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
		j, _ := e.svc.Get(ctx, id)
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
		wg.Go(func() { _, _ = e.svc.Cancel(ctx, id) })
		wg.Go(func() {
			_, _, _ = e.svc.MarkDispatched(ctx, e.worker, id, epoch)
			_, _, _ = e.svc.MarkRunning(ctx, e.worker, id, epoch, time.Time{})
			_, _ = e.svc.RecordProgress(ctx, e.worker, id, epoch, jobs.Sample{Time: e.clock.Now()})
			_, _, _ = e.svc.Finish(ctx, e.worker, id, epoch, jobs.Outcome{Status: domain.JobCancelled})
		})
		wg.Go(func() { _, _ = e.svc.Get(ctx, id) })
	}
	wg.Wait()
	for _, id := range all {
		seq := e.rec.statuses(id)
		for i := 1; i < len(seq); i++ {
			if !seq[i-1].CanTransitionTo(seq[i]) {
				t.Errorf("job %s published %v: %s -> %s is not a transition", id, seq, seq[i-1], seq[i])
			}
		}
		j, _ := e.svc.Get(ctx, id)
		if !j.Status.Terminal() {
			t.Errorf("job %s ended %s", id, j.Status)
		}
	}
}
