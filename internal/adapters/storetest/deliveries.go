package storetest

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
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// This file is the contract suite of notifications.Repository (the
// outbox).

const testPayload = `{"version": 1, "event": "failed", "title": "Job failed"}`

// requireDeliveries creates the users of the test and skips it while the
// outbox or the contacts port is a stub.
func requireDeliveries(t *testing.T, s Store, owners ...domain.UserID) {
	t.Helper()
	requireContacts(t, s, owners...)
	_, err := s.ListDeliveries(context.Background(), notifications.DeliveryFilter{Limit: -1})
	skipIfUnsupported(t, "ListDeliveries", "track B (notifications)", err)
}

func newDelivery(t *testing.T, n int, c domain.ContactID, owner domain.UserID, key string) notifications.Delivery {
	return notifications.Delivery{
		ID: deliveryID(t, n), ContactID: c, OwnerID: owner, Event: domain.EventFailed, DedupeKey: key,
		Payload: []byte(testPayload), Status: notifications.StatusPending, MaxAttempts: 8,
		NextAttemptAt: t0(), CreatedAt: t0(),
	}
}

// finishJob creates job n on w (of the suite's job owner) and moves it to
// status at finished.
func finishJob(t *testing.T, s Store, n int, w domain.WorkerID, status domain.JobStatus, finished time.Time, schedule domain.ScheduleID) jobs.Job {
	t.Helper()
	j := newJob(t, n, w)
	j.ScheduleID = schedule
	mustCreateJob(t, s, j)
	if status == domain.JobQueued {
		return j
	}
	j.Status, j.FinishedAt, j.Stats = status, finished, jobs.Stats{Bytes: 100, Files: 2, Errors: 1}
	j.Error = jobs.JobError{Code: "path_not_allowed", Message: "outside local_roots"}
	stored, err := s.UpdateJob(context.Background(), j)
	if err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	return stored
}

// commitJob plans ds for job id and fails the test on error.
func commitJob(t *testing.T, s Store, id domain.JobID, ds ...notifications.Delivery) int {
	t.Helper()
	n, err := s.CommitJobOutcome(context.Background(), id, ds, t0())
	if err != nil {
		t.Fatalf("CommitJobOutcome: %v", err)
	}
	return n
}

func pendingJobIDs(t *testing.T, s Store) []domain.JobID {
	t.Helper()
	os, err := s.PendingJobOutcomes(context.Background(), -1)
	if err != nil {
		t.Fatalf("PendingJobOutcomes: %v", err)
	}
	out := make([]domain.JobID, len(os))
	for i, o := range os {
		out[i] = o.JobID
	}
	return out
}

func deliveryIDs(ds []notifications.Delivery) []domain.DeliveryID {
	out := make([]domain.DeliveryID, len(ds))
	for i, d := range ds {
		out[i] = d.ID
	}
	return out
}

