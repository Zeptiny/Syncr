package notifications_test

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/adapters/memstore"
	"github.com/Zeptiny/Syncr/internal/adapters/secrets"
	"github.com/Zeptiny/Syncr/internal/app/contacts"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/notifications"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/descriptor"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/notify"
	"github.com/Zeptiny/Syncr/internal/platform/clock"
)

var t0 = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

const secretURL = "https://hooks.example/secret-hook-token-0001"

// fakeChannel records sends and answers with outcome (nil: success).
type fakeChannel struct {
	mu      sync.Mutex
	sends   []notify.Message
	cfgs    []descriptor.Values
	outcome func(n int, msg notify.Message) error
	// sent, if set, receives every message after it was recorded.
	sent chan notify.Message
	// block makes Send wait for its context.
	block bool
}

func (*fakeChannel) ID() string          { return "fake" }
func (*fakeChannel) Label() string       { return "Fake" }
func (*fakeChannel) Description() string { return "A channel type of the tests." }
func (*fakeChannel) Fields() []descriptor.Field {
	return []descriptor.Field{
		{Name: "url", Label: "URL", Kind: descriptor.KindString, Required: true, Secret: true},
		{Name: "room", Label: "Room", Kind: descriptor.KindString},
	}
}

func (c *fakeChannel) Validate(cfg descriptor.Values) error {
	var v domain.Validation
	v.AddAll("", descriptor.CheckFields(c.Fields(), cfg))
	return v.Err()
}

func (*fakeChannel) Summary(descriptor.Values) string { return "fake target" }

func (c *fakeChannel) Send(ctx context.Context, cfg descriptor.Values, msg notify.Message) error {
	c.mu.Lock()
	n := len(c.sends)
	c.sends = append(c.sends, msg)
	c.cfgs = append(c.cfgs, cfg.Clone())
	outcome, sent, block := c.outcome, c.sent, c.block
	c.mu.Unlock()
	if sent != nil {
		sent <- msg
	}
	if block {
		<-ctx.Done()
		return notify.Temporary(0, "the send was cancelled", 0)
	}
	if outcome != nil {
		return outcome(n, msg)
	}
	return nil
}

func (c *fakeChannel) messages() []notify.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.sends)
}

func (c *fakeChannel) setOutcome(f func(n int, msg notify.Message) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.outcome = f
}

// zeroReader is a Random that always draws 0 (the shortest jitter).
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

type allowAll struct{}

func (allowAll) Allow(string) bool { return true }

// resolver wraps the contacts service's ResolveChannel, optionally failing.
type resolver struct {
	inner *contacts.Service
	mu    sync.Mutex
	err   error
}

func (r *resolver) ResolveChannel(ctx context.Context, id domain.ContactID) (string, descriptor.Values, error) {
	r.mu.Lock()
	err := r.err
	r.mu.Unlock()
	if err != nil {
		return "", descriptor.Values{}, err
	}
	return r.inner.ResolveChannel(ctx, id)
}

type env struct {
	t        *testing.T
	svc      *notifications.Service
	contacts *contacts.Service
	store    *memstore.Store
	repo     notifications.Repository
	clock    *clock.Fake
	ids      *idgen.Sequential
	ch       *fakeChannel
	resolver *resolver
	logs     *bytes.Buffer
	alice    domain.Actor
	bob      domain.Actor
	admin    domain.Actor
	worker   workers.Worker
	jobs     int
}

type option func(*env, *notifications.Deps, *notifications.Config)

func withConfig(f func(*notifications.Config)) option {
	return func(_ *env, _ *notifications.Deps, c *notifications.Config) { f(c) }
}

func withRepo(wrap func(*memstore.Store) notifications.Repository) option {
	return func(e *env, d *notifications.Deps, _ *notifications.Config) { d.Repo = wrap(e.store) }
}

