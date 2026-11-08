package schedules_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/adapters/memstore"
	"github.com/Zeptiny/Syncr/internal/app/contacts"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/clock"
)

// t0 is the tests' start time: a Wednesday, 12:00 UTC.
var t0 = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// testDefinition is a definition as the API stores it (secret-free).
const testDefinition = `{"rclone_transfer":{"mode":"RCLONE_TRANSFER_MODE_COPY","source":{"path":"/data"},"destination":{"path":"/backup"}}}`

// env is a schedules service over memstore with fakes for the other use
// cases.
type env struct {
	t        *testing.T
	store    *memstore.Store
	clock    *clock.Fake
	svc      *schedules.Service
	remotes  *fakeRemotes
	contacts *fakeContacts
	waker    *countingWaker
	alice    domain.Actor
	bob      domain.Actor
	admin    domain.Actor
	worker   domain.WorkerID
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		t:        t,
		store:    memstore.New(),
		clock:    clock.NewFake(t0),
		remotes:  &fakeRemotes{owner: map[domain.RemoteID]domain.UserID{}},
		contacts: &fakeContacts{owner: map[domain.ContactID]domain.UserID{}},
		waker:    &countingWaker{},
		alice:    domain.Actor{UserID: domain.NewUserID(), Role: domain.RoleUser},
		bob:      domain.Actor{UserID: domain.NewUserID(), Role: domain.RoleUser},
		admin:    domain.Actor{UserID: domain.NewUserID(), Role: domain.RoleAdmin},
	}
	for i, a := range []domain.Actor{e.alice, e.bob, e.admin} {
		u := users.User{ID: a.UserID, Username: fmt.Sprintf("user%d", i), Role: a.Role, PasswordHash: "x", CreatedAt: t0, UpdatedAt: t0, Version: 1}
		if err := e.store.CreateUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	e.worker = e.addWorker("nas")
	svc, err := schedules.New(schedules.Deps{
		Repo: e.store, Remotes: e.remotes, Contacts: e.contacts, Workers: storeWorkers{e.store},
		Waker: e.waker, Clock: e.clock, IDs: &idgen.Sequential{},
	}, schedules.Config{})
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	return e
}

func (e *env) addWorker(name string) domain.WorkerID {
	e.t.Helper()
	w := workers.Worker{ID: domain.NewWorkerID(), Name: name, Mode: domain.WorkerActive, CreatedAt: t0}
	if err := e.store.CreateWorker(context.Background(), w); err != nil {
		e.t.Fatal(err)
	}
	return w.ID
}

// addRemote makes a live remote of owner (in the store and the fake
// checker).
func (e *env) addRemote(owner domain.Actor, name string) domain.RemoteID {
	e.t.Helper()
	r := remotes.Remote{ID: domain.NewRemoteID(), OwnerID: owner.UserID, Name: name, Type: "s3", CreatedAt: t0, UpdatedAt: t0, Version: 1}
	if err := e.store.CreateRemote(context.Background(), r); err != nil {
		e.t.Fatal(err)
	}
	e.remotes.add(r.ID, owner.UserID)
	return r.ID
}

// addContact makes a contact of owner (in the store and the fake checker).
func (e *env) addContact(owner domain.Actor, name string) domain.ContactID {
	e.t.Helper()
	c := contacts.Contact{ID: domain.NewContactID(), OwnerID: owner.UserID, Name: name, Type: "webhook", Enabled: true, CreatedAt: t0, UpdatedAt: t0, Version: 1}
	if err := e.store.CreateContact(context.Background(), c); err != nil {
		e.t.Fatal(err)
	}
	e.contacts.add(c.ID, owner.UserID)
	return c.ID
}

// input is a valid schedule input running hourly on the env's worker.
func (e *env) input(name string) schedules.Input {
	return schedules.Input{
		Name: name, Description: "every hour", Cron: "0 * * * *", Timezone: "UTC",
		Target:  schedules.Target{WorkerID: e.worker},
		Missed:  schedules.MissedPolicy{Action: schedules.MissedRunLate, MaxDelay: time.Hour},
		Overlap: schedules.OverlapSkip, Enabled: true,
		Kind: "rclone_transfer", Definition: []byte(testDefinition), SearchTerms: []string{"copy", "/data", "/backup"},
	}
}