func testDeliveryPlanJobOnce(t *testing.T, s Store) {
	ctx := context.Background()
	owner := ownerID(t)
	requireDeliveries(t, s, owner)
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	c := newContact(t, 1, owner, "Ops")
	c.JobEvents = domain.EventSet{domain.EventFailed}
	mustCreateContact(t, s, c)
	j := finishJob(t, s, 1, w.ID, domain.JobFailed, t0().Add(time.Minute), domain.ScheduleID{})

	outcomes, err := s.PendingJobOutcomes(ctx, -1)
	if err != nil || len(outcomes) != 1 {
		t.Fatalf("PendingJobOutcomes = (%+v, %v)", outcomes, err)
	}
	o := outcomes[0]
	if o.JobID != j.ID || o.OwnerID != owner || o.Kind != j.Kind || !sameJSON(o.Definition, j.Definition) || o.Status != domain.JobFailed || o.WorkerID != w.ID ||
		o.WorkerName != w.Name || !o.ScheduleID.IsZero() || o.ErrorCode != "path_not_allowed" || o.ErrorMessage == "" ||
		o.Bytes != 100 || o.Files != 2 || o.Errors != 1 || !o.FinishedAt.Equal(t0().Add(time.Minute)) ||
		len(o.Subscribers) != 1 || o.Subscribers[0].ContactID != c.ID || !slices.Equal(o.Subscribers[0].Events, c.JobEvents) {
		t.Errorf("job outcome = %+v", o)
	}

	// Concurrent planners insert the delivery once.
	key := notifications.JobDedupeKey(j.ID, domain.EventFailed)
	var wg sync.WaitGroup
	var mu sync.Mutex
	inserted := 0
	for i := range 8 {
		wg.Go(func() {
			d := newDelivery(t, 100+i, c.ID, owner, key)
			d.JobID = j.ID
			n, err := s.CommitJobOutcome(ctx, j.ID, []notifications.Delivery{d}, t0())
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			inserted += n
			mu.Unlock()
		})
	}
	wg.Wait()
	if inserted != 1 {
		t.Errorf("concurrent commits inserted %d deliveries, want 1", inserted)
	}
	if ds, _ := s.ListDeliveries(ctx, notifications.DeliveryFilter{Limit: -1}); len(ds) != 1 || ds[0].JobID != j.ID || ds[0].DedupeKey != key {
		t.Errorf("deliveries after the race = %+v", ds)
	}
	if got := pendingJobIDs(t, s); len(got) != 0 {
		t.Errorf("a planned job is still pending: %v", got)
	}

	// A deleted contact's delivery is skipped; the job is marked anyway.
	j2 := finishJob(t, s, 2, w.ID, domain.JobInterrupted, t0().Add(2*time.Minute), domain.ScheduleID{})
	gone := newDelivery(t, 1, contactID(t, 99), owner, notifications.JobDedupeKey(j2.ID, domain.EventInterrupted))
	if n := commitJob(t, s, j2.ID, gone); n != 0 {
		t.Errorf("a delivery to a missing contact was inserted (%d)", n)
	}
	// An invalid delivery fails the commit; the job stays pending.
	j3 := finishJob(t, s, 3, w.ID, domain.JobSucceeded, t0().Add(3*time.Minute), domain.ScheduleID{})
	bad := newDelivery(t, 2, c.ID, owner, notifications.JobDedupeKey(j3.ID, domain.EventTest))
	bad.Event = domain.EventTest
	_, err = s.CommitJobOutcome(ctx, j3.ID, []notifications.Delivery{bad}, t0())
	wantErr(t, "CommitJobOutcome(test event)", err, domain.ErrInvalidArgument)
	if got := pendingJobIDs(t, s); !slices.Equal(got, []domain.JobID{j3.ID}) {
		t.Errorf("pending after a failed commit = %v", got)
	}
	// Cancelled jobs are planned (without deliveries) like the others.
	j4 := finishJob(t, s, 4, w.ID, domain.JobCancelled, t0().Add(4*time.Minute), domain.ScheduleID{})
	if got := pendingJobIDs(t, s); !slices.Equal(got, []domain.JobID{j3.ID, j4.ID}) {
		t.Errorf("pending = %v, want by finish time with the cancelled job", got)
	}
	commitJob(t, s, j4.ID)
	_, err = s.CommitJobOutcome(ctx, jobID(t, 99), nil, t0())
	wantErr(t, "CommitJobOutcome(missing)", err, domain.ErrNotFound)
}