func newEnv(t *testing.T, opts ...option) *env {
	t.Helper()
	key := secrets.Key{ID: 1, Bytes: make([]byte, secrets.KeySize)}
	if _, err := crand.Read(key.Bytes); err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, store: memstore.New(), clock: clock.NewFake(t0), ids: &idgen.Sequential{}, ch: &fakeChannel{}, logs: &bytes.Buffer{}}
	reg, err := notify.NewRegistry(e.ch)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i, a := range []*domain.Actor{&e.alice, &e.bob, &e.admin} {
		role := domain.RoleUser
		if i == 2 {
			role = domain.RoleAdmin
		}
		*a = domain.Actor{UserID: e.ids.NewUserID(), Role: role}
		u := users.User{ID: a.UserID, Username: []string{"alice", "bob", "admin"}[i], Role: role, PasswordHash: "x",
			CreatedAt: t0, UpdatedAt: t0, PasswordChangedAt: t0, Version: 1}
		if err := e.store.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	e.worker = workers.Worker{ID: e.ids.NewWorkerID(), Name: "nas-01", Mode: domain.WorkerActive, CreatedAt: t0}
	if err := e.store.CreateWorker(ctx, e.worker); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.contacts, err = contacts.New(contacts.Deps{
		Contacts: e.store, Channels: reg, Cipher: ring, Limiter: allowAll{}, Clock: e.clock, IDs: e.ids, Logger: logger,
	}, contacts.Config{})
	if err != nil {
		t.Fatal(err)
	}
	e.resolver = &resolver{inner: e.contacts}
	deps := notifications.Deps{
		Repo: e.store, Channels: reg, Contacts: e.resolver, Clock: e.clock, IDs: e.ids, Random: zeroReader{},
		PublicURL: "https://syncr.example/", Logger: logger,
	}
	cfg := notifications.Config{SendTimeout: time.Second}
	for _, o := range opts {
		o(e, &deps, &cfg)
	}
	e.repo = deps.Repo
	e.svc, err = notifications.New(deps, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// contact creates a contact of a.
func (e *env) contact(a domain.Actor, name string, enabled bool, events ...domain.NotificationEvent) contacts.Contact {
	e.t.Helper()
	c, err := e.contacts.Create(context.Background(), a, contacts.Input{
		Name: name, Type: "fake", Enabled: enabled, JobEvents: events,
		Options: map[string]string{"room": name}, Secrets: map[string]string{"url": secretURL},
	})
	if err != nil {
		e.t.Fatalf("create contact %s: %v", name, err)
	}
	return c
}

func link(c domain.ContactID, events ...domain.NotificationEvent) schedules.ContactLink {
	return schedules.ContactLink{ContactID: c, Events: domain.EventSet(events).Normalize()}
}

// schedule creates a schedule of a with links.
func (e *env) schedule(a domain.Actor, name string, links ...schedules.ContactLink) schedules.Schedule {
	e.t.Helper()
	sc := schedules.Schedule{
		ID: e.ids.NewScheduleID(), OwnerID: a.UserID, Name: name, Cron: "30 2 * * *", Timezone: "Europe/Berlin",
		Kind: "rclone_transfer", Definition: []byte(`{}`), Target: schedules.Target{WorkerID: e.worker.ID},
		Missed: schedules.MissedPolicy{Action: schedules.MissedRunLate}, Overlap: schedules.OverlapSkip,
		Contacts: links, CreatedAt: t0, UpdatedAt: t0, Version: 1,
	}
	if err := e.store.CreateSchedule(context.Background(), sc); err != nil {
		e.t.Fatal(err)
	}
	return sc
}

// job stores a job of a that finished with status (scheduled by schedule,
// if not zero).
func (e *env) job(a domain.Actor, status domain.JobStatus, schedule domain.ScheduleID) jobs.Job {
	e.t.Helper()
	ctx := context.Background()
	e.jobs++
	j := jobs.Job{
		ID: e.ids.NewJobID(), WorkerID: e.worker.ID, OwnerID: a.UserID, Kind: "rclone_transfer", Status: domain.JobQueued,
		Definition: []byte(`{}`), ScheduleID: schedule, CreatedAt: t0, QueuedAt: t0, ExitCode: -1, Version: 1,
	}
	if err := e.store.CreateJob(ctx, j); err != nil {
		e.t.Fatal(err)
	}
	finished := t0.Add(time.Duration(e.jobs) * time.Minute)
	j.Status, j.StartedAt, j.FinishedAt = status, finished.Add(-90*time.Second), finished
	j.Stats = jobs.Stats{Bytes: 1 << 20, Files: 12, Errors: 1}
	if status != domain.JobSucceeded {
		j.Error = jobs.JobError{Code: "path_not_allowed", Message: "outside local_roots"}
	}
	stored, err := e.store.UpdateJob(ctx, j)
	if err != nil {
		e.t.Fatal(err)
	}
	return stored
}

func (e *env) setResolveErr(err error) {
	e.resolver.mu.Lock()
	defer e.resolver.mu.Unlock()
	e.resolver.err = err
}

func (e *env) plan() int {
	e.t.Helper()
	n, err := e.svc.Plan(context.Background())
	if err != nil {
		e.t.Fatalf("Plan: %v", err)
	}
	return n
}

func (e *env) deliver() int {
	e.t.Helper()
	n, err := e.svc.DeliverDue(context.Background())
	if err != nil {
		e.t.Fatalf("DeliverDue: %v", err)
	}
	return n
}

func (e *env) deliveries() []notifications.Delivery {
	e.t.Helper()
	ds, err := e.store.ListDeliveries(context.Background(), notifications.DeliveryFilter{Limit: -1})
	if err != nil {
		e.t.Fatal(err)
	}
	return ds
}

func (e *env) only() notifications.Delivery {
	e.t.Helper()
	ds := e.deliveries()
	if len(ds) != 1 {
		e.t.Fatalf("deliveries = %d, want 1: %+v", len(ds), ds)
	}
	return ds[0]
}

func payload(t *testing.T, d notifications.Delivery) notify.Message {
	t.Helper()
	var m notify.Message
	if err := json.Unmarshal(d.Payload, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func contactsOf(ds []notifications.Delivery) []domain.ContactID {
	out := make([]domain.ContactID, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.ContactID)
	}
	slices.SortFunc(out, func(a, b domain.ContactID) int { return strings.Compare(a.String(), b.String()) })
	return out
}

func (e *env) pendingJobs() int {
	e.t.Helper()
	os, err := e.store.PendingJobOutcomes(context.Background(), -1)
	if err != nil {
		e.t.Fatal(err)
	}
	return len(os)
}

func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: err = %v, want %v", what, err, want)
	}
}

func TestNewConfig(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	base := notifications.Deps{Repo: e.store, Channels: notifyRegistry(t), Contacts: e.resolver, Clock: e.clock, IDs: e.ids}
	if _, err := notifications.New(notifications.Deps{}, notifications.Config{}); err == nil {
		t.Error("New accepted missing dependencies")
	}
	if _, err := notifications.New(base, notifications.Config{}); err != nil {
		t.Errorf("New(defaults): %v", err)
	}
	for name, cfg := range map[string]notifications.Config{
		"negative attempts":    {MaxAttempts: -1},
		"negative concurrency": {Concurrency: -1},
		"lease below timeout":  {Lease: time.Second, SendTimeout: 2 * time.Second},
		"negative tick":        {Tick: -time.Second},
	} {
		if _, err := notifications.New(base, cfg); err == nil {
			t.Errorf("New(%s) accepted %+v", name, cfg)
		}
	}
}

func notifyRegistry(t *testing.T) *notify.Registry {
	t.Helper()
	reg, err := notify.NewRegistry(&fakeChannel{})
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestPlanManualJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	on := e.contact(e.alice, "on", true, domain.EventFailed)
	e.contact(e.alice, "off", false, domain.EventFailed)
	e.contact(e.alice, "quiet", true) // no job events: manual jobs never notify it
	e.contact(e.bob, "theirs", true, domain.EventFailed)
	j := e.job(e.alice, domain.JobFailed, domain.ScheduleID{})

	if n := e.plan(); n != 1 {
		t.Fatalf("Plan = %d, want 1", n)
	}
	d := e.only()
	if d.ContactID != on.ID || d.OwnerID != e.alice.UserID || d.Event != domain.EventFailed || d.JobID != j.ID ||
		!d.ScheduleID.IsZero() || d.DedupeKey != notifications.JobDedupeKey(j.ID, domain.EventFailed) ||
		d.Status != notifications.StatusPending || d.Attempts != 0 || d.MaxAttempts != 8 ||
		!d.NextAttemptAt.Equal(t0) || !d.CreatedAt.Equal(t0) {
		t.Errorf("delivery = %+v", d)
	}
	m := payload(t, d)
	if m.Version != notify.MessageVersion || m.ID != "" || m.Event != domain.EventFailed || !m.OccurredAt.Equal(j.FinishedAt) ||
		m.Title != "Job failed: rclone_transfer" || m.URL != "https://syncr.example/jobs/"+j.ID.String() || m.Schedule != nil || m.Run != nil ||
		m.Job == nil || m.Job.ID != j.ID.String() || m.Job.WorkerName != "nas-01" || m.Job.ErrorCode != "path_not_allowed" ||
		m.Job.Bytes != 1<<20 || m.Job.Files != 12 || m.Job.Errors != 1 || m.Job.DurationSeconds != 90 || m.Job.Status != domain.JobFailed {
		t.Errorf("message = %+v (job %+v)", m, m.Job)
	}
	if strings.Contains(string(d.Payload), "secret-hook-token") {
		t.Error("the payload contains a secret")
	}

	// Planned once: the job is marked.
	if n := e.plan(); n != 0 || e.pendingJobs() != 0 {
		t.Errorf("Plan again = %d, pending %d", n, e.pendingJobs())
	}
	// A succeeded job notifies nobody subscribed to failures only, and is
	// marked all the same.
	e.job(e.alice, domain.JobSucceeded, domain.ScheduleID{})
	if n := e.plan(); n != 0 || e.pendingJobs() != 0 || len(e.deliveries()) != 1 {
		t.Errorf("Plan(succeeded) = %d, pending %d", n, e.pendingJobs())
	}
}

// describer is a JobDescriber that describes every job as desc (or
// fails with err) and records its calls.
type describer struct {
	mu    sync.Mutex
	desc  string
	err   error
	calls []string
}

func (d *describer) DescribeJob(_ context.Context, owner domain.UserID, kind string, definition []byte) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, owner.String()+" "+kind+" "+string(definition))
	return d.desc, d.err
}

