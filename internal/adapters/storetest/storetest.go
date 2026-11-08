// Package storetest is the contract test suite of the controller's
// repository ports (workers.Repository, workers.TokenRepository,
// jobs.Repository, jobs.SampleRepository, jobs.ActivityRepository,
// users.Repository, users.SessionRepository, users.APITokenRepository,
// remotes.Repository, schedules.Repository, contacts.Repository,
// notifications.Repository).
// Every store adapter runs it from its tests:
//
//	func TestContract(t *testing.T) {
//		storetest.Run(t, func(t *testing.T) storetest.Store { return newStore(t) })
//	}
//
// It checks the documented semantics, above all the conditional updates the
// use cases rely on for concurrency safety (compare-and-swap epoch,
// monotonic cursors, single-use tokens, version-checked updates, the first
// user race), the referential rules (owners, union members, soft delete),
// and that returned values do not alias the store.
//
// A suite whose port an adapter does not implement yet (its methods return
// a wrapped errors.ErrUnsupported) is skipped, naming the pending work.
//
// It is test support, not an adapter: it imports only the standard library,
// internal/domain, internal/backend (for its value types) and the app
// packages whose ports it exercises.
package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/contacts"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/notifications"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Store is everything a controller store adapter implements.
type Store interface {
	workers.Repository
	workers.TokenRepository
	jobs.Repository
	jobs.SampleRepository
	jobs.ActivityRepository
	users.Repository
	users.SessionRepository
	users.APITokenRepository
	remotes.Repository
	schedules.Repository
	contacts.Repository
	notifications.Repository
}

// Run runs the suite; open returns a new, empty store for each subtest.
func Run(t *testing.T, open func(t *testing.T) Store) {
	t.Helper()
	tests := []struct {
		name string
		fn   func(*testing.T, Store)
	}{
		{"WorkerCreateGetList", testWorkerCreateGetList},
		{"WorkerRenameRemove", testWorkerRenameRemove},
		{"WorkerEpochCAS", testWorkerEpochCAS},
		{"WorkerAckSeqMonotonic", testWorkerAckSeqMonotonic},
		{"WorkerInfoAndTouch", testWorkerInfoAndTouch},
		{"WorkerConcurrentLinkUpdates", testWorkerConcurrentLinkUpdates},
		{"EnrollmentTokens", testEnrollmentTokens},
		{"EnrollmentTokenSingleUseUnderRace", testEnrollmentTokenRace},
		{"JobCreateGet", testJobCreateGet},
		{"JobUpdateVersioned", testJobUpdateVersioned},
		{"JobUpdateRace", testJobUpdateRace},
		{"JobLists", testJobLists},
		{"JobListFilters", testJobListFilters},
		{"JobQueryIsLiteral", testJobQueryIsLiteral},
		{"JobRemotes", testJobRemotes},
		{"Samples", testSamples},
		{"NoAliasing", testNoAliasing},

		{"UserCreateGetList", testUserCreateGetList},
		{"UserUpdateVersioned", testUserUpdateVersioned},
		{"UserFirstUserRace", testUserFirstUserRace},
		{"UserDelete", testUserDelete},
		{"UserDeleteOwner", testUserDeleteOwner},
		{"Sessions", testSessions},
		{"APITokens", testAPITokens},

		{"RemoteCreateGetList", testRemoteCreateGetList},
		{"RemoteNamesUniqueAmongLive", testRemoteNamesUniqueAmongLive},
		{"RemoteUpdateVersioned", testRemoteUpdateVersioned},
		{"RemoteUnionMembers", testRemoteUnionMembers},
		{"RemoteSoftDelete", testRemoteSoftDelete},
		{"RemoteNoAliasing", testRemoteNoAliasing},

		// Milestone 4.
		{"WorkerNamesUniqueAmongLive", testWorkerNamesUniqueAmongLive},
		{"WorkerLabels", testWorkerLabels},
		{"APITokenPurge", testAPITokenPurge},

		{"ScheduleCreateGetList", testScheduleCreateGetList},
		{"ScheduleReferences", testScheduleReferences},
		{"ScheduleUpdateVersioned", testScheduleUpdateVersioned},
		{"ScheduleSetEnabled", testScheduleSetEnabled},
		{"ScheduleFireCAS", testScheduleFireCAS},
		{"ScheduleFireRace", testScheduleFireRace},
		{"ScheduleMergeOpenRun", testScheduleMergeOpenRun},
		{"ScheduleRunCAS", testScheduleRunCAS},
		{"ScheduleHasActiveRun", testScheduleHasActiveRun},
		{"ScheduleLists", testScheduleLists},
		{"ScheduleDeleteCascades", testScheduleDeleteCascades},
		{"ScheduleNoAliasing", testScheduleNoAliasing},

		{"ContactCreateGetList", testContactCreateGetList},
		{"ContactUpdateVersioned", testContactUpdateVersioned},
		{"ContactSchedulesAndDelete", testContactSchedulesAndDelete},
		{"ContactNoAliasing", testContactNoAliasing},

		{"DeliveryPlanJobOnce", testDeliveryPlanJobOnce},
		{"DeliverySubscribers", testDeliverySubscribers},
		{"DeliveryPlanRun", testDeliveryPlanRun},
		{"DeliveryClaimsDisjoint", testDeliveryClaimsDisjoint},
		{"DeliveryLeaseAndComplete", testDeliveryLeaseAndComplete},
		{"DeliveryRetryListPurge", testDeliveryRetryListPurge},

		{"ActivityBuckets", testActivityBuckets},
		{"ActivityCountUnfinished", testActivityCountUnfinished},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.fn(t, open(t))
		})
	}
}

// t0() is the base time of the suite (microsecond precision, like the clocks).
func t0() time.Time { return time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC) }