func (e *env) create(a domain.Actor, in schedules.Input) schedules.Schedule {
	e.t.Helper()
	sc, err := e.svc.Create(context.Background(), a, in)
	if err != nil {
		e.t.Fatalf("Create(%s): %v", in.Name, err)
	}
	return sc
}

func (e *env) runs(id domain.ScheduleID) []schedules.Run {
	e.t.Helper()
	rs, err := e.store.ListRuns(context.Background(), schedules.RunFilter{ScheduleID: id, Limit: -1})
	if err != nil {
		e.t.Fatal(err)
	}
	return rs
}

func (e *env) schedule(id domain.ScheduleID) schedules.Schedule {
	e.t.Helper()
	sc, err := e.store.GetSchedule(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return sc
}

// addJob stores a job of the schedule in status st (it makes the schedule
// active while unfinished).
func (e *env) addJob(sc schedules.Schedule, st domain.JobStatus) jobs.Job {
	e.t.Helper()
	j := jobs.Job{
		ID: domain.NewJobID(), WorkerID: e.worker, OwnerID: sc.OwnerID, ScheduleID: sc.ID, Kind: "rclone_transfer",
		Status: st, Definition: []byte(testDefinition), CreatedAt: e.clock.Now(), QueuedAt: e.clock.Now(), ExitCode: -1, Version: 1,
	}
	if st.Terminal() {
		j.FinishedAt = e.clock.Now()
	}
	if err := e.store.CreateJob(context.Background(), j); err != nil {
		e.t.Fatal(err)
	}
	return j
}

// wantErr fails unless err matches target.
func wantErr(t *testing.T, what string, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Errorf("%s: error = %v, want %v", what, err, target)
	}
}

// fieldsOf returns the field paths of a *domain.ValidationError.
func fieldsOf(t *testing.T, err error) []string {
	t.Helper()
	var verr *domain.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("error %v is not a *domain.ValidationError", err)
	}
	var out []string
	for _, f := range verr.Fields {
		out = append(out, f.Field)
	}
	return out
}

// fakeRemotes is the RemoteChecker: live remotes of their owner are usable.
type fakeRemotes struct {
	mu    sync.Mutex
	owner map[domain.RemoteID]domain.UserID
}

func (f *fakeRemotes) add(id domain.RemoteID, owner domain.UserID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.owner[id] = owner
}

func (f *fakeRemotes) Usable(_ context.Context, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[domain.RemoteID]string{}
	for _, id := range ids {
		if o, ok := f.owner[id]; !ok || o != owner {
			return nil, fmt.Errorf("remote %s: %w", id, domain.ErrNotFound)
		}
		out[id] = "remote-" + id.String()[30:]
	}
	return out, nil
}

// fakeContacts is the ContactChecker: contacts of their owner are usable.
type fakeContacts struct {
	mu    sync.Mutex
	owner map[domain.ContactID]domain.UserID
}

func (f *fakeContacts) add(id domain.ContactID, owner domain.UserID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.owner[id] = owner
}

func (f *fakeContacts) Usable(_ context.Context, owner domain.UserID, ids []domain.ContactID) (map[domain.ContactID]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[domain.ContactID]string{}
	for _, id := range ids {
		if o, ok := f.owner[id]; !ok || o != owner {
			return nil, fmt.Errorf("contact %s: %w", id, domain.ErrNotFound)
		}
		out[id] = "contact"
	}
	return out, nil
}

// storeWorkers is the WorkerChecker over the store, as the composition
// root adapts the workers service: a removed worker is not found.
type storeWorkers struct{ s *memstore.Store }

func (w storeWorkers) CheckWorker(ctx context.Context, id domain.WorkerID) error {
	wk, err := w.s.GetWorker(ctx, id)
	if err != nil {
		return err
	}
	if wk.Removed() {
		return fmt.Errorf("worker %s: %w", id, domain.ErrNotFound)
	}
	return nil
}

// countingWaker counts the wake-ups.
type countingWaker struct{ n atomic.Int64 }

func (w *countingWaker) Wake() { w.n.Add(1) }