func (d *describer) set(desc string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.desc, d.err = desc, err
}

func (d *describer) callList() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.calls)
}

func withDescriber(d notifications.JobDescriber) option {
	return func(_ *env, deps *notifications.Deps, _ *notifications.Config) { deps.Jobs = d }
}

// TestPlanDescribesJobs: a job without a schedule is titled by its
// description (not its kind), a scheduled one keeps its schedule's name;
// both carry the description. A describer error leaves the outcome to the
// next pass; jobs without deliveries are not described.
func TestPlanDescribesJobs(t *testing.T) {
	t.Parallel()
	desc := &describer{desc: "Copy Photos → B2 offsite"}
	e := newEnv(t, withDescriber(desc))
	on := e.contact(e.alice, "on", true, domain.EventFailed)
	sc := e.schedule(e.alice, "Nightly photos", link(on.ID, domain.EventFailed))

	e.job(e.alice, domain.JobSucceeded, domain.ScheduleID{}) // nobody subscribed: not described
	manual := e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	if n := e.plan(); n != 1 {
		t.Fatalf("Plan = %d, want 1", n)
	}
	m := payload(t, e.only())
	if m.Title != "Job failed: Copy Photos → B2 offsite" || m.Job == nil || m.Job.Description != "Copy Photos → B2 offsite" ||
		m.Job.Kind != "rclone_transfer" || m.Job.ID != manual.ID.String() {
		t.Errorf("message = %+v (job %+v)", m, m.Job)
	}
	if facts := notify.Facts(m); len(facts) == 0 || facts[0] != (notify.Fact{Label: "Job", Value: "Copy Photos → B2 offsite"}) {
		t.Errorf("facts = %+v, want the description first", facts)
	}
	if calls := desc.callList(); !slices.Equal(calls, []string{e.alice.UserID.String() + " rclone_transfer {}"}) {
		t.Errorf("DescribeJob calls = %q", calls)
	}

	desc.set("", errors.New("store unavailable"))
	scheduled := e.job(e.alice, domain.JobFailed, sc.ID)
	if n, err := e.svc.Plan(context.Background()); n != 0 || err == nil || e.pendingJobs() != 1 {
		t.Fatalf("Plan with a failing describer = (%d, %v), pending %d; want an error and the job left pending", n, err, e.pendingJobs())
	}
	desc.set("Sync /srv/docs → B2", nil)
	if n := e.plan(); n != 1 || e.pendingJobs() != 0 {
		t.Fatalf("Plan again = %d, pending %d", n, e.pendingJobs())
	}
	m = payload(t, e.deliveries()[0])
	if m.Job == nil || m.Job.ID != scheduled.ID.String() || m.Title != "Job failed: Nightly photos" || m.Job.Description != "Sync /srv/docs → B2" {
		t.Errorf("scheduled message = %+v (job %+v)", m, m.Job)
	}
}

func TestPlanScheduledJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	on := e.contact(e.alice, "on", true, domain.EventFailed, domain.EventSucceeded)
	quiet := e.contact(e.alice, "quiet", true)
	off := e.contact(e.alice, "off", false)
	sc := e.schedule(e.alice, "Nightly photos",
		link(quiet.ID, domain.EventSucceeded, domain.EventFailed),
		link(off.ID, domain.EventSucceeded),
		link(on.ID, domain.EventMissed))

	ok := e.job(e.alice, domain.JobSucceeded, sc.ID)
	if n := e.plan(); n != 1 {
		t.Fatalf("Plan = %d, want 1", n)
	}
	d := e.only()
	// The linked, enabled contact only: never through job events (on
	// subscribes to succeeded manual jobs, but its link does not).
	if d.ContactID != quiet.ID || d.Event != domain.EventSucceeded || d.ScheduleID != sc.ID || d.JobID != ok.ID {
		t.Errorf("delivery = %+v", d)
	}
	m := payload(t, d)
	if m.Schedule == nil || m.Schedule.ID != sc.ID.String() || m.Schedule.Name != "Nightly photos" || m.Title != "Job succeeded: Nightly photos" {
		t.Errorf("message = %+v", m)
	}

	e.job(e.alice, domain.JobInterrupted, sc.ID) // nobody links interrupted
	failed := e.job(e.alice, domain.JobFailed, sc.ID)
	if n := e.plan(); n != 1 {
		t.Fatalf("Plan = %d, want 1", n)
	}
	ds := e.deliveries()
	if len(ds) != 2 || ds[0].JobID != failed.ID || ds[0].ContactID != quiet.ID {
		t.Errorf("deliveries = %+v", ds)
	}
}

func TestCancelledJobsNeverNotify(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	all := e.contact(e.alice, "all", true, domain.JobEvents()...)
	sc := e.schedule(e.alice, "Nightly", link(all.ID, domain.ScheduleEvents()...))
	e.job(e.alice, domain.JobCancelled, domain.ScheduleID{})
	e.job(e.alice, domain.JobCancelled, sc.ID)
	if n := e.plan(); n != 0 || len(e.deliveries()) != 0 {
		t.Errorf("Plan(cancelled) = %d, deliveries %+v", n, e.deliveries())
	}
	if e.pendingJobs() != 0 {
		t.Error("cancelled jobs are still pending")
	}
}

