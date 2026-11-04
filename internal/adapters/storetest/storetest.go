// Package storetest is the contract test suite of the controller's
// repository ports (workers.Repository, workers.TokenRepository,
// jobs.Repository, jobs.SampleRepository). Every store adapter runs it from
// its tests:
//
//	func TestContract(t *testing.T) {
//		storetest.Run(t, func(t *testing.T) storetest.Store { return newStore(t) })
//	}
//
// It checks the documented semantics, above all the conditional updates the
// use cases rely on for concurrency safety (compare-and-swap epoch,
// monotonic cursors, single-use tokens, version-checked job updates), and
// that returned values do not alias the store.
//
// It is test support, not an adapter: it imports only the standard library,
// internal/domain and the app packages whose ports it exercises.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Store is everything a controller store adapter implements.
type Store interface {
	workers.Repository
	workers.TokenRepository
	jobs.Repository
	jobs.SampleRepository
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
		{"Samples", testSamples},
		{"NoAliasing", testNoAliasing},
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
		ID: jobID(t, n), WorkerID: w, Kind: "rclone_transfer", Status: domain.JobQueued,
		SpecEnc: []byte("spec"), CreatedAt: t0(), QueuedAt: t0(), ExitCode: -1, Version: 1,
	}
}

func mustCreateJob(t *testing.T, s Store, j jobs.Job) {
	t.Helper()
	if err := s.CreateJob(context.Background(), j); err != nil {
		t.Fatalf("CreateJob: %v", err)
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
	mustCreateJob(t, s, j)
	wantErr(t, "duplicate CreateJob", s.CreateJob(ctx, j), domain.ErrAlreadyExists)
	wantErr(t, "CreateJob(unknown worker)", s.CreateJob(ctx, newJob(t, 2, workerID(t, 9))), domain.ErrNotFound)
	got, err := s.GetJob(ctx, j.ID)
	if err != nil || got.Status != domain.JobQueued || got.Version != 1 || string(got.SpecEnc) != "spec" || got.ExitCode != -1 || !got.QueuedAt.Equal(t0()) {
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
	next.Kind, next.SpecEnc, next.CreatedAt = "other", []byte("x"), t0().Add(time.Hour)
	stored, err := s.UpdateJob(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != 2 || stored.Status != domain.JobDispatched || stored.Epoch != "e" || stored.Stats.Bytes != 1<<40 ||
		stored.Error.Code != "c" || stored.Requeues != 2 || stored.ExitCode != 3 || !stored.CancelRequestedAt.Equal(t0().Add(2*time.Second)) {
		t.Errorf("UpdateJob returned %+v", stored)
	}
	if stored.Kind != "rclone_transfer" || string(stored.SpecEnc) != "spec" || !stored.CreatedAt.Equal(t0()) {
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
	gj, _ := s.GetJob(ctx, j.ID)
	gj.SpecEnc[0] = 'X'
	gj2, _ := s.GetJob(ctx, j.ID)
	if gj2.Warnings[0] != "w" || string(gj2.SpecEnc) != "spec" {
		t.Error("store aliases job values")
	}
}