func testDeliverySubscribers(t *testing.T, s Store) {
	ctx := context.Background()
	owner, other := ownerID(t), userID(t, 2)
	requireDeliveries(t, s, owner, other)
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	on := newContact(t, 1, owner, "on")
	on.JobEvents = domain.EventSet{domain.EventFailed}
	off := newContact(t, 2, owner, "off")
	off.JobEvents, off.Enabled = domain.EventSet{domain.EventFailed}, false
	quiet := newContact(t, 3, owner, "quiet") // no job events
	theirs := newContact(t, 4, other, "theirs")
	theirs.JobEvents = domain.EventSet{domain.EventFailed}
	for _, c := range []contacts.Contact{on, off, quiet, theirs} {
		mustCreateContact(t, s, c)
	}
	finishJob(t, s, 9, w.ID, domain.JobQueued, time.Time{}, domain.ScheduleID{}) // unfinished: never pending
	manual := finishJob(t, s, 1, w.ID, domain.JobFailed, t0().Add(time.Minute), domain.ScheduleID{})
	subscribers := func(id domain.JobID) []notifications.Subscriber {
		t.Helper()
		os, err := s.PendingJobOutcomes(ctx, -1)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range os {
			if o.JobID == id {
				return o.Subscribers
			}
		}
		t.Fatalf("job %s is not pending", id)
		return nil
	}
	if got := subscribers(manual.ID); len(got) != 1 || got[0].ContactID != on.ID {
		t.Errorf("manual job subscribers = %+v, want the owner's enabled contact with job events", got)
	}
	if got := pendingJobIDs(t, s); !slices.Equal(got, []domain.JobID{manual.ID}) {
		t.Errorf("pending = %v", got)
	}
	if !schedulesSupported(t, s) {
		t.Skip("m4: pending track A (schedules): scheduled job subscribers are not checked")
	}
	sc := newSchedule(t, 1, owner, "Nightly", w.ID)
	sc.Contacts = []schedules.ContactLink{
		{ContactID: quiet.ID, Events: domain.EventSet{domain.EventSucceeded}},
		{ContactID: off.ID, Events: domain.EventSet{domain.EventSucceeded}},
	}
	mustCreateSchedule(t, s, sc)
	scheduled := finishJob(t, s, 2, w.ID, domain.JobSucceeded, t0().Add(2*time.Minute), sc.ID)
	os, err := s.PendingJobOutcomes(ctx, 1)
	if err != nil || len(os) != 1 || os[0].JobID != manual.ID {
		t.Errorf("PendingJobOutcomes(1) = (%+v, %v), want the oldest", os, err)
	}
	got := subscribers(scheduled.ID)
	if len(got) != 1 || got[0].ContactID != quiet.ID || !slices.Equal(got[0].Events, domain.EventSet{domain.EventSucceeded}) {
		t.Errorf("scheduled job subscribers = %+v, want the enabled linked contacts only (never job events)", got)
	}
	os, _ = s.PendingJobOutcomes(ctx, -1)
	if o := os[len(os)-1]; o.ScheduleID != sc.ID || o.ScheduleName != "Nightly" {
		t.Errorf("scheduled job outcome = %+v", o)
	}
}

func testDeliveryPlanRun(t *testing.T, s Store) {
	ctx := context.Background()
	owner := ownerID(t)
	requireDeliveries(t, s, owner)
	if !schedulesSupported(t, s) {
		t.Skip("m4: pending track A (schedules): run outcomes need schedules")
	}
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	c := newContact(t, 1, owner, "Ops")
	mustCreateContact(t, s, c)
	sc := newSchedule(t, 1, owner, "Nightly", w.ID)
	sc.Contacts = []schedules.ContactLink{{ContactID: c.ID, Events: domain.EventSet{domain.EventMissed}}}
	mustCreateSchedule(t, s, sc)
	mk := func(n int, st schedules.RunStatus, updated time.Time) schedules.Run {
		r := newRun(t, n, sc.ID, st)
		r.UpdatedAt, r.Reason, r.Coalesced, r.WorkerID = updated, schedules.ReasonWorkerOffline, 2, w.ID
		if st == schedules.RunLaunched {
			r.JobID = jobID(t, n)
		}
		return mustAddRun(t, s, r)
	}
	r2 := mk(2, schedules.RunFailed, t0().Add(2*time.Minute))
	r1 := mk(1, schedules.RunMissed, t0().Add(time.Minute))
	mk(3, schedules.RunLaunched, t0())
	mk(4, schedules.RunPending, t0())
	os, err := s.PendingRunOutcomes(ctx, -1)
	if err != nil || len(os) != 2 || os[0].RunID != r1.ID || os[1].RunID != r2.ID {
		t.Fatalf("PendingRunOutcomes = (%+v, %v), want the missed and failed runs by update time", os, err)
	}
	o := os[0]
	if o.ScheduleID != sc.ID || o.ScheduleName != "Nightly" || o.OwnerID != owner || o.Status != string(schedules.RunMissed) ||
		o.Reason != schedules.ReasonWorkerOffline || o.Coalesced != 2 || o.WorkerID != w.ID || !o.ScheduledFor.Equal(t0()) ||
		len(o.Subscribers) != 1 || o.Subscribers[0].ContactID != c.ID {
		t.Errorf("run outcome = %+v", o)
	}
	d := newDelivery(t, 1, c.ID, owner, notifications.RunDedupeKey(r1.ID, domain.EventMissed))
	d.Event, d.ScheduleID = domain.EventMissed, sc.ID
	if n, err := s.CommitRunOutcome(ctx, r1.ID, []notifications.Delivery{d}, t0()); err != nil || n != 1 {
		t.Errorf("CommitRunOutcome = (%d, %v)", n, err)
	}
	again := d
	again.ID = deliveryID(t, 2)
	if n, err := s.CommitRunOutcome(ctx, r1.ID, []notifications.Delivery{again}, t0()); err != nil || n != 0 {
		t.Errorf("CommitRunOutcome(again) = (%d, %v), want nothing inserted", n, err)
	}
	if os, _ := s.PendingRunOutcomes(ctx, -1); len(os) != 1 || os[0].RunID != r2.ID {
		t.Errorf("pending after the commit = %+v", os)
	}
	_, err = s.CommitRunOutcome(ctx, runID(t, 99), nil, t0())
	wantErr(t, "CommitRunOutcome(missing)", err, domain.ErrNotFound)
}