func TestPlanRuns(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	c := e.contact(e.alice, "ops", true)
	other := e.contact(e.alice, "other", true)
	sc := e.schedule(e.alice, "Office docs", link(c.ID, domain.EventMissed, domain.EventFailed), link(other.ID, domain.EventSucceeded))
	run := func(status schedules.RunStatus, reason string) schedules.Run {
		t.Helper()
		e.clock.Advance(time.Second)
		r := schedules.Run{
			ID: e.ids.NewScheduleRunID(), ScheduleID: sc.ID, Trigger: schedules.TriggerCron,
			ScheduledFor: t0, Deadline: t0.Add(time.Hour), Coalesced: 2, Status: status, Reason: reason,
			Message: "nas-01 was offline", WorkerID: e.worker.ID, CreatedAt: t0, UpdatedAt: e.clock.Now(),
		}
		got, err := e.store.AddRun(ctx, r, false)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	missed := run(schedules.RunMissed, schedules.ReasonWorkerOffline)
	failed := run(schedules.RunFailed, schedules.ReasonWorkerRemoved)
	run(schedules.RunFailed, schedules.ReasonOwnerDisabled) // never notifies
	run(schedules.RunCancelled, schedules.ReasonDisabled)   // never pending

	if n := e.plan(); n != 2 {
		t.Fatalf("Plan = %d, want 2", n)
	}
	ds := e.deliveries()
	if len(ds) != 2 {
		t.Fatalf("deliveries = %+v", ds)
	}
	byKey := map[string]notifications.Delivery{}
	for _, d := range ds {
		byKey[d.DedupeKey] = d
	}
	d, ok := byKey[notifications.RunDedupeKey(missed.ID, domain.EventMissed)]
	if !ok || d.ContactID != c.ID || d.Event != domain.EventMissed || d.ScheduleID != sc.ID || !d.JobID.IsZero() || d.OwnerID != e.alice.UserID {
		t.Errorf("missed run delivery = %+v", d)
	}
	m := payload(t, d)
	if m.Run == nil || m.Run.ID != missed.ID.String() || m.Run.Reason != schedules.ReasonWorkerOffline || m.Run.Coalesced != 2 ||
		!m.Run.ScheduledFor.Equal(t0) || m.Run.Message != "nas-01 was offline" || m.Job != nil ||
		m.URL != "https://syncr.example/schedules/"+sc.ID.String() || m.Title != "Scheduled run missed: Office docs" {
		t.Errorf("missed run message = %+v (run %+v)", m, m.Run)
	}
	if d, ok := byKey[notifications.RunDedupeKey(failed.ID, domain.EventFailed)]; !ok || d.Event != domain.EventFailed || payload(t, d).Title != "Scheduled run failed to start: Office docs" {
		t.Errorf("failed run delivery = %+v", d)
	}
	if os, _ := e.store.PendingRunOutcomes(ctx, -1); len(os) != 0 {
		t.Errorf("runs still pending: %+v", os)
	}
}

func TestPlanExactlyOnceUnderConcurrency(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c1, c2 := e.contact(e.alice, "one", true, domain.EventFailed), e.contact(e.alice, "two", true, domain.EventFailed)
	const jobsN = 150 // more than one batch
	for range jobsN {
		e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for range 4 {
		wg.Go(func() {
			n, err := e.svc.Plan(context.Background())
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			total += n
			mu.Unlock()
		})
	}
	wg.Wait()
	ds := e.deliveries()
	if total != 2*jobsN || len(ds) != 2*jobsN {
		t.Fatalf("concurrent planners planned %d (stored %d), want %d", total, len(ds), 2*jobsN)
	}
	keys := map[string]bool{}
	for _, d := range ds {
		k := d.ContactID.String() + d.DedupeKey
		if keys[k] {
			t.Errorf("delivery planned twice: %s", k)
		}
		keys[k] = true
	}
	perContact := map[domain.ContactID]int{}
	for _, c := range contactsOf(ds) {
		perContact[c]++
	}
	if perContact[c1.ID] != jobsN || perContact[c2.ID] != jobsN {
		t.Errorf("deliveries per contact = %v", perContact)
	}
}

// crashingRepo makes the first CommitJobOutcome store the deliveries but
// not the job's mark (as if the planner died between the two), or, with
// afterCommit, commit everything and then fail.
type crashingRepo struct {
	*memstore.Store
	carrier     domain.JobID
	afterCommit bool
	mu          sync.Mutex
	// armed makes the next commit crash.
	armed bool
}

func (r *crashingRepo) CommitJobOutcome(ctx context.Context, id domain.JobID, ds []notifications.Delivery, at time.Time) (int, error) {
	r.mu.Lock()
	crash := r.armed
	r.armed = false
	r.mu.Unlock()
	switch {
	case !crash:
		return r.Store.CommitJobOutcome(ctx, id, ds, at)
	case r.afterCommit:
		if _, err := r.Store.CommitJobOutcome(ctx, id, ds, at); err != nil {
			return 0, err
		}
	default:
		// The deliveries are inserted, the job is not marked: commit them
		// through another job.
		if _, err := r.Store.CommitJobOutcome(ctx, r.carrier, ds, at); err != nil {
			return 0, err
		}
	}
	return 0, errors.New("planner crashed")
}

func TestPlanCrashReplansOnce(t *testing.T) {
	t.Parallel()
	for _, afterCommit := range []bool{false, true} {
		t.Run(fmt.Sprintf("after commit %v", afterCommit), func(t *testing.T) {
			t.Parallel()
			repo := &crashingRepo{afterCommit: afterCommit}
			e := newEnv(t, withRepo(func(s *memstore.Store) notifications.Repository { repo.Store = s; return repo }))
			c := e.contact(e.alice, "ops", true, domain.EventFailed, domain.EventSucceeded)
			carrier := e.job(e.alice, domain.JobCancelled, domain.ScheduleID{})
			repo.carrier = carrier.ID
			if _, err := e.svc.Plan(context.Background()); err != nil {
				t.Fatal(err) // marks the carrier
			}
			j := e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
			repo.mu.Lock()
			repo.armed = true
			repo.mu.Unlock()
			if _, err := e.svc.Plan(context.Background()); err == nil {
				t.Fatal("Plan did not report the crash")
			}
			wantPending := 1
			if afterCommit {
				wantPending = 0
			}
			if got := e.pendingJobs(); got != wantPending {
				t.Fatalf("pending after the crash = %d, want %d", got, wantPending)
			}
			// The restarted planner plans the job again: nothing new.
			if n := e.plan(); n != 0 {
				t.Errorf("re-plan inserted %d deliveries", n)
			}
			d := e.only()
			if d.ContactID != c.ID || d.JobID != j.ID || e.pendingJobs() != 0 {
				t.Errorf("after the re-plan: %+v, pending %d", d, e.pendingJobs())
			}
		})
	}
}

func TestDeliverSuccess(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.contact(e.alice, "ops", true, domain.EventFailed)
	e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	e.plan()
	planned := e.only()
	e.clock.Advance(time.Second)
	if n := e.deliver(); n != 1 {
		t.Fatalf("DeliverDue = %d", n)
	}
	sent := e.ch.messages()
	if len(sent) != 1 || sent[0].ID != planned.ID.String() || sent[0].Event != domain.EventFailed {
		t.Fatalf("sent = %+v", sent)
	}
	if cfg := e.ch.cfgs[0]; cfg.Secrets["url"] != secretURL || cfg.Options["room"] != "ops" {
		t.Errorf("sent with %+v", cfg)
	}
	d := e.only()
	if d.Status != notifications.StatusDelivered || d.Attempts != 1 || !d.DeliveredAt.Equal(t0.Add(time.Second)) ||
		!d.LastAttemptAt.Equal(t0.Add(time.Second)) || d.LastError != "" || !d.LeaseUntil.IsZero() || d.ContactID != c.ID {
		t.Errorf("delivered = %+v", d)
	}
	if n := e.deliver(); n != 0 || len(e.ch.messages()) != 1 {
		t.Error("a delivered notification was sent again")
	}
}

func TestBackoffAndRetryAfter(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.contact(e.alice, "ops", true, domain.EventFailed)
	e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	e.plan()
	var retryAfter time.Duration
	e.ch.setOutcome(func(int, notify.Message) error { return notify.Temporary(503, "unavailable", retryAfter) })

	// The zero jitter draws the shortest delay: half of 30s, 1m, 2m, ...
	at := t0
	for attempt, want := range []time.Duration{15 * time.Second, 30 * time.Second, time.Minute} {
		e.deliver()
		d := e.only()
		if d.Status != notifications.StatusPending || d.Attempts != attempt+1 || !d.NextAttemptAt.Equal(at.Add(want)) ||
			d.LastError != "status 503: unavailable" || !d.LeaseUntil.IsZero() {
			t.Fatalf("after attempt %d: %+v, want next at +%v", attempt+1, d, want)
		}
		if e.deliver() != 0 {
			t.Fatal("a delivery waiting for its retry was sent")
		}
		e.clock.Set(d.NextAttemptAt)
		at = d.NextAttemptAt
	}
	// The target's Retry-After replaces the delay, capped at an hour.
	retryAfter = 2 * time.Minute
	e.deliver()
	if d := e.only(); !d.NextAttemptAt.Equal(at.Add(2 * time.Minute)) {
		t.Errorf("with Retry-After 2m: next at %v", d.NextAttemptAt.Sub(at))
	}
	at = e.only().NextAttemptAt
	e.clock.Set(at)
	retryAfter = 5 * time.Hour
	e.deliver()
	if d := e.only(); !d.NextAttemptAt.Equal(at.Add(time.Hour)) {
		t.Errorf("with Retry-After 5h: next at %v", d.NextAttemptAt.Sub(at))
	}
}

func TestPermanentErrorIsDead(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.contact(e.alice, "ops", true, domain.EventFailed)
	e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	e.plan()
	e.ch.setOutcome(func(int, notify.Message) error { return notify.Permanent(404, "Unknown Webhook") })
	e.deliver()
	d := e.only()
	if d.Status != notifications.StatusDead || d.Attempts != 1 || d.LastError != "status 404: Unknown Webhook" {
		t.Errorf("after a permanent error: %+v", d)
	}
	e.clock.Advance(24 * time.Hour)
	if e.deliver() != 0 || len(e.ch.messages()) != 1 {
		t.Error("a dead delivery was sent again")
	}
}

func TestMaxAttemptsIsDead(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withConfig(func(c *notifications.Config) { c.MaxAttempts = 3 }))
	e.contact(e.alice, "ops", true, domain.EventFailed)
	e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	e.plan()
	e.ch.setOutcome(func(int, notify.Message) error { return errors.New("dial tcp: " + secretURL) })
	for range 3 {
		e.deliver()
		e.clock.Set(e.only().NextAttemptAt.Add(time.Hour))
	}
	d := e.only()
	if d.Status != notifications.StatusDead || d.Attempts != 3 || d.MaxAttempts != 3 || len(e.ch.messages()) != 3 {
		t.Errorf("after 3 failed attempts: %+v (sends %d)", d, len(e.ch.messages()))
	}
	// An error that is not a SendError is never stored or logged as is.
	if strings.Contains(d.LastError, "secret-hook-token") || d.LastError == "" {
		t.Errorf("LastError = %q", d.LastError)
	}
	if strings.Contains(e.logs.String(), "secret-hook-token") {
		t.Error("the log contains the contact's secret")
	}
	if e.deliver() != 0 {
		t.Error("a dead delivery was claimed")
	}
}

// A channel that quotes the secret URL's path in its reason is caught by
// the service's own redaction before the reason is stored or logged.
func TestSecretsAreRedactedFromStoredReasons(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.contact(e.alice, "ops", true, domain.EventFailed)
	e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	e.plan()
	e.ch.setOutcome(func(int, notify.Message) error {
		return notify.Permanent(404, `The requested webhook "POST secret-hook-token-0001" is not registered`)
	})
	e.deliver()
	d := e.only()
	if d.Status != notifications.StatusDead || strings.Contains(d.LastError, "secret-hook-token") || !strings.Contains(d.LastError, "not registered") {
		t.Errorf("delivery = %+v", d)
	}
	if strings.Contains(e.logs.String(), "secret-hook-token") {
		t.Error("the log contains the contact's secret")
	}
}

func TestExpiredLeaseIsReclaimed(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withConfig(func(c *notifications.Config) { c.MaxAttempts = 2 }))
	ctx := context.Background()
	e.contact(e.alice, "ops", true, domain.EventFailed)
	e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	e.plan()
	lease := notifications.DefaultConfig().Lease

	// A deliverer claims the delivery and dies.
	claimed, err := e.store.ClaimDeliveries(ctx, t0, lease, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = (%+v, %v)", claimed, err)
	}
	if e.deliver() != 0 {
		t.Fatal("a leased delivery was claimed again")
	}
	e.clock.Set(t0.Add(lease))
	if e.deliver() != 1 {
		t.Fatal("the expired lease was not reclaimed")
	}
	if d := e.only(); d.Status != notifications.StatusDelivered || d.Attempts != 2 || len(e.ch.messages()) != 1 {
		t.Errorf("after the reclaim: %+v", d)
	}
	// The dead deliverer's late completion loses.
	_, err = e.store.CompleteDelivery(ctx, notifications.Completion{ID: claimed[0].ID, Attempts: 1, Status: notifications.StatusDead, At: t0})
	wantErr(t, "stale completion", err, domain.ErrConflict)
}