func id[K any](t *testing.T, parse func(string) (domain.ID[K], error), n int) domain.ID[K] {
	t.Helper()
	v, err := parse(fmt.Sprintf("00000000-0000-7000-8000-%012x", n))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func workerID(t *testing.T, n int) domain.WorkerID { return id(t, domain.ParseWorkerID, n) }
func jobID(t *testing.T, n int) domain.JobID       { return id(t, domain.ParseJobID, 1000+n) }
func userID(t *testing.T, n int) domain.UserID     { return id(t, domain.ParseUserID, 2000+n) }
func remoteID(t *testing.T, n int) domain.RemoteID { return id(t, domain.ParseRemoteID, 3000+n) }

func scheduleID(t *testing.T, n int) domain.ScheduleID { return id(t, domain.ParseScheduleID, 5000+n) }
func contactID(t *testing.T, n int) domain.ContactID   { return id(t, domain.ParseContactID, 6000+n) }
func runID(t *testing.T, n int) domain.ScheduleRunID   { return id(t, domain.ParseScheduleRunID, 7000+n) }
func deliveryID(t *testing.T, n int) domain.DeliveryID { return id(t, domain.ParseDeliveryID, 8000+n) }

// skipIfUnsupported skips the test when err says the adapter does not
// implement the port yet (a stub returning errors.ErrUnsupported), naming
// the pending work; any other error fails it.
func skipIfUnsupported(t *testing.T, what, pending string, err error) {
	t.Helper()
	switch {
	case err == nil:
	case errors.Is(err, errors.ErrUnsupported):
		t.Skipf("m4: pending %s: %s is not implemented by this store (%v)", pending, what, err)
	default:
		t.Fatalf("%s: %v", what, err)
	}
}

// testDefinition is the encoded definition of test jobs (jobs store it as
// JSON; databases may normalize its whitespace).
const testDefinition = `{"rclone_transfer": {"mode": "RCLONE_TRANSFER_MODE_COPY"}}`

// sameJSON reports whether a and b are equal JSON documents.
func sameJSON(a, b []byte) bool {
	var va, vb any
	return json.Unmarshal(a, &va) == nil && json.Unmarshal(b, &vb) == nil && reflect.DeepEqual(va, vb)
}

// ownerID is the owner of test jobs; mustCreateJob creates it.
func ownerID(t *testing.T) domain.UserID { return userID(t, 1) }

func newWorker(t *testing.T, n int) workers.Worker {
	return workers.Worker{
		ID: workerID(t, n), Name: fmt.Sprintf("w%d", n), Mode: domain.WorkerActive,
		SecretEnc: []byte("enc"), CreatedAt: t0(),
	}
}

func mustCreateWorker(t *testing.T, s Store, w workers.Worker) {
	t.Helper()
	if err := s.CreateWorker(context.Background(), w); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
}

func newJob(t *testing.T, n int, w domain.WorkerID) jobs.Job {
	return jobs.Job{
		ID: jobID(t, n), WorkerID: w, OwnerID: ownerID(t), Kind: "rclone_transfer", Status: domain.JobQueued,
		Definition: []byte(testDefinition), CreatedAt: t0(), QueuedAt: t0(), ExitCode: -1, Version: 1,
	}
}

// mustCreateJob creates j, and its owner first if needed.
func mustCreateJob(t *testing.T, s Store, j jobs.Job) {
	t.Helper()
	ensureUser(t, s, j.OwnerID)
	if err := s.CreateJob(context.Background(), j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
}

// ensureUser creates a user with id unless it exists.
func ensureUser(t *testing.T, s Store, id domain.UserID) {
	t.Helper()
	u := newUser(t, 0)
	u.ID, u.Username = id, "owner-"+id.String()[len(id.String())-6:]
	err := s.CreateUser(context.Background(), u)
	if err != nil && !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("CreateUser(%s): %v", id, err)
	}
}

func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: err = %v, want %v", what, err, want)
	}
}

func testWorkerCreateGetList(t *testing.T, s Store) {
	ctx := context.Background()
	a := newWorker(t, 2)
	a.Info = workers.Info{Version: "v2", Tools: []workers.Tool{{Name: "rclone", Version: "v1"}}, JobKinds: []string{"k"}, LocalRoots: []string{"/data"}}
	b := newWorker(t, 1)
	b.Mode, b.URL, b.CertFingerprint = domain.WorkerPassive, "https://w", "ab"
	mustCreateWorker(t, s, a)
	mustCreateWorker(t, s, b)
	wantErr(t, "duplicate CreateWorker", s.CreateWorker(ctx, a), domain.ErrAlreadyExists)

	got, err := s.GetWorker(ctx, a.ID)
	if err != nil || got.Name != a.Name || !slices.Equal(got.Info.LocalRoots, a.Info.LocalRoots) || got.Info.Tools[0] != a.Info.Tools[0] || !got.CreatedAt.Equal(t0()) {
		t.Errorf("GetWorker = (%+v, %v)", got, err)
	}
	_, err = s.GetWorker(ctx, workerID(t, 99))
	wantErr(t, "GetWorker(missing)", err, domain.ErrNotFound)

	all, err := s.ListWorkers(ctx, workers.ListFilter{})
	if err != nil || len(all) != 2 || all[0].ID != b.ID || all[1].ID != a.ID {
		t.Errorf("ListWorkers = (%v, %v), want ID order", all, err)
	}
	passive, _ := s.ListWorkers(ctx, workers.ListFilter{Mode: domain.WorkerPassive})
	if len(passive) != 1 || passive[0].ID != b.ID {
		t.Errorf("ListWorkers(passive) = %v", passive)
	}
}

