//go:build integration

package postgres

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/contacts"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/notifications"
	"github.com/Zeptiny/Syncr/internal/descriptor"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/notify"
)

// These tests cover the outbox at the SQL level: schedules, their contact
// links and runs are inserted with plain SQL (the schedules adapter is not
// needed), and claims and commits race across two connection pools, like
// two controllers.

// outboxFixture is a migrated database with an owner and a worker.
type outboxFixture struct {
	t      *testing.T
	s      *Store
	db     string
	owner  domain.UserID
	worker domain.WorkerID
	n      int
}

func newOutboxFixture(t *testing.T) *outboxFixture {
	t.Helper()
	name := createDB(t, true)
	f := &outboxFixture{t: t, s: openStore(t, name), db: name}
	f.owner = seedUser(t, f.s)
	w := newTestWorker()
	if err := f.s.CreateWorker(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	f.worker = w.ID
	return f
}

// contact creates a contact of the fixture's owner.
func (f *outboxFixture) contact(name string, enabled bool, events ...domain.NotificationEvent) domain.ContactID {
	f.t.Helper()
	c := contacts.Contact{
		ID: domain.NewContactID(), OwnerID: f.owner, Name: name, Type: "webhook", Options: map[string]string{},
		Enabled: enabled, JobEvents: events, CreatedAt: testTime(), UpdatedAt: testTime(), Version: 1,
	}
	if err := f.s.CreateContact(f.t.Context(), c); err != nil {
		f.t.Fatal(err)
	}
	return c.ID
}

// schedule inserts a paused schedule of the owner on the worker.
func (f *outboxFixture) schedule(name string) domain.ScheduleID {
	f.t.Helper()
	id := domain.NewScheduleID()
	_, err := f.s.pool.Exec(f.t.Context(), `
		INSERT INTO schedules (id, owner_id, name, cron, timezone, kind, definition, worker_id,
		                       missed_action, overlap, enabled, created_at, updated_at)
		VALUES ($1, $2, $3, '30 2 * * *', 'Europe/Berlin', 'rclone_transfer', '{}', $4,
		        'run_late', 'skip', false, now(), now())`,
		pgID(id), pgID(f.owner), name, pgID(f.worker))
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *outboxFixture) link(schedule domain.ScheduleID, contact domain.ContactID, events ...domain.NotificationEvent) {
	f.t.Helper()
	_, err := f.s.pool.Exec(f.t.Context(), `INSERT INTO schedule_contacts (schedule_id, contact_id, events) VALUES ($1, $2, $3)`,
		pgID(schedule), pgID(contact), outboxEventStrings(events))
	if err != nil {
		f.t.Fatal(err)
	}
}

// run inserts a run of schedule with status, ended at updated.
func (f *outboxFixture) run(schedule domain.ScheduleID, status, reason string, updated time.Time) domain.ScheduleRunID {
	f.t.Helper()
	id := domain.NewScheduleRunID()
	var job any
	if status == "launched" {
		job = pgID(domain.NewJobID())
	}
	_, err := f.s.pool.Exec(f.t.Context(), `
		INSERT INTO schedule_runs (id, schedule_id, trigger, scheduled_for, deadline, coalesced, status, reason,
		                           message, worker_id, job_id, created_at, updated_at)
		VALUES ($1, $2, 'cron', $3, $4, 2, $5, $6, 'offline', $7, $8, $3, $9)`,
		pgID(id), pgID(schedule), pgTime(testTime()), pgTime(testTime().Add(time.Hour)), status, reason,
		pgID(f.worker), job, pgTime(updated))
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

// job stores a job of the owner that finished with status at finished.
func (f *outboxFixture) job(status domain.JobStatus, schedule domain.ScheduleID, finished time.Time) domain.JobID {
	f.t.Helper()
	ctx := f.t.Context()
	j := newTestJob(f.worker, f.owner)
	j.ScheduleID = schedule
	if err := f.s.CreateJob(ctx, j); err != nil {
		f.t.Fatal(err)
	}
	j.Status, j.StartedAt, j.FinishedAt = status, finished.Add(-time.Minute), finished
	j.Stats, j.Error = jobs.Stats{Bytes: 7, Files: 3, Errors: 1}, jobs.JobError{Code: "failed", Message: "boom"}
	if _, err := f.s.UpdateJob(ctx, j); err != nil {
		f.t.Fatal(err)
	}
	return j.ID
}

// delivery is a new pending delivery to contact for job.
func (f *outboxFixture) delivery(contact domain.ContactID, job domain.JobID, key string) notifications.Delivery {
	f.n++
	return notifications.Delivery{
		ID: domain.NewDeliveryID(), ContactID: contact, OwnerID: f.owner, Event: domain.EventFailed, DedupeKey: key,
		JobID: job, Payload: []byte(`{"version": 1}`), Status: notifications.StatusPending, MaxAttempts: 8,
		NextAttemptAt: testTime().Add(time.Duration(f.n) * time.Millisecond), CreatedAt: testTime(),
	}
}

func jobOutcome(t *testing.T, s *Store, id domain.JobID) notifications.JobOutcome {
	t.Helper()
	os, err := s.PendingJobOutcomes(t.Context(), -1)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range os {
		if o.JobID == id {
			return o
		}
	}
	t.Fatalf("job %s is not pending", id)
	return notifications.JobOutcome{}
}

func subscriberIDs(subs []notifications.Subscriber) []domain.ContactID {
	out := make([]domain.ContactID, len(subs))
	for i, s := range subs {
		out[i] = s.ContactID
	}
	return out
}

func TestOutboxScheduledSubscribersAndRuns(t *testing.T) {
	t.Parallel()
	f := newOutboxFixture(t)
	ctx := t.Context()
	linked := f.contact("linked", true)
	off := f.contact("off", false)
	manual := f.contact("manual", true, domain.EventFailed)
	sc := f.schedule("Nightly")
	f.link(sc, linked, domain.EventSucceeded, domain.EventMissed)
	f.link(sc, off, domain.EventSucceeded)

	scheduled := f.job(domain.JobSucceeded, sc, testTime().Add(time.Minute))
	byHand := f.job(domain.JobFailed, domain.ScheduleID{}, testTime().Add(2*time.Minute))
	o := jobOutcome(t, f.s, scheduled)
	if o.ScheduleID != sc || o.ScheduleName != "Nightly" || o.WorkerID != f.worker || o.Bytes != 7 || o.Files != 3 || o.Errors != 1 ||
		o.ErrorCode != "failed" || !o.FinishedAt.Equal(testTime().Add(time.Minute)) || !o.StartedAt.Equal(testTime()) ||
		!slices.Equal(subscriberIDs(o.Subscribers), []domain.ContactID{linked}) ||
		!slices.Equal(o.Subscribers[0].Events, domain.EventSet{domain.EventSucceeded, domain.EventMissed}) {
		t.Errorf("scheduled job outcome = %+v, want the enabled linked contact only", o)
	}
	if o := jobOutcome(t, f.s, byHand); !o.ScheduleID.IsZero() || o.ScheduleName != "" ||
		!slices.Equal(subscriberIDs(o.Subscribers), []domain.ContactID{manual}) {
		t.Errorf("manual job outcome = %+v, want the contacts with job events", o)
	}

	missed := f.run(sc, "missed", "worker_offline", testTime().Add(2*time.Minute))
	failed := f.run(sc, "failed", "worker_removed", testTime().Add(time.Minute))
	f.run(sc, "launched", "", testTime())
	f.run(sc, "pending", "", testTime())
	runs, err := f.s.PendingRunOutcomes(ctx, -1)
	if err != nil || len(runs) != 2 || runs[0].RunID != failed || runs[1].RunID != missed {
		t.Fatalf("PendingRunOutcomes = (%+v, %v), want the failed and missed runs by end time", runs, err)
	}
	r := runs[1]
	if r.ScheduleID != sc || r.ScheduleName != "Nightly" || r.OwnerID != f.owner || r.Status != "missed" || r.Reason != "worker_offline" ||
		r.Message != "offline" || r.Coalesced != 2 || r.WorkerID != f.worker || !r.ScheduledFor.Equal(testTime()) ||
		!slices.Equal(subscriberIDs(r.Subscribers), []domain.ContactID{linked}) {
		t.Errorf("run outcome = %+v", r)
	}
	if runs, _ := f.s.PendingRunOutcomes(ctx, 1); len(runs) != 1 || runs[0].RunID != failed {
		t.Errorf("PendingRunOutcomes(1) = %+v", runs)
	}
	d := f.delivery(linked, domain.JobID{}, notifications.RunDedupeKey(missed, domain.EventMissed))
	d.Event, d.ScheduleID = domain.EventMissed, sc
	if n, err := f.s.CommitRunOutcome(ctx, missed, []notifications.Delivery{d}, testTime()); err != nil || n != 1 {
		t.Fatalf("CommitRunOutcome = (%d, %v)", n, err)
	}
	again := d
	again.ID = domain.NewDeliveryID()
	if n, err := f.s.CommitRunOutcome(ctx, missed, []notifications.Delivery{again}, testTime()); err != nil || n != 0 {
		t.Errorf("CommitRunOutcome(again) = (%d, %v)", n, err)
	}
	if runs, _ := f.s.PendingRunOutcomes(ctx, -1); len(runs) != 1 || runs[0].RunID != failed {
		t.Errorf("pending after the commit = %+v", runs)
	}
	_, err = f.s.CommitRunOutcome(ctx, domain.NewScheduleRunID(), nil, testTime())
	wantDomainErr(t, "CommitRunOutcome(missing)", err, domain.ErrNotFound)

	// Deleting the schedule keeps the job's history and delivery log: the
	// references are cleared, and its unnotified job becomes a manual one.
	if _, err := f.s.pool.Exec(ctx, `DELETE FROM schedules WHERE id = $1`, pgID(sc)); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.GetDelivery(ctx, d.ID); err != nil || !got.ScheduleID.IsZero() {
		t.Errorf("delivery after the schedule was deleted = (%+v, %v)", got, err)
	}
	if o := jobOutcome(t, f.s, scheduled); !o.ScheduleID.IsZero() || !slices.Equal(subscriberIDs(o.Subscribers), []domain.ContactID{manual}) {
		t.Errorf("job outcome after the schedule was deleted = %+v", o)
	}
	if runs, _ := f.s.PendingRunOutcomes(ctx, -1); len(runs) != 0 {
		t.Errorf("runs of a deleted schedule are pending: %+v", runs)
	}
}

func TestOutboxClaimsDisjointAcrossPools(t *testing.T) {
	t.Parallel()
	f := newOutboxFixture(t)
	ctx := t.Context()
	other := openStore(t, f.db) // a second pool: another controller
	c := f.contact("ops", true)
	const total = 300
	j := f.job(domain.JobFailed, domain.ScheduleID{}, testTime())
	ds := make([]notifications.Delivery, total)
	for i := range ds {
		ds[i] = f.delivery(c, j, fmt.Sprintf("k%d", i))
	}
	if n, err := f.s.CommitJobOutcome(ctx, j, ds, testTime()); err != nil || n != total {
		t.Fatalf("CommitJobOutcome = (%d, %v)", n, err)
	}

	now, lease := testTime().Add(time.Hour), 2*time.Minute
	var mu sync.Mutex
	seen := map[domain.DeliveryID]int{}
	var wg sync.WaitGroup
	for i := range 8 {
		s := f.s
		if i%2 == 1 {
			s = other
		}
		wg.Go(func() {
			for {
				got, err := s.ClaimDeliveries(ctx, now, lease, 7)
				if err != nil {
					t.Error(err)
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, d := range got {
					seen[d.ID]++
					if d.Attempts != 1 || d.Status != notifications.StatusSending || !d.LeaseUntil.Equal(now.Add(lease)) {
						t.Errorf("claimed %+v", d)
					}
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(seen) != total {
		t.Errorf("claimed %d deliveries, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("delivery %s claimed %d times", id, n)
		}
	}
}

func TestOutboxLeaseReclaimAcrossPools(t *testing.T) {
	t.Parallel()
	f := newOutboxFixture(t)
	ctx := t.Context()
	other := openStore(t, f.db)
	c := f.contact("ops", true)
	j := f.job(domain.JobFailed, domain.ScheduleID{}, testTime())
	d := f.delivery(c, j, "k")
	if _, err := f.s.CommitJobOutcome(ctx, j, []notifications.Delivery{d}, testTime()); err != nil {
		t.Fatal(err)
	}
	lease := 2 * time.Minute
	t1 := testTime().Add(time.Second)
	first, err := f.s.ClaimDeliveries(ctx, t1, lease, 10)
	if err != nil || len(first) != 1 {
		t.Fatalf("claim = (%+v, %v)", first, err)
	}
	if got, err := other.ClaimDeliveries(ctx, t1.Add(lease-time.Microsecond), lease, 10); err != nil || len(got) != 0 {
		t.Fatalf("a leased delivery was claimed again: (%+v, %v)", got, err)
	}
	// The first deliverer died: the other pool reclaims the delivery once
	// the lease expired, and the attempt counts.
	re, err := other.ClaimDeliveries(ctx, t1.Add(lease), lease, 10)
	if err != nil || len(re) != 1 || re[0].Attempts != 2 || !re[0].LeaseUntil.Equal(t1.Add(2*lease)) || !re[0].LastAttemptAt.Equal(t1.Add(lease)) {
		t.Fatalf("reclaim = (%+v, %v)", re, err)
	}
	_, err = f.s.CompleteDelivery(ctx, notifications.Completion{ID: d.ID, Attempts: 1, Status: notifications.StatusDelivered, At: t1})
	wantDomainErr(t, "CompleteDelivery(stale)", err, domain.ErrConflict)
	got, err := other.CompleteDelivery(ctx, notifications.Completion{ID: d.ID, Attempts: 2, Status: notifications.StatusDelivered, At: t1.Add(lease)})
	if err != nil || got.Status != notifications.StatusDelivered || !got.DeliveredAt.Equal(t1.Add(lease)) || !got.LeaseUntil.IsZero() {
		t.Errorf("CompleteDelivery = (%+v, %v)", got, err)
	}
	_, err = f.s.CompleteDelivery(ctx, notifications.Completion{ID: d.ID, Attempts: 2, Status: "lost", At: t1})
	wantDomainErr(t, "CompleteDelivery(bad status)", err, domain.ErrInvalidArgument)
}

func TestOutboxCommitRacesContactDelete(t *testing.T) {
	t.Parallel()
	f := newOutboxFixture(t)
	ctx := t.Context()
	other := openStore(t, f.db)
	for i := range 20 {
		c := f.contact(fmt.Sprintf("c%d", i), true, domain.EventFailed)
		j := f.job(domain.JobFailed, domain.ScheduleID{}, testTime())
		d := f.delivery(c, j, notifications.JobDedupeKey(j, domain.EventFailed))
		var wg sync.WaitGroup
		wg.Go(func() {
			if _, err := f.s.CommitJobOutcome(ctx, j, []notifications.Delivery{d}, testTime()); err != nil {
				t.Errorf("CommitJobOutcome racing a delete: %v", err)
			}
		})
		wg.Go(func() {
			if err := other.DeleteContact(ctx, c); err != nil {
				t.Errorf("DeleteContact racing a commit: %v", err)
			}
		})
		wg.Wait()
		if _, err := f.s.GetDelivery(ctx, d.ID); err == nil {
			t.Errorf("round %d: a delivery of a deleted contact survived", i)
		}
		if os, _ := f.s.PendingJobOutcomes(ctx, -1); len(os) != 0 {
			t.Errorf("round %d: the job was not marked: %+v", i, os)
		}
	}
}

// fixedClock, uuidIDs and noContacts are the notifications service's ports
// for a planning-only test.
type fixedClock struct{}

func (fixedClock) Now() time.Time { return testTime() }

type uuidIDs struct{}

func (uuidIDs) NewDeliveryID() domain.DeliveryID { return domain.NewDeliveryID() }

type noContacts struct{}

func (noContacts) ResolveChannel(context.Context, domain.ContactID) (string, descriptor.Values, error) {
	return "", descriptor.Values{}, domain.ErrNotFound
}

func TestOutboxPlanOnceAcrossControllers(t *testing.T) {
	t.Parallel()
	f := newOutboxFixture(t)
	ctx := t.Context()
	c1, c2 := f.contact("one", true, domain.EventFailed), f.contact("two", true, domain.EventFailed)
	const jobsN = 120
	for i := range jobsN {
		f.job(domain.JobFailed, domain.ScheduleID{}, testTime().Add(time.Duration(i)*time.Second))
	}
	reg, err := notify.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	var svcs []*notifications.Service
	for _, s := range []*Store{f.s, openStore(t, f.db)} {
		svc, err := notifications.New(notifications.Deps{Repo: s, Channels: reg, Contacts: noContacts{}, Clock: fixedClock{}, IDs: uuidIDs{}},
			notifications.Config{})
		if err != nil {
			t.Fatal(err)
		}
		svcs = append(svcs, svc)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	planned := 0
	for i := range 6 {
		wg.Go(func() {
			n, err := svcs[i%2].Plan(ctx)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			planned += n
			mu.Unlock()
		})
	}
	wg.Wait()
	ds, err := f.s.ListDeliveries(ctx, notifications.DeliveryFilter{Limit: -1})
	if err != nil {
		t.Fatal(err)
	}
	if planned != 2*jobsN || len(ds) != 2*jobsN {
		t.Fatalf("planned %d, stored %d, want %d", planned, len(ds), 2*jobsN)
	}
	per := map[domain.ContactID]int{}
	for _, d := range ds {
		per[d.ContactID]++
	}
	if per[c1] != jobsN || per[c2] != jobsN {
		t.Errorf("deliveries per contact = %v", per)
	}
}