func TestExpiredLastAttemptIsDead(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withConfig(func(c *notifications.Config) { c.MaxAttempts = 1 }))
	e.contact(e.alice, "ops", true, domain.EventFailed)
	e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	e.plan()
	lease := notifications.DefaultConfig().Lease
	if _, err := e.store.ClaimDeliveries(context.Background(), t0, lease, 10); err != nil {
		t.Fatal(err)
	}
	e.clock.Set(t0.Add(lease))
	e.deliver()
	d := e.only()
	if d.Status != notifications.StatusDead || d.Attempts != 2 || d.LastError == "" || len(e.ch.messages()) != 0 {
		t.Errorf("a reclaimed last attempt = %+v (sends %d), want dead without a send", d, len(e.ch.messages()))
	}
}

func TestLastErrorNamesTheStatusOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		reason string
		want   string
	}{
		{"Discord answered 404 Not Found: the webhook does not exist", "Discord answered 404 Not Found: the webhook does not exist"},
		{"SMTP 421 4.7.0 too many connections", "SMTP 421 4.7.0 too many connections"},
		{"Unknown Webhook", "status 404: Unknown Webhook"},
		{"error 4040 from the proxy", "status 404: error 4040 from the proxy"},
	} {
		e := newEnv(t)
		e.contact(e.alice, "ops", true, domain.EventFailed)
		e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
		e.plan()
		code := 404
		if strings.HasPrefix(tc.reason, "SMTP") {
			code = 421
		}
		e.ch.setOutcome(func(int, notify.Message) error { return notify.Permanent(code, tc.reason) })
		e.deliver()
		if got := e.only().LastError; got != tc.want {
			t.Errorf("reason %q: last error %q, want %q", tc.reason, got, tc.want)
		}
	}
}

func TestRetryDead(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	e.contact(e.alice, "ops", true, domain.EventFailed)
	e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	e.plan()
	e.ch.setOutcome(func(int, notify.Message) error { return notify.Permanent(401, "bad token") })
	e.deliver()
	dead := e.only()

	for _, a := range []domain.Actor{e.bob, e.admin, {}} {
		_, err := e.svc.Retry(ctx, a, dead.ID)
		wantErr(t, "Retry(other user)", err, domain.ErrNotFound)
	}
	_, err := e.svc.Retry(ctx, e.alice, e.ids.NewDeliveryID())
	wantErr(t, "Retry(missing)", err, domain.ErrNotFound)
	e.clock.Advance(time.Hour)
	got, err := e.svc.Retry(ctx, e.alice, dead.ID)
	if err != nil || got.Status != notifications.StatusPending || got.Attempts != 0 || !got.NextAttemptAt.Equal(t0.Add(time.Hour)) ||
		got.LastError != "status 401: bad token" {
		t.Fatalf("Retry = (%+v, %v)", got, err)
	}
	_, err = e.svc.Retry(ctx, e.alice, dead.ID)
	wantErr(t, "Retry(pending)", err, domain.ErrFailedPrecondition)

	e.ch.setOutcome(nil)
	e.deliver()
	if d := e.only(); d.Status != notifications.StatusDelivered || d.Attempts != 1 {
		t.Errorf("after the retry: %+v", d)
	}
}