func testWorkerRenameRemove(t *testing.T, s Store) {
	ctx := context.Background()
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	got, err := s.RenameWorker(ctx, w.ID, "renamed")
	if err != nil || got.Name != "renamed" {
		t.Errorf("RenameWorker = (%v, %v)", got.Name, err)
	}
	_, err = s.RenameWorker(ctx, workerID(t, 9), "x")
	wantErr(t, "RenameWorker(missing)", err, domain.ErrNotFound)

	removed, err := s.RemoveWorker(ctx, w.ID, t0().Add(time.Hour))
	if err != nil || !removed.RemovedAt.Equal(t0().Add(time.Hour)) || removed.SecretEnc != nil {
		t.Errorf("RemoveWorker = (%+v, %v): want RemovedAt set and secret wiped", removed, err)
	}
	_, err = s.RemoveWorker(ctx, w.ID, t0())
	wantErr(t, "RemoveWorker(removed)", err, domain.ErrNotFound)
	if got, err := s.GetWorker(ctx, w.ID); err != nil || !got.Removed() {
		t.Errorf("GetWorker(removed) = (%+v, %v): want the removed worker", got, err)
	}
	if ws, _ := s.ListWorkers(ctx, workers.ListFilter{}); len(ws) != 0 {
		t.Errorf("ListWorkers lists a removed worker: %v", ws)
	}
	if ws, _ := s.ListWorkers(ctx, workers.ListFilter{IncludeRemoved: true}); len(ws) != 1 {
		t.Errorf("ListWorkers(IncludeRemoved) = %v", ws)
	}
	// Removed workers are missing for every mutation.
	_, err = s.RenameWorker(ctx, w.ID, "x")
	wantErr(t, "RenameWorker(removed)", err, domain.ErrNotFound)
	_, err = s.AdvanceWorkerEpoch(ctx, w.ID, "", workers.NewEpoch{Epoch: "e"})
	wantErr(t, "AdvanceWorkerEpoch(removed)", err, domain.ErrNotFound)
	_, err = s.AdvanceWorkerAckSeq(ctx, w.ID, "", 1)
	wantErr(t, "AdvanceWorkerAckSeq(removed)", err, domain.ErrNotFound)
	wantErr(t, "SetWorkerInfo(removed)", s.SetWorkerInfo(ctx, w.ID, "", workers.Info{}), domain.ErrNotFound)
	wantErr(t, "TouchWorker(removed)", s.TouchWorker(ctx, w.ID, t0()), domain.ErrNotFound)
	// And jobs cannot be created for them.
	wantErr(t, "CreateJob(removed worker)", s.CreateJob(ctx, newJob(t, 1, w.ID)), domain.ErrNotFound)
}

func testWorkerEpochCAS(t *testing.T, s Store) {
	ctx := context.Background()
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	got, err := s.AdvanceWorkerEpoch(ctx, w.ID, "", workers.NewEpoch{Epoch: "e1", StartedAt: t0()})
	if err != nil || got.Link.Epoch != "e1" || !got.Link.EpochStartedAt.Equal(t0()) {
		t.Fatalf("AdvanceWorkerEpoch(''->e1) = (%+v, %v)", got.Link, err)
	}
	if _, err := s.AdvanceWorkerAckSeq(ctx, w.ID, "e1", 7); err != nil {
		t.Fatal(err)
	}
	// Wrong "from": conflict, nothing changes.
	_, err = s.AdvanceWorkerEpoch(ctx, w.ID, "", workers.NewEpoch{Epoch: "e2"})
	wantErr(t, "AdvanceWorkerEpoch(stale from)", err, domain.ErrConflict)
	got, _ = s.GetWorker(ctx, w.ID)
	if got.Link.Epoch != "e1" || got.Link.AckSeq != 7 {
		t.Errorf("after a failed CAS: link = %+v", got.Link)
	}
	got, err = s.AdvanceWorkerEpoch(ctx, w.ID, "e1", workers.NewEpoch{Epoch: "e2", StartedAt: t0().Add(time.Minute), PastEpochs: []string{"e1"}})
	if err != nil || got.Link.Epoch != "e2" || got.Link.AckSeq != 0 || !slices.Equal(got.Link.PastEpochs, []string{"e1"}) {
		t.Errorf("AdvanceWorkerEpoch(e1->e2) = (%+v, %v): want AckSeq reset and history", got.Link, err)
	}
	_, err = s.AdvanceWorkerEpoch(ctx, workerID(t, 9), "", workers.NewEpoch{Epoch: "x"})
	wantErr(t, "AdvanceWorkerEpoch(missing)", err, domain.ErrNotFound)
}

func testWorkerAckSeqMonotonic(t *testing.T, s Store) {
	ctx := context.Background()
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	if _, err := s.AdvanceWorkerEpoch(ctx, w.ID, "", workers.NewEpoch{Epoch: "e"}); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct{ seq, want int64 }{{5, 5}, {3, 5}, {5, 5}, {9, 9}, {0, 9}} {
		got, err := s.AdvanceWorkerAckSeq(ctx, w.ID, "e", step.seq)
		if err != nil || got != step.want {
			t.Errorf("AdvanceWorkerAckSeq(%d) = (%d, %v), want %d", step.seq, got, err, step.want)
		}
	}
	_, err := s.AdvanceWorkerAckSeq(ctx, w.ID, "other", 100)
	wantErr(t, "AdvanceWorkerAckSeq(other epoch)", err, domain.ErrConflict)
	if got, _ := s.GetWorker(ctx, w.ID); got.Link.AckSeq != 9 {
		t.Errorf("AckSeq = %d after a wrong-epoch call, want 9", got.Link.AckSeq)
	}
}

func testWorkerInfoAndTouch(t *testing.T, s Store) {
	ctx := context.Background()
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	if _, err := s.AdvanceWorkerEpoch(ctx, w.ID, "", workers.NewEpoch{Epoch: "e"}); err != nil {
		t.Fatal(err)
	}
	info := workers.Info{Version: "v2", OS: "linux", JobKinds: []string{"a", "b"}, MaxConcurrentJobs: 2, ReportedAt: t0()}
	if err := s.SetWorkerInfo(ctx, w.ID, "e", info); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "SetWorkerInfo(other epoch)", s.SetWorkerInfo(ctx, w.ID, "old", workers.Info{Version: "old"}), domain.ErrConflict)
	got, _ := s.GetWorker(ctx, w.ID)
	if got.Info.Version != "v2" || !slices.Equal(got.Info.JobKinds, []string{"a", "b"}) || got.Info.MaxConcurrentJobs != 2 {
		t.Errorf("Info = %+v", got.Info)
	}

	for _, at := range []time.Time{t0().Add(time.Minute), t0(), t0().Add(2 * time.Minute)} {
		if err := s.TouchWorker(ctx, w.ID, at); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := s.GetWorker(ctx, w.ID); !got.Link.LastSeen.Equal(t0().Add(2 * time.Minute)) {
		t.Errorf("LastSeen = %v, want the maximum", got.Link.LastSeen)
	}
	wantErr(t, "TouchWorker(missing)", s.TouchWorker(ctx, workerID(t, 9), t0()), domain.ErrNotFound)
}

