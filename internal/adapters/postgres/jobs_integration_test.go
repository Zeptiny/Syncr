//go:build integration

package postgres

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// The job tests of the contract suite that need remotes skip on Postgres
// until the remotes port is implemented (m3: pending track B). These tests
// cover the same job paths now, with users and remotes inserted by plain
// SQL.

// insertJobsTestUser inserts a user row and returns its ID.
func insertJobsTestUser(t *testing.T, s *Store) domain.UserID {
	t.Helper()
	id := domain.NewUserID()
	_, err := s.pool.Exec(t.Context(), `INSERT INTO users (id, username, role, password_hash, created_at, updated_at, password_changed_at)
		VALUES ($1, $2, 'user', 'x', now(), now(), now())`, pgID(id), "jobs-"+id.String()[24:])
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// insertJobsTestRemote inserts a remote row of owner (deleted if deleted)
// and returns its ID.
func insertJobsTestRemote(t *testing.T, s *Store, owner domain.UserID, name string, deleted bool) domain.RemoteID {
	t.Helper()
	id := domain.NewRemoteID()
	var deletedAt any
	if deleted {
		deletedAt = time.Now()
	}
	_, err := s.pool.Exec(t.Context(), `INSERT INTO remotes (id, owner_id, name, backend_type, created_at, updated_at, deleted_at)
		VALUES ($1, $2, $3, 's3', now(), now(), $4)`, pgID(id), pgID(owner), name, deletedAt)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func jobsTestWorker(t *testing.T, s *Store) domain.WorkerID {
	t.Helper()
	w := workers.Worker{ID: domain.NewWorkerID(), Name: "w", Mode: domain.WorkerActive, CreatedAt: testTime()}
	if err := s.CreateWorker(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	return w.ID
}

func TestJobRemotesInOneTransaction(t *testing.T) {
	t.Parallel()
	s := newMigratedStore(t)
	ctx := t.Context()
	owner := insertJobsTestUser(t, s)
	w := jobsTestWorker(t, s)
	r1, r2 := insertJobsTestRemote(t, s, owner, "one", false), insertJobsTestRemote(t, s, owner, "two", false)
	gone := insertJobsTestRemote(t, s, owner, "gone", true)
	sorted := []domain.RemoteID{r1, r2}

	a := newTestJob(w, owner)
	a.RemoteIDs, a.SearchText = []domain.RemoteID{r2, r1}, "rclone_transfer\nsync\none\ntwo"
	if err := s.CreateJob(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetJob(ctx, a.ID)
	if err != nil || !slices.Equal(got.RemoteIDs, sorted) || got.SearchText != a.SearchText {
		t.Errorf("GetJob = remotes %v, text %q (%v)", got.RemoteIDs, got.SearchText, err)
	}
	b := newTestJob(w, owner)
	b.RemoteIDs = []domain.RemoteID{gone}
	if err := s.CreateJob(ctx, b); err != nil {
		t.Errorf("CreateJob(deleted remote) = %v, want it stored", err)
	}

	bad := newTestJob(w, owner)
	bad.RemoteIDs = []domain.RemoteID{r1, domain.NewRemoteID()}
	if err := s.CreateJob(ctx, bad); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("CreateJob(unknown remote) = %v, want ErrNotFound", err)
	}
	if _, err := s.GetJob(ctx, bad.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("the failed job was stored: %v", err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM job_remotes WHERE job_id = $1`, pgID(bad.ID)).Scan(&n); err != nil || n != 0 {
		t.Errorf("job_remotes rows of the failed job: %d (%v)", n, err)
	}

	for remote, want := range map[domain.RemoteID][]domain.JobID{r1: {a.ID}, r2: {a.ID}, gone: {b.ID}, domain.NewRemoteID(): nil} {
		js, err := s.ListJobs(ctx, jobs.ListFilter{RemoteID: remote, Limit: 10})
		var ids []domain.JobID
		for _, j := range js {
			ids = append(ids, j.ID)
		}
		if err != nil || !slices.Equal(ids, want) {
			t.Errorf("ListJobs(remote %s) = %v (%v), want %v", remote, ids, err, want)
		}
	}
	js, err := s.ListUnfinishedJobs(ctx, w)
	if err != nil || len(js) != 2 || !slices.Equal(js[0].RemoteIDs, sorted) || !slices.Equal(js[1].RemoteIDs, []domain.RemoteID{gone}) {
		t.Errorf("ListUnfinishedJobs = %+v (%v)", js, err)
	}
	next := got
	next.Status = domain.JobDispatched
	updated, err := s.UpdateJob(ctx, next)
	if err != nil || !slices.Equal(updated.RemoteIDs, sorted) || updated.SearchText != a.SearchText {
		t.Errorf("UpdateJob = remotes %v, text %q (%v)", updated.RemoteIDs, updated.SearchText, err)
	}
}

func TestJobOwnerForeignKey(t *testing.T) {
	t.Parallel()
	s := newMigratedStore(t)
	ctx := context.Background()
	w := jobsTestWorker(t, s)
	j := newTestJob(w, domain.NewUserID())
	if err := s.CreateJob(ctx, j); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("CreateJob(unknown owner) = %v, want ErrNotFound", err)
	}
}