func TestDeliverContactProblems(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	c := e.contact(e.alice, "ops", true, domain.EventFailed)
	e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	e.plan()

	// A disabled contact's planned deliveries are still sent.
	if _, err := e.contacts.Update(ctx, e.alice, c.ID, c.Version, contacts.Input{Name: "ops", Options: c.Options}); err != nil {
		t.Fatal(err)
	}
	e.deliver()
	if d := e.only(); d.Status != notifications.StatusDelivered {
		t.Errorf("a disabled contact's delivery = %+v", d)
	}

	// Credentials that cannot be read: dead at once.
	c2 := e.contact(e.alice, "two", true, domain.EventFailed)
	e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	e.plan()
	e.setResolveErr(fmt.Errorf("resolve: %w", domain.ErrFailedPrecondition))
	e.deliver()
	ds, _ := e.store.ListDeliveries(ctx, notifications.DeliveryFilter{ContactID: c2.ID, Limit: -1})
	if len(ds) != 1 || ds[0].Status != notifications.StatusDead || !strings.Contains(ds[0].LastError, "credentials") {
		t.Errorf("undecryptable contact's delivery = %+v", ds)
	}

	// A store failure is temporary.
	e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	e.plan()
	e.setResolveErr(errors.New("connection reset"))
	e.deliver()
	ds, _ = e.store.ListDeliveries(ctx, notifications.DeliveryFilter{Statuses: []notifications.Status{notifications.StatusPending}, Limit: -1})
	if len(ds) != 1 || ds[0].Attempts != 1 || ds[0].LastError == "" {
		t.Errorf("after a store failure: %+v", ds)
	}

	// A contact deleted while its delivery is being sent: nothing is
	// recorded (the delivery goes with the contact).
	e.setResolveErr(fmt.Errorf("resolve: %w", domain.ErrNotFound))
	e.clock.Advance(time.Hour)
	e.deliver()
	ds, _ = e.store.ListDeliveries(ctx, notifications.DeliveryFilter{Statuses: []notifications.Status{notifications.StatusSending}, Limit: -1})
	if len(ds) != 1 || ds[0].Attempts != 2 || ds[0].LastError == "" {
		t.Errorf("an attempt for a deleted contact recorded something: %+v", ds)
	}
	if err := e.contacts.Delete(ctx, e.alice, c2.ID); err != nil {
		t.Fatal(err)
	}
	e.setResolveErr(nil)
	if ds := e.deliveries(); len(ds) != 1 || ds[0].ContactID != c.ID {
		t.Errorf("deliveries after the contact was deleted = %+v", ds)
	}
}

func TestDeliverUnknownChannelType(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	c := contacts.Contact{
		ID: e.ids.NewContactID(), OwnerID: e.alice.UserID, Name: "legacy", Type: "pager", Enabled: true,
		JobEvents: domain.EventSet{domain.EventFailed}, CreatedAt: t0, UpdatedAt: t0, Version: 1,
	}
	if err := e.store.CreateContact(ctx, c); err != nil {
		t.Fatal(err)
	}
	e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	e.plan()
	e.deliver()
	if d := e.only(); d.Status != notifications.StatusDead || !strings.Contains(d.LastError, `"pager"`) {
		t.Errorf("delivery to an unknown channel type = %+v", d)
	}
}

func TestDeliverInterruptedByShutdown(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.contact(e.alice, "ops", true, domain.EventFailed)
	e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	e.plan()
	e.ch.block, e.ch.sent = true, make(chan notify.Message, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := e.svc.DeliverDue(ctx)
		done <- err
	}()
	<-e.ch.sent
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// The outcome is recorded despite the cancelled context, for a prompt
	// retry. The attempt is given back: our shutdown is not a failure of
	// the target.
	if d := e.only(); d.Status != notifications.StatusPending || d.Attempts != 0 || !d.NextAttemptAt.Equal(t0) || d.LastError == "" {
		t.Errorf("an interrupted attempt = %+v", d)
	}
}

// shutdownDuringAttempt claims the delivery and cancels the pass while the
// send is in flight.
func shutdownDuringAttempt(t *testing.T, e *env) {
	t.Helper()
	e.ch.block, e.ch.sent = true, make(chan notify.Message, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := e.svc.DeliverDue(ctx)
		done <- err
	}()
	<-e.ch.sent
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	e.ch.block, e.ch.sent = false, nil
}

// An attempt cut short by the controller's own shutdown must not end the
// delivery, even when it was the last one allowed: nothing was wrong with
// the notification, so the next start sends it.
func TestShutdownOnTheLastAttemptDoesNotKillTheDelivery(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withConfig(func(c *notifications.Config) { c.MaxAttempts = 2 }))
	e.contact(e.alice, "ops", true, domain.EventFailed)
	e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	e.plan()

	e.ch.setOutcome(func(int, notify.Message) error { return notify.Temporary(503, "unavailable", 0) })
	e.deliver() // attempt 1 fails
	e.clock.Set(e.only().NextAttemptAt)

	shutdownDuringAttempt(t, e) // attempt 2, the last, is interrupted
	d := e.only()
	if d.Status != notifications.StatusPending || d.Attempts != 1 || !d.NextAttemptAt.Equal(e.clock.Now()) {
		t.Fatalf("after the interrupted last attempt: %+v", d)
	}

	// The next start sends it, and the budget is intact.
	e.ch.setOutcome(func(int, notify.Message) error { return nil })
	if e.deliver() != 1 {
		t.Fatal("the interrupted delivery was not retried at once")
	}
	if d := e.only(); d.Status != notifications.StatusDelivered || d.Attempts != 2 {
		t.Errorf("after the retry: %+v", d)
	}
}