func testWorkerConcurrentLinkUpdates(t *testing.T, s Store) {
	ctx := context.Background()
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	if _, err := s.AdvanceWorkerEpoch(ctx, w.ID, "", workers.NewEpoch{Epoch: "e"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 1; i <= 50; i++ {
		wg.Go(func() {
			if _, err := s.AdvanceWorkerAckSeq(ctx, w.ID, "e", int64(i)); err != nil {
				t.Error(err)
			}
			if err := s.TouchWorker(ctx, w.ID, t0().Add(time.Duration(i)*time.Second)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	got, _ := s.GetWorker(ctx, w.ID)
	if got.Link.AckSeq != 50 || !got.Link.LastSeen.Equal(t0().Add(50*time.Second)) {
		t.Errorf("after concurrent advances: AckSeq %d, LastSeen %v; want the maxima", got.Link.AckSeq, got.Link.LastSeen)
	}

	// Concurrent epoch CAS from the same "from": exactly one wins.
	var wins sync.WaitGroup
	won := make(chan string, 10)
	for i := range 10 {
		wins.Go(func() {
			if _, err := s.AdvanceWorkerEpoch(ctx, w.ID, "e", workers.NewEpoch{Epoch: fmt.Sprintf("n%d", i)}); err == nil {
				won <- fmt.Sprintf("n%d", i)
			} else if !errors.Is(err, domain.ErrConflict) {
				t.Error(err)
			}
		})
	}
	wins.Wait()
	close(won)
	if n := len(won); n != 1 {
		t.Fatalf("%d concurrent epoch CAS calls succeeded, want 1", n)
	}
	winner := <-won
	if got, _ := s.GetWorker(ctx, w.ID); got.Link.Epoch != winner || got.Link.AckSeq != 0 {
		t.Errorf("after the CAS race: epoch %q ack %d, want %q and 0", got.Link.Epoch, got.Link.AckSeq, winner)
	}
}

func testEnrollmentTokens(t *testing.T, s Store) {
	ctx := context.Background()
	h1, h2, h3 := workers.TokenHash{1}, workers.TokenHash{2}, workers.TokenHash{3}
	for _, tok := range []workers.EnrollmentToken{
		{Hash: h1, CreatedAt: t0(), ExpiresAt: t0().Add(time.Hour)},
		{Hash: h2, CreatedAt: t0(), ExpiresAt: t0().Add(time.Minute)},
		{Hash: h3, CreatedAt: t0(), ExpiresAt: t0().Add(48 * time.Hour)},
	} {
		if err := s.CreateEnrollmentToken(ctx, tok); err != nil {
			t.Fatal(err)
		}
	}
	wantErr(t, "duplicate token", s.CreateEnrollmentToken(ctx, workers.EnrollmentToken{Hash: h1}), domain.ErrAlreadyExists)

	by := workerID(t, 1)
	got, err := s.ConsumeEnrollmentToken(ctx, h1, t0().Add(time.Minute), by)
	if err != nil || !got.UsedAt.Equal(t0().Add(time.Minute)) || got.UsedBy != by {
		t.Errorf("ConsumeEnrollmentToken = (%+v, %v)", got, err)
	}
	_, err = s.ConsumeEnrollmentToken(ctx, h1, t0().Add(2*time.Minute), by)
	wantErr(t, "second use", err, domain.ErrNotFound)
	_, err = s.ConsumeEnrollmentToken(ctx, h2, t0().Add(time.Minute), by) // at == ExpiresAt
	wantErr(t, "expired token", err, domain.ErrNotFound)
	_, err = s.ConsumeEnrollmentToken(ctx, workers.TokenHash{9}, t0(), by)
	wantErr(t, "unknown token", err, domain.ErrNotFound)

	n, err := s.DeleteEnrollmentTokens(ctx, t0().Add(2*time.Hour))
	if err != nil || n != 2 {
		t.Errorf("DeleteEnrollmentTokens = (%d, %v), want 2 (h1 used, h2 expired)", n, err)
	}
	if _, err := s.ConsumeEnrollmentToken(ctx, h3, t0().Add(3*time.Hour), by); err != nil {
		t.Errorf("token h3 should survive the purge: %v", err)
	}
}

func testEnrollmentTokenRace(t *testing.T, s Store) {
	ctx := context.Background()
	h := workers.TokenHash{7}
	if err := s.CreateEnrollmentToken(ctx, workers.EnrollmentToken{Hash: h, CreatedAt: t0(), ExpiresAt: t0().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := range 20 {
		wg.Go(func() {
			if _, err := s.ConsumeEnrollmentToken(ctx, h, t0(), workerID(t, i+1)); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d concurrent redemptions succeeded, want 1", wins)
	}
}

func testJobCreateGet(t *testing.T, s Store) {
	ctx := context.Background()
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	j := newJob(t, 1, w.ID)
	j.SearchText = "rclone_transfer\ncopy\n/data"
	mustCreateJob(t, s, j)
	wantErr(t, "duplicate CreateJob", s.CreateJob(ctx, j), domain.ErrAlreadyExists)
	wantErr(t, "CreateJob(unknown worker)", s.CreateJob(ctx, newJob(t, 2, workerID(t, 9))), domain.ErrNotFound)
	orphan := newJob(t, 3, w.ID)
	orphan.OwnerID = userID(t, 99)
	wantErr(t, "CreateJob(unknown owner)", s.CreateJob(ctx, orphan), domain.ErrNotFound)
	if _, err := s.GetJob(ctx, orphan.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a job without owner was stored: %v", err)
	}
	got, err := s.GetJob(ctx, j.ID)
	if err != nil || got.Status != domain.JobQueued || got.Version != 1 || got.OwnerID != ownerID(t) ||
		!sameJSON(got.Definition, []byte(testDefinition)) || got.ExitCode != -1 || !got.QueuedAt.Equal(t0()) ||
		got.SearchText != j.SearchText || len(got.RemoteIDs) != 0 {
		t.Errorf("GetJob = (%+v, %v)", got, err)
	}
	_, err = s.GetJob(ctx, jobID(t, 99))
	wantErr(t, "GetJob(missing)", err, domain.ErrNotFound)
}

func testJobUpdateVersioned(t *testing.T, s Store) {
	ctx := context.Background()
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	j := newJob(t, 1, w.ID)
	mustCreateJob(t, s, j)

	next := j
	next.Status, next.Epoch, next.DispatchedAt = domain.JobDispatched, "e", t0().Add(time.Second)
	next.Stats = jobs.Stats{Bytes: 1 << 40, Elapsed: time.Minute}
	next.Error = jobs.JobError{Code: "c", Message: "m"}
	next.Warnings, next.Output, next.ExitCode, next.Requeues = []string{"w"}, []byte{1}, 3, 2
	next.CancelRequestedAt = t0().Add(2 * time.Second)
	// Immutable fields are never written.
	next.Kind, next.Definition, next.CreatedAt = "other", []byte(`{"other": true}`), t0().Add(time.Hour)
	next.OwnerID, next.SearchText, next.RemoteIDs = userID(t, 99), "other", []domain.RemoteID{remoteID(t, 99)}
	stored, err := s.UpdateJob(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != 2 || stored.Status != domain.JobDispatched || stored.Epoch != "e" || stored.Stats.Bytes != 1<<40 ||
		stored.Error.Code != "c" || stored.Requeues != 2 || stored.ExitCode != 3 || !stored.CancelRequestedAt.Equal(t0().Add(2*time.Second)) {
		t.Errorf("UpdateJob returned %+v", stored)
	}
	if stored.Kind != "rclone_transfer" || !sameJSON(stored.Definition, []byte(testDefinition)) || stored.OwnerID != ownerID(t) || !stored.CreatedAt.Equal(t0()) ||
		stored.SearchText != "" || len(stored.RemoteIDs) != 0 {
		t.Errorf("UpdateJob wrote immutable fields: %+v", stored)
	}
	// Stale version: conflict, nothing written.
	stale := j
	stale.Status = domain.JobCancelled
	_, err = s.UpdateJob(ctx, stale)
	wantErr(t, "UpdateJob(stale version)", err, domain.ErrConflict)
	if got, _ := s.GetJob(ctx, j.ID); got.Status != domain.JobDispatched || got.Version != 2 {
		t.Errorf("after a stale update: %s v%d", got.Status, got.Version)
	}
	missing := newJob(t, 9, w.ID)
	_, err = s.UpdateJob(ctx, missing)
	wantErr(t, "UpdateJob(missing)", err, domain.ErrNotFound)
}

func testJobUpdateRace(t *testing.T, s Store) {
	ctx := context.Background()
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	j := newJob(t, 1, w.ID)
	mustCreateJob(t, s, j)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := range 20 {
		wg.Go(func() {
			next := j
			next.Requeues = i
			if _, err := s.UpdateJob(ctx, next); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			} else if !errors.Is(err, domain.ErrConflict) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d concurrent updates of version 1 succeeded, want 1", wins)
	}
}

func testJobLists(t *testing.T, s Store) {
	ctx := context.Background()
	w1, w2 := newWorker(t, 1), newWorker(t, 2)
	mustCreateWorker(t, s, w1)
	mustCreateWorker(t, s, w2)
	statuses := []domain.JobStatus{domain.JobQueued, domain.JobDispatched, domain.JobRunning, domain.JobSucceeded, domain.JobQueued}
	for i, st := range statuses {
		j := newJob(t, i+1, w1.ID)
		if i == 4 {
			j.WorkerID = w2.ID
		}
		j.QueuedAt = t0().Add(time.Duration(10-i) * time.Minute)
		mustCreateJob(t, s, j)
		if st != domain.JobQueued {
			j.Status = st
			if _, err := s.UpdateJob(ctx, j); err != nil {
				t.Fatal(err)
			}
		}
	}
	ids := func(js []jobs.Job) []domain.JobID {
		out := make([]domain.JobID, len(js))
		for i, j := range js {
			out[i] = j.ID
		}
		return out
	}

	all, err := s.ListJobs(ctx, jobs.ListFilter{Limit: 10})
	if err != nil || !slices.Equal(ids(all), []domain.JobID{jobID(t, 5), jobID(t, 4), jobID(t, 3), jobID(t, 2), jobID(t, 1)}) {
		t.Errorf("ListJobs = %v (%v), want ID descending", ids(all), err)
	}
	page, _ := s.ListJobs(ctx, jobs.ListFilter{Limit: 2, Before: jobID(t, 4)})
	if !slices.Equal(ids(page), []domain.JobID{jobID(t, 3), jobID(t, 2)}) {
		t.Errorf("ListJobs(page) = %v", ids(page))
	}
	byWorker, _ := s.ListJobs(ctx, jobs.ListFilter{Limit: 10, WorkerID: w2.ID})
	if !slices.Equal(ids(byWorker), []domain.JobID{jobID(t, 5)}) {
		t.Errorf("ListJobs(worker) = %v", ids(byWorker))
	}
	byStatus, _ := s.ListJobs(ctx, jobs.ListFilter{Limit: 10, Statuses: []domain.JobStatus{domain.JobRunning, domain.JobSucceeded}})
	if !slices.Equal(ids(byStatus), []domain.JobID{jobID(t, 4), jobID(t, 3)}) {
		t.Errorf("ListJobs(statuses) = %v", ids(byStatus))
	}

	unfinished, _ := s.ListUnfinishedJobs(ctx, w1.ID)
	if !slices.Equal(ids(unfinished), []domain.JobID{jobID(t, 1), jobID(t, 2), jobID(t, 3)}) {
		t.Errorf("ListUnfinishedJobs = %v, want queued/dispatched/running of w1, ID ascending", ids(unfinished))
	}

	// Queued: job 1 (QueuedAt t0()+10m) and job 5 (t0()+6m); ordered by QueuedAt.
	queued, _ := s.ListQueuedJobs(ctx, t0().Add(time.Hour), 10)
	if !slices.Equal(ids(queued), []domain.JobID{jobID(t, 5), jobID(t, 1)}) {
		t.Errorf("ListQueuedJobs = %v", ids(queued))
	}
	queued, _ = s.ListQueuedJobs(ctx, t0().Add(10*time.Minute), 10) // exclusive bound
	if !slices.Equal(ids(queued), []domain.JobID{jobID(t, 5)}) {
		t.Errorf("ListQueuedJobs(before) = %v", ids(queued))
	}
	queued, _ = s.ListQueuedJobs(ctx, t0().Add(time.Hour), 1)
	if len(queued) != 1 {
		t.Errorf("ListQueuedJobs(limit 1) = %v", ids(queued))
	}
}

func jobIDs(js []jobs.Job) []domain.JobID {
	out := make([]domain.JobID, len(js))
	for i, j := range js {
		out[i] = j.ID
	}
	return out
}

// testJobListFilters covers the filters that need no remotes: owner,
// kinds, query and the creation time range, alone and combined with the
// others and the keyset cursor.
func testJobListFilters(t *testing.T, s Store) {
	ctx := context.Background()
	w1, w2 := newWorker(t, 1), newWorker(t, 2)
	mustCreateWorker(t, s, w1)
	mustCreateWorker(t, s, w2)
	alice, bob := ownerID(t), userID(t, 2)
	ensureUser(t, s, bob)
	type spec struct {
		owner  domain.UserID
		worker domain.WorkerID
		kind   string
		status domain.JobStatus
		text   string
	}
	specs := []spec{
		{alice, w1.ID, "rclone_transfer", domain.JobSucceeded, "rclone_transfer\nsync\n/srv/photos\nphotos"},
		{bob, w1.ID, "rclone_transfer", domain.JobFailed, "rclone_transfer\ncopy\n/home/bob\nbob's nas"},
		{alice, w2.ID, "restic_backup", domain.JobSucceeded, "restic_backup\n/srv/photos"},
		{alice, w1.ID, "rclone_transfer", domain.JobQueued, "rclone_transfer\nmove\ninbox\narchive"},
		{bob, w2.ID, "restic_check", domain.JobQueued, "restic_check"},
	}
	for i, sp := range specs {
		j := newJob(t, i+1, sp.worker)
		j.OwnerID, j.Kind, j.SearchText = sp.owner, sp.kind, sp.text
		j.CreatedAt = t0().Add(time.Duration(i) * time.Minute)
		mustCreateJob(t, s, j)
		if sp.status != domain.JobQueued {
			j.Status = sp.status
			if _, err := s.UpdateJob(ctx, j); err != nil {
				t.Fatal(err)
			}
		}
	}
	n := func(is ...int) []domain.JobID {
		out := make([]domain.JobID, len(is))
		for i, x := range is {
			out[i] = jobID(t, x)
		}
		return out
	}
	tests := []struct {
		name string
		f    jobs.ListFilter
		want []domain.JobID
	}{
		{"everyone", jobs.ListFilter{}, n(5, 4, 3, 2, 1)},
		{"owner", jobs.ListFilter{OwnerID: alice}, n(4, 3, 1)},
		{"other owner", jobs.ListFilter{OwnerID: bob}, n(5, 2)},
		{"owner without jobs", jobs.ListFilter{OwnerID: userID(t, 77)}, nil},
		{"AllUsers is the service's business", jobs.ListFilter{OwnerID: bob, AllUsers: true}, n(5, 2)},
		{"one kind", jobs.ListFilter{Kinds: []string{"restic_backup"}}, n(3)},
		{"kinds", jobs.ListFilter{Kinds: []string{"restic_backup", "restic_check"}}, n(5, 3)},
		{"unknown kind", jobs.ListFilter{Kinds: []string{"nope"}}, nil},
		{"query", jobs.ListFilter{Query: "/srv/photos"}, n(3, 1)},
		{"query across a separator matches nothing", jobs.ListFilter{Query: "sync /srv"}, nil},
		{"query with an apostrophe", jobs.ListFilter{Query: "bob's"}, n(2)},
		{"query matching every job", jobs.ListFilter{Query: "r"}, n(5, 4, 3, 2, 1)},
		{"created after (inclusive)", jobs.ListFilter{CreatedAfter: t0().Add(3 * time.Minute)}, n(5, 4)},
		{"created before (exclusive)", jobs.ListFilter{CreatedBefore: t0().Add(time.Minute)}, n(1)},
		{"created range", jobs.ListFilter{CreatedAfter: t0().Add(time.Minute), CreatedBefore: t0().Add(4 * time.Minute)}, n(4, 3, 2)},
		{"empty range", jobs.ListFilter{CreatedAfter: t0().Add(time.Hour), CreatedBefore: t0()}, nil},
		{"owner and worker", jobs.ListFilter{OwnerID: alice, WorkerID: w1.ID}, n(4, 1)},
		{"owner and status", jobs.ListFilter{OwnerID: alice, Statuses: []domain.JobStatus{domain.JobSucceeded}}, n(3, 1)},
		{"owner, kind and query", jobs.ListFilter{OwnerID: alice, Kinds: []string{"rclone_transfer"}, Query: "photos"}, n(1)},
		{"everything", jobs.ListFilter{
			OwnerID: alice, WorkerID: w1.ID, Statuses: []domain.JobStatus{domain.JobQueued, domain.JobSucceeded},
			Kinds: []string{"rclone_transfer"}, Query: "a", CreatedAfter: t0(), CreatedBefore: t0().Add(time.Hour),
		}, n(4, 1)},
		{"cursor and filter", jobs.ListFilter{OwnerID: alice, Before: jobID(t, 4)}, n(3, 1)},
		{"cursor, filter and limit", jobs.ListFilter{OwnerID: alice, Before: jobID(t, 4), Limit: 1}, n(3)},
	}
	for _, tt := range tests {
		if tt.f.Limit == 0 {
			tt.f.Limit = 10
		}
		got, err := s.ListJobs(ctx, tt.f)
		if err != nil || !slices.Equal(jobIDs(got), tt.want) {
			t.Errorf("ListJobs(%s) = %v (%v), want %v", tt.name, jobIDs(got), err, tt.want)
		}
	}
}

// testJobQueryIsLiteral: the query is a plain substring; LIKE wildcards
// and the escape character in it match only themselves.
func testJobQueryIsLiteral(t *testing.T, s Store) {
	ctx := context.Background()
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	texts := []string{
		"rclone_transfer\na_b",               // 1
		"rclone_transfer\naxb",               // 2
		"rclone_transfer\n100%",              // 3
		"rclone_transfer\n1000",              // 4
		`rclone_transfer` + "\n" + `c:\data`, // 5
		"rclone_transfer\ncd",                // 6
		"rclone_transfer\nab",                // 7
	}
	for i, text := range texts {
		j := newJob(t, i+1, w.ID)
		j.SearchText = text
		mustCreateJob(t, s, j)
	}
	for _, tt := range []struct {
		query string
		want  []int
	}{
		{"a_b", []int{1}},
		{"_", []int{7, 6, 5, 4, 3, 2, 1}}, // every text has the kind's underscore
		{"x_", nil},
		{"100%", []int{3}},
		{"%", []int{3}},
		{"10%0", nil},
		{`\d`, []int{5}},
		{`\`, []int{5}},
		{`c:\`, []int{5}},
		{`\%`, nil},
	} {
		got, err := s.ListJobs(ctx, jobs.ListFilter{Query: tt.query, Limit: 10})
		var want []domain.JobID
		for _, n := range tt.want {
			want = append(want, jobID(t, n))
		}
		if err != nil || !slices.Equal(jobIDs(got), want) {
			t.Errorf("ListJobs(query %q) = %v (%v), want jobs %v", tt.query, jobIDs(got), err, tt.want)
		}
	}
}

// mustCreateJobRemote creates remote n of owner (and the owner) for the
// job tests.
func mustCreateJobRemote(t *testing.T, s Store, n int, owner domain.UserID) domain.RemoteID {
	t.Helper()
	ensureUser(t, s, owner)
	r := remotes.Remote{
		ID: remoteID(t, n), OwnerID: owner, Name: fmt.Sprintf("Remote %d", n), Type: "s3",
		CreatedAt: t0(), UpdatedAt: t0(), Version: 1,
	}
	err := s.CreateRemote(context.Background(), r)
	if err != nil {
		t.Fatalf("CreateRemote(%d): %v", n, err)
	}
	return r.ID
}

// testJobRemotes covers RemoteIDs: stored with the job in one
// transaction, returned sorted by every read, filtered by ListFilter.RemoteID,
// and referencing existing (possibly deleted) remotes only.
func testJobRemotes(t *testing.T, s Store) {
	ctx := context.Background()
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	r1, r2, r3 := mustCreateJobRemote(t, s, 1, ownerID(t)), mustCreateJobRemote(t, s, 2, ownerID(t)), mustCreateJobRemote(t, s, 3, ownerID(t))
	if err := s.DeleteRemote(ctx, r3, t0()); err != nil {
		t.Fatal(err)
	}

	a := newJob(t, 1, w.ID)
	a.RemoteIDs = []domain.RemoteID{r2, r1} // any order; read back sorted
	mustCreateJob(t, s, a)
	b := newJob(t, 2, w.ID)
	b.RemoteIDs = []domain.RemoteID{r2}
	mustCreateJob(t, s, b)
	c := newJob(t, 3, w.ID)
	c.RemoteIDs = []domain.RemoteID{r3} // a deleted remote still exists
	mustCreateJob(t, s, c)
	mustCreateJob(t, s, newJob(t, 4, w.ID))

	sorted := []domain.RemoteID{r1, r2}
	got, err := s.GetJob(ctx, a.ID)
	if err != nil || !slices.Equal(got.RemoteIDs, sorted) {
		t.Errorf("GetJob remotes = %v (%v), want %v", got.RemoteIDs, err, sorted)
	}
	next := got
	next.Status, next.RemoteIDs = domain.JobDispatched, nil
	updated, err := s.UpdateJob(ctx, next)
	if err != nil || !slices.Equal(updated.RemoteIDs, sorted) {
		t.Errorf("UpdateJob remotes = %v (%v), want them kept", updated.RemoteIDs, err)
	}
	all, _ := s.ListJobs(ctx, jobs.ListFilter{Limit: 10})
	byID := map[domain.JobID][]domain.RemoteID{}
	for _, j := range all {
		byID[j.ID] = j.RemoteIDs
	}
	if !slices.Equal(byID[a.ID], sorted) || !slices.Equal(byID[b.ID], []domain.RemoteID{r2}) || !slices.Equal(byID[c.ID], []domain.RemoteID{r3}) || len(byID[jobID(t, 4)]) != 0 {
		t.Errorf("ListJobs remotes = %v", byID)
	}
	unfinished, _ := s.ListUnfinishedJobs(ctx, w.ID)
	queued, _ := s.ListQueuedJobs(ctx, t0().Add(time.Hour), 10)
	for _, list := range [][]jobs.Job{unfinished, queued} {
		for _, j := range list {
			if j.ID == b.ID && !slices.Equal(j.RemoteIDs, []domain.RemoteID{r2}) {
				t.Errorf("job %s listed with remotes %v", j.ID, j.RemoteIDs)
			}
		}
	}

	for _, tt := range []struct {
		remote domain.RemoteID
		want   []domain.JobID
	}{
		{r1, []domain.JobID{a.ID}},
		{r2, []domain.JobID{b.ID, a.ID}},
		{r3, []domain.JobID{c.ID}},
		{remoteID(t, 9), nil},
	} {
		got, err := s.ListJobs(ctx, jobs.ListFilter{RemoteID: tt.remote, Limit: 10})
		if err != nil || !slices.Equal(jobIDs(got), tt.want) {
			t.Errorf("ListJobs(remote %s) = %v (%v), want %v", tt.remote, jobIDs(got), err, tt.want)
		}
	}
	if got, _ := s.ListJobs(ctx, jobs.ListFilter{RemoteID: r2, Before: b.ID, Limit: 10}); !slices.Equal(jobIDs(got), []domain.JobID{a.ID}) {
		t.Errorf("ListJobs(remote, cursor) = %v", jobIDs(got))
	}

	// A missing remote fails the whole insert: no job, no remote rows.
	bad := newJob(t, 5, w.ID)
	bad.RemoteIDs = []domain.RemoteID{r1, remoteID(t, 99)}
	wantErr(t, "CreateJob(unknown remote)", s.CreateJob(ctx, bad), domain.ErrNotFound)
	if _, err := s.GetJob(ctx, bad.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a job with an unknown remote was stored: %v", err)
	}
	if got, _ := s.ListJobs(ctx, jobs.ListFilter{RemoteID: r1, Limit: 10}); !slices.Equal(jobIDs(got), []domain.JobID{a.ID}) {
		t.Errorf("a failed insert left remote rows: %v", jobIDs(got))
	}
}

func testSamples(t *testing.T, s Store) {
	ctx := context.Background()
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	j := newJob(t, 1, w.ID)
	mustCreateJob(t, s, j)
	if _, ok, err := s.LatestJobSample(ctx, j.ID); ok || err != nil {
		t.Errorf("LatestJobSample(empty) = (%v, %v)", ok, err)
	}
	for _, sec := range []int{2, 1, 3, 2} { // out of order, one duplicate key
		smp := jobs.Sample{JobID: j.ID, Time: t0().Add(time.Duration(sec) * time.Second), Stats: jobs.Stats{Bytes: int64(sec) << 32}}
		if err := s.AppendJobSample(ctx, smp); err != nil {
			t.Fatalf("AppendJobSample: %v", err)
		}
	}
	list, err := s.ListJobSamples(ctx, j.ID)
	if err != nil || len(list) != 3 {
		t.Fatalf("ListJobSamples = (%d samples, %v), want 3 (duplicate ignored)", len(list), err)
	}
	for i, smp := range list {
		if want := t0().Add(time.Duration(i+1) * time.Second); !smp.Time.Equal(want) || smp.Stats.Bytes != int64(i+1)<<32 {
			t.Errorf("sample %d = %+v, want time %v", i, smp, want)
		}
	}
	latest, ok, err := s.LatestJobSample(ctx, j.ID)
	if err != nil || !ok || !latest.Time.Equal(t0().Add(3*time.Second)) {
		t.Errorf("LatestJobSample = (%+v, %v, %v)", latest, ok, err)
	}
	wantErr(t, "AppendJobSample(missing job)", s.AppendJobSample(ctx, jobs.Sample{JobID: jobID(t, 9), Time: t0()}), domain.ErrNotFound)
}

func testNoAliasing(t *testing.T, s Store) {
	ctx := context.Background()
	w := newWorker(t, 1)
	w.Info.JobKinds = []string{"a"}
	mustCreateWorker(t, s, w)
	w.Info.JobKinds[0] = "mutated"
	got, _ := s.GetWorker(ctx, w.ID)
	if got.Info.JobKinds[0] != "a" {
		t.Error("store aliases the CreateWorker argument")
	}
	got.Info.JobKinds[0] = "mutated"
	got.SecretEnc[0] = 'X'
	again, _ := s.GetWorker(ctx, w.ID)
	if again.Info.JobKinds[0] != "a" || string(again.SecretEnc) != "enc" {
		t.Error("store aliases a GetWorker result")
	}

	j := newJob(t, 1, w.ID)
	j.Warnings = []string{"w"}
	mustCreateJob(t, s, j)
	j.Warnings[0] = "mutated"
	j.Definition[0] = 'X'
	gj, _ := s.GetJob(ctx, j.ID)
	gj.Definition[0] = 'X'
	gj2, _ := s.GetJob(ctx, j.ID)
	if gj2.Warnings[0] != "w" || !sameJSON(gj2.Definition, []byte(testDefinition)) {
		t.Error("store aliases job values")
	}

	t.Run("remotes", func(t *testing.T) {
		r1, r2 := mustCreateJobRemote(t, s, 1, ownerID(t)), mustCreateJobRemote(t, s, 2, ownerID(t))
		k := newJob(t, 2, w.ID)
		k.RemoteIDs = []domain.RemoteID{r1, r2}
		mustCreateJob(t, s, k)
		k.RemoteIDs[0] = r2
		got, _ := s.GetJob(ctx, k.ID)
		got.RemoteIDs[0] = r2
		list, _ := s.ListJobs(ctx, jobs.ListFilter{Limit: 10})
		for _, l := range list {
			if len(l.RemoteIDs) > 0 {
				l.RemoteIDs[0] = r2
			}
		}
		if again, _ := s.GetJob(ctx, k.ID); !slices.Equal(again.RemoteIDs, []domain.RemoteID{r1, r2}) {
			t.Errorf("store aliases job remote IDs: %v", again.RemoteIDs)
		}
	})
}

// errorsIs is errors.Is, for compact conditions.
func errorsIs(err, target error) bool { return errors.Is(err, target) }