// planDeliveries plans n deliveries (keys k0..k(n-1)) to contact c for one
// finished job, due at t0 + i seconds.
func planDeliveries(t *testing.T, s Store, c domain.ContactID, n int) []notifications.Delivery {
	t.Helper()
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	j := finishJob(t, s, 1, w.ID, domain.JobFailed, t0(), domain.ScheduleID{})
	ds := make([]notifications.Delivery, n)
	for i := range ds {
		ds[i] = newDelivery(t, i+1, c, ownerID(t), fmt.Sprintf("k%d", i))
		ds[i].JobID, ds[i].NextAttemptAt = j.ID, t0().Add(time.Duration(i)*time.Second)
	}
	if got := commitJob(t, s, j.ID, ds...); got != n {
		t.Fatalf("planned %d deliveries, want %d", got, n)
	}
	return ds
}

func testDeliveryClaimsDisjoint(t *testing.T, s Store) {
	ctx := context.Background()
	owner := ownerID(t)
	requireDeliveries(t, s, owner)
	c := newContact(t, 1, owner, "Ops")
	mustCreateContact(t, s, c)
	ds := planDeliveries(t, s, c.ID, 40)
	now, lease := t0().Add(time.Hour), 2*time.Minute

	first, err := s.ClaimDeliveries(ctx, now, lease, 3)
	if err != nil || !slices.Equal(deliveryIDs(first), deliveryIDs(ds[:3])) {
		t.Fatalf("ClaimDeliveries(3) = (%v, %v), want the earliest due", deliveryIDs(first), err)
	}
	for _, d := range first {
		if d.Status != notifications.StatusSending || d.Attempts != 1 || !d.LeaseUntil.Equal(now.Add(lease)) || !d.LastAttemptAt.Equal(now) ||
			!sameJSON(d.Payload, []byte(testPayload)) {
			t.Errorf("claimed delivery = %+v", d)
		}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[domain.DeliveryID]int{}
	for range 4 {
		wg.Go(func() {
			for {
				got, err := s.ClaimDeliveries(ctx, now, lease, 4)
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
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(seen) != 37 {
		t.Errorf("concurrent claims got %d deliveries, want the other 37", len(seen))
	}
	for id, n := range seen {
		if n != 1 || slices.Contains(deliveryIDs(first), id) {
			t.Errorf("delivery %s claimed %d times", id, n)
		}
	}
}

func testDeliveryLeaseAndComplete(t *testing.T, s Store) {
	ctx := context.Background()
	owner := ownerID(t)
	requireDeliveries(t, s, owner)
	c := newContact(t, 1, owner, "Ops")
	mustCreateContact(t, s, c)
	d := planDeliveries(t, s, c.ID, 1)[0]
	lease := 2 * time.Minute
	claim := func(at time.Time) []notifications.Delivery {
		t.Helper()
		got, err := s.ClaimDeliveries(ctx, at, lease, 10)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := claim(t0().Add(-time.Second)); len(got) != 0 {
		t.Errorf("a delivery that is not due was claimed: %v", got)
	}
	if got := claim(t0()); len(got) != 1 || got[0].Attempts != 1 {
		t.Fatalf("claim = %+v", got)
	}
	if got := claim(t0().Add(time.Minute)); len(got) != 0 {
		t.Errorf("a leased delivery was claimed again: %v", got)
	}
	// The lease expired: another deliverer reclaims it, and the attempt
	// counts.
	re := claim(t0().Add(lease))
	if len(re) != 1 || re[0].Attempts != 2 || !re[0].LeaseUntil.Equal(t0().Add(2*lease)) {
		t.Fatalf("reclaim = %+v", re)
	}
	_, err := s.CompleteDelivery(ctx, notifications.Completion{ID: d.ID, Attempts: 1, Status: notifications.StatusDelivered, At: t0()})
	wantErr(t, "CompleteDelivery(stale claim)", err, domain.ErrConflict)
	retry := t0().Add(10 * time.Minute)
	got, err := s.CompleteDelivery(ctx, notifications.Completion{ID: d.ID, Attempts: 2, Status: notifications.StatusPending,
		NextAttemptAt: retry, LastError: "status 503: unavailable", At: t0().Add(lease)})
	if err != nil || got.Status != notifications.StatusPending || !got.NextAttemptAt.Equal(retry) || !got.LeaseUntil.IsZero() ||
		got.LastError != "status 503: unavailable" || got.Attempts != 2 {
		t.Errorf("CompleteDelivery(retry) = (%+v, %v)", got, err)
	}
	if got := claim(retry.Add(-time.Second)); len(got) != 0 {
		t.Errorf("a delivery waiting for its retry was claimed: %v", got)
	}
	third := claim(retry)
	if len(third) != 1 || third[0].Attempts != 3 {
		t.Fatalf("claim after the backoff = %+v", third)
	}
	at := retry.Add(time.Second)
	got, err = s.CompleteDelivery(ctx, notifications.Completion{ID: d.ID, Attempts: 3, Status: notifications.StatusDelivered, At: at})
	if err != nil || got.Status != notifications.StatusDelivered || !got.DeliveredAt.Equal(at) || got.LastError != "" {
		t.Errorf("CompleteDelivery(delivered) = (%+v, %v)", got, err)
	}
	_, err = s.CompleteDelivery(ctx, notifications.Completion{ID: d.ID, Attempts: 3, Status: notifications.StatusDead, At: at})
	wantErr(t, "CompleteDelivery(completed)", err, domain.ErrConflict)
	if got := claim(at.Add(time.Hour)); len(got) != 0 {
		t.Errorf("a delivered delivery was claimed: %v", got)
	}
	_, err = s.CompleteDelivery(ctx, notifications.Completion{ID: deliveryID(t, 99), Attempts: 1, Status: notifications.StatusDead, At: at})
	wantErr(t, "CompleteDelivery(missing)", err, domain.ErrNotFound)
}

func testDeliveryRetryListPurge(t *testing.T, s Store) {
	ctx := context.Background()
	owner := ownerID(t)
	requireDeliveries(t, s, owner)
	c1, c2 := newContact(t, 1, owner, "Ops"), newContact(t, 2, owner, "Mail")
	mustCreateContact(t, s, c1)
	mustCreateContact(t, s, c2)
	ds := planDeliveries(t, s, c1.ID, 3)
	w := newWorker(t, 2)
	mustCreateWorker(t, s, w)
	j := finishJob(t, s, 2, w.ID, domain.JobFailed, t0(), domain.ScheduleID{})
	onMail := newDelivery(t, 10, c2.ID, owner, "mail")
	onMail.JobID, onMail.CreatedAt, onMail.NextAttemptAt = j.ID, t0().Add(time.Hour), t0().Add(time.Hour)
	commitJob(t, s, j.ID, onMail)

	// ds[0] dies, ds[1] is delivered, ds[2] stays pending.
	claimed, err := s.ClaimDeliveries(ctx, t0().Add(time.Second), time.Minute, 2)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("ClaimDeliveries = (%v, %v)", deliveryIDs(claimed), err)
	}
	if _, err := s.CompleteDelivery(ctx, notifications.Completion{ID: ds[0].ID, Attempts: 1, Status: notifications.StatusDead, LastError: "404", At: t0()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteDelivery(ctx, notifications.Completion{ID: ds[1].ID, Attempts: 1, Status: notifications.StatusDelivered, At: t0()}); err != nil {
		t.Fatal(err)
	}

	_, err = s.RetryDelivery(ctx, ds[1].ID, t0())
	wantErr(t, "RetryDelivery(delivered)", err, domain.ErrFailedPrecondition)
	_, err = s.RetryDelivery(ctx, deliveryID(t, 99), t0())
	wantErr(t, "RetryDelivery(missing)", err, domain.ErrNotFound)
	at := t0().Add(time.Hour)
	got, err := s.RetryDelivery(ctx, ds[0].ID, at)
	if err != nil || got.Status != notifications.StatusPending || got.Attempts != 0 || !got.NextAttemptAt.Equal(at) || got.LastError != "404" {
		t.Errorf("RetryDelivery = (%+v, %v)", got, err)
	}
	if got, err := s.GetDelivery(ctx, ds[0].ID); err != nil || got.Status != notifications.StatusPending {
		t.Errorf("GetDelivery = (%+v, %v)", got, err)
	}
	_, err = s.GetDelivery(ctx, deliveryID(t, 99))
	wantErr(t, "GetDelivery(missing)", err, domain.ErrNotFound)

	list := func(f notifications.DeliveryFilter) []domain.DeliveryID {
		t.Helper()
		got, err := s.ListDeliveries(ctx, f)
		if err != nil {
			t.Fatalf("ListDeliveries(%+v): %v", f, err)
		}
		return deliveryIDs(got)
	}
	all := []domain.DeliveryID{onMail.ID, ds[2].ID, ds[1].ID, ds[0].ID}
	for name, tt := range map[string]struct {
		f    notifications.DeliveryFilter
		want []domain.DeliveryID
	}{
		"all":     {notifications.DeliveryFilter{Limit: -1}, all},
		"owner":   {notifications.DeliveryFilter{OwnerID: owner, Limit: -1}, all},
		"other":   {notifications.DeliveryFilter{OwnerID: userID(t, 99), Limit: -1}, nil},
		"contact": {notifications.DeliveryFilter{ContactID: c1.ID, Limit: -1}, []domain.DeliveryID{ds[2].ID, ds[1].ID, ds[0].ID}},
		"pending": {notifications.DeliveryFilter{Statuses: []notifications.Status{notifications.StatusPending}, Limit: -1}, []domain.DeliveryID{onMail.ID, ds[2].ID, ds[0].ID}},
		"page":    {notifications.DeliveryFilter{Before: onMail.ID, Limit: 2}, []domain.DeliveryID{ds[2].ID, ds[1].ID}},
	} {
		if got := list(tt.f); !slices.Equal(got, tt.want) {
			t.Errorf("ListDeliveries(%s) = %v, want %v", name, got, tt.want)
		}
	}

	// Purge removes finished deliveries created before the cutoff only.
	n, err := s.PurgeDeliveries(ctx, t0().Add(time.Second))
	if err != nil || n != 1 {
		t.Errorf("PurgeDeliveries = (%d, %v), want the delivered one", n, err)
	}
	if got := list(notifications.DeliveryFilter{Limit: -1}); !slices.Equal(got, []domain.DeliveryID{onMail.ID, ds[2].ID, ds[0].ID}) {
		t.Errorf("after the purge: %v", got)
	}
	// Deleting a contact deletes its deliveries.
	if err := s.DeleteContact(ctx, c2.ID); err != nil {
		t.Fatal(err)
	}
	if got := list(notifications.DeliveryFilter{Limit: -1}); !slices.Equal(got, []domain.DeliveryID{ds[2].ID, ds[0].ID}) {
		t.Errorf("after deleting a contact: %v", got)
	}
	// Returned payloads do not alias the store.
	got, _ = s.GetDelivery(ctx, ds[2].ID)
	got.Payload[0] = 'x'
	if again, _ := s.GetDelivery(ctx, ds[2].ID); !sameJSON(again.Payload, []byte(testPayload)) {
		t.Error("GetDelivery returns storage")
	}
}