// A receiver that does not answer within the send timeout is a failed
// attempt, unlike our own shutdown.
func TestReceiverTimeoutOnTheLastAttemptIsDead(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withConfig(func(c *notifications.Config) { c.MaxAttempts = 1; c.SendTimeout = 20 * time.Millisecond }))
	e.contact(e.alice, "ops", true, domain.EventFailed)
	e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	e.plan()
	e.ch.block = true
	e.deliver()
	if d := e.only(); d.Status != notifications.StatusDead || d.Attempts != 1 {
		t.Errorf("after a receiver timeout on the last attempt: %+v", d)
	}
}

// A reply the target gave counts even when the shutdown came right after.
func TestReplyBeforeShutdownCounts(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withConfig(func(c *notifications.Config) { c.MaxAttempts = 1 }))
	e.contact(e.alice, "ops", true, domain.EventFailed)
	e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	e.plan()
	ctx, cancel := context.WithCancel(context.Background())
	e.ch.setOutcome(func(int, notify.Message) error {
		cancel()
		return notify.Permanent(404, "not found")
	})
	if _, err := e.svc.DeliverDue(ctx); err != nil {
		t.Fatal(err)
	}
	if d := e.only(); d.Status != notifications.StatusDead || d.Attempts != 1 {
		t.Errorf("after a 404 during shutdown: %+v", d)
	}
}

func TestListDeliveries(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	c := e.contact(e.alice, "ops", true, domain.EventFailed)
	e.contact(e.bob, "theirs", true, domain.EventFailed)
	for range 5 {
		e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	}
	e.job(e.bob, domain.JobFailed, domain.ScheduleID{})
	e.plan()

	page, err := e.svc.List(ctx, e.alice, notifications.DeliveryFilter{Limit: 2, OwnerID: e.bob.UserID})
	if err != nil || len(page.Deliveries) != 2 || page.Next != page.Deliveries[1].ID {
		t.Fatalf("List(2) = (%+v, %v)", page, err)
	}
	var all []notifications.Delivery
	for f := (notifications.DeliveryFilter{Limit: 2}); ; {
		page, err := e.svc.List(ctx, e.alice, f)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page.Deliveries...)
		if page.Next.IsZero() {
			break
		}
		f.Before = page.Next
	}
	if len(all) != 5 {
		t.Errorf("paged through %d deliveries, want alice's 5", len(all))
	}
	for i, d := range all {
		if d.OwnerID != e.alice.UserID || d.ContactID != c.ID || (i > 0 && d.ID.String() >= all[i-1].ID.String()) {
			t.Errorf("delivery %d = %+v, want alice's newest first", i, d)
		}
	}
	if page, _ := e.svc.List(ctx, e.alice, notifications.DeliveryFilter{Statuses: []notifications.Status{notifications.StatusDead}}); len(page.Deliveries) != 0 {
		t.Errorf("List(dead) = %+v", page)
	}
	if page, _ := e.svc.List(ctx, e.admin, notifications.DeliveryFilter{}); len(page.Deliveries) != 0 {
		t.Errorf("List(admin) = %+v, want only the admin's own", page)
	}
	if page, err := e.svc.List(ctx, domain.Actor{}, notifications.DeliveryFilter{}); err != nil || len(page.Deliveries) != 0 {
		t.Errorf("List(no user) = (%+v, %v)", page, err)
	}
	_, err = e.svc.List(ctx, e.alice, notifications.DeliveryFilter{Statuses: []notifications.Status{"lost"}})
	wantErr(t, "List(bad status)", err, domain.ErrInvalidArgument)
	_, err = e.svc.List(ctx, e.alice, notifications.DeliveryFilter{Limit: -1})
	wantErr(t, "List(negative limit)", err, domain.ErrInvalidArgument)
}

func TestPurge(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withConfig(func(c *notifications.Config) { c.Retention = 24 * time.Hour }))
	e.contact(e.alice, "ops", true, domain.EventFailed)
	e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	e.plan()
	e.deliver()
	e.clock.Advance(23 * time.Hour)
	e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	e.plan() // pending: never purged
	e.clock.Advance(2 * time.Hour)
	n, err := e.svc.Purge(context.Background())
	if err != nil || n != 1 {
		t.Errorf("Purge = (%d, %v), want the old delivered one", n, err)
	}
	if d := e.only(); d.Status != notifications.StatusPending {
		t.Errorf("left = %+v", d)
	}
}

func TestRunDeliversWhenWoken(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withConfig(func(c *notifications.Config) { c.Tick = time.Hour }))
	e.contact(e.alice, "ops", true, domain.EventFailed)
	e.ch.sent = make(chan notify.Message, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.svc.Run(ctx) }()

	first := e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	e.svc.Wake()
	if m := <-e.ch.sent; m.Job == nil || m.Job.ID != first.ID.String() {
		t.Errorf("first notification = %+v", m)
	}
	// The ticker is an hour: only Wake can plan the second job now.
	second := e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	e.svc.Wake()
	e.svc.Wake() // never blocks
	if m := <-e.ch.sent; m.Job == nil || m.Job.ID != second.ID.String() {
		t.Errorf("second notification = %+v", m)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run = %v", err)
	}
}

func TestRunReasonOwnerDisabledIsPinned(t *testing.T) {
	t.Parallel()
	// The planner mirrors schedules.ReasonOwnerDisabled (TestPlanRuns
	// checks the behavior): changing one without the other breaks it.
	if schedules.ReasonOwnerDisabled != "owner_disabled" {
		t.Fatalf("schedules.ReasonOwnerDisabled = %q; update notifications.reasonOwnerDisabled", schedules.ReasonOwnerDisabled)
	}
}
