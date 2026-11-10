//go:build integration

package postgres

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// These tests cover what the contract suite cannot: races across two
// connection pools (two controllers on one database), the stored form of
// the snapshot summaries, a full-size snapshot copy, and the transactions
// of MarkDeleting and DeleteRemote against PostgreSQL's locks.

func resticTestHex(n int) string { return fmt.Sprintf("%064x", n) }

// resticTestRepo inserts a user and a remote and returns a repository on
// it, not stored yet.
func resticTestRepo(t *testing.T, s *Store, name string) restic.Repo {
	t.Helper()
	owner := insertJobsTestUser(t, s)
	id := domain.NewResticRepoID()
	u := id.UUID()
	return restic.Repo{
		ID: id, OwnerID: owner, Name: name, RemoteID: insertJobsTestRemote(t, s, owner, "remote-"+name, false), Path: "restic/" + name,
		PasswordEnc: []byte("encrypted"), ResticID: strings.Repeat(fmt.Sprintf("%x", u[:]), 2), FormatVersion: 2,
		CreatedAt: testTime(), UpdatedAt: testTime(), Version: 1,
	}
}

func TestResticSummaryIsStoredWithResticFieldNames(t *testing.T) {
	s := newMigratedStore(t)
	ctx := t.Context()
	r := resticTestRepo(t, s, "photos")
	if err := s.CreateRepo(ctx, r); err != nil {
		t.Fatal(err)
	}
	at := testTime()
	snap := restic.Snapshot{ID: resticTestHex(1), Time: at, Hostname: "nas\x00-01", Paths: []string{"/srv"},
		Summary: &restic.SnapshotSummary{BackupStart: at.Add(-time.Minute), BackupEnd: at, FilesNew: 3, DataAdded: 10, DataAddedPacked: 7, TotalFilesProcessed: 5, TotalBytesProcessed: 12345}}
	if err := s.ReplaceSnapshots(ctx, r.ID, []restic.Snapshot{snap, {ID: resticTestHex(2), Time: at.Add(-time.Hour)}}, false, at); err != nil {
		t.Fatal(err)
	}
	var bytes, files, added int64
	var start time.Time
	err := s.pool.QueryRow(ctx, `SELECT (summary->>'total_bytes_processed')::bigint, (summary->>'total_files_processed')::bigint,
		(summary->>'data_added')::bigint, (summary->>'backup_start')::timestamptz
		FROM restic_snapshots WHERE repository_id = $1 AND snapshot_id = $2`, pgID(r.ID), snap.ID).Scan(&bytes, &files, &added, &start)
	if err != nil || bytes != 12345 || files != 5 || added != 10 || !start.Equal(at.Add(-time.Minute)) {
		t.Errorf("the stored summary: bytes %d files %d added %d start %s, %v", bytes, files, added, start, err)
	}
	var null bool
	if err := s.pool.QueryRow(ctx, `SELECT summary IS NULL FROM restic_snapshots WHERE repository_id = $1 AND snapshot_id = $2`,
		pgID(r.ID), resticTestHex(2)).Scan(&null); err != nil || !null {
		t.Errorf("a snapshot without a summary stores NULL: %v, %v", null, err)
	}
	// A NUL byte from a tool cannot break the copy.
	list, err := s.ListSnapshots(ctx, r.ID)
	if err != nil || len(list.Snapshots) != 2 || list.Snapshots[0].Hostname != "nas�-01" || list.Snapshots[1].Summary != nil {
		t.Errorf("ListSnapshots = %+v, %v", list.Snapshots, err)
	}
	if got, err := s.GetRepo(ctx, r.ID); err != nil || got.Stats.LatestSnapshotBytes != 12345 || got.Stats.SnapshotCount != 2 {
		t.Errorf("stats = %+v, %v", got.Stats, err)
	}
}

func TestResticReplaceFullSizeAcrossPools(t *testing.T) {
	s1, s2 := twoStores(t)
	ctx := t.Context()
	r := resticTestRepo(t, s1, "big")
	if err := s1.CreateRepo(ctx, r); err != nil {
		t.Fatal(err)
	}
	// Two controllers refresh the same repository with full-size lists.
	set := func(base int) []restic.Snapshot {
		out := make([]restic.Snapshot, restic.MaxSnapshots)
		for i := range out {
			out[i] = restic.Snapshot{ID: resticTestHex(base + i), Time: testTime().Add(time.Duration(i) * time.Second), Tags: []string{"t"},
				Summary: &restic.SnapshotSummary{TotalBytesProcessed: int64(base + i)}}
		}
		return out
	}
	sets := [][]restic.Snapshot{set(1_000_000), set(2_000_000)}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, s := range []*Store{s1, s2} {
		wg.Go(func() {
			errs[i] = s.ReplaceSnapshots(ctx, r.ID, sets[i], i == 0, testTime().Add(time.Duration(i+1)*time.Hour))
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	list, err := s2.ListSnapshots(ctx, r.ID)
	if err != nil || len(list.Snapshots) != restic.MaxSnapshots {
		t.Fatalf("ListSnapshots = %d snapshots, %v", len(list.Snapshots), err)
	}
	winner := 0
	if list.Snapshots[0].ID == sets[1][restic.MaxSnapshots-1].ID {
		winner = 1
	}
	for i, sn := range list.Snapshots {
		if want := sets[winner][restic.MaxSnapshots-1-i].ID; sn.ID != want {
			t.Fatalf("snapshot %d is %s: the cache mixes the two lists", i, sn.ID)
		}
	}
	if list.Truncated != (winner == 0) || !list.RefreshedAt.Equal(testTime().Add(time.Duration(winner+1)*time.Hour)) {
		t.Errorf("truncated %v, refreshed %s: not the winning list's (%d)", list.Truncated, list.RefreshedAt, winner)
	}
	got, err := s1.GetRepo(ctx, r.ID)
	if err != nil || got.Stats.SnapshotCount != restic.MaxSnapshots || got.Stats.LatestSnapshotBytes != sets[winner][restic.MaxSnapshots-1].Summary.TotalBytesProcessed {
		t.Errorf("stats = %+v, %v", got.Stats, err)
	}
}

func TestResticMarkDeletingAcrossPools(t *testing.T) {
	s1, s2 := twoStores(t)
	ctx := t.Context()
	r := resticTestRepo(t, s1, "doomed")
	if err := s1.CreateRepo(ctx, r); err != nil {
		t.Fatal(err)
	}
	worker := schedTestWorker(t, s1)
	sched := func(name string, repo domain.ResticRepoID) schedules.Schedule {
		sc := schedules.Schedule{
			ID: domain.NewScheduleID(), OwnerID: r.OwnerID, Name: name, Cron: "0 * * * *", Timezone: "UTC", Kind: "restic_backup",
			Definition: []byte(`{}`), Target: schedules.Target{WorkerID: worker}, Missed: schedules.MissedPolicy{Action: schedules.MissedRunLate},
			Overlap: schedules.OverlapSkip, Enabled: true, NextRunAt: schedT0.Add(time.Hour), RepositoryID: repo,
			CreatedAt: schedT0, UpdatedAt: schedT0, Version: 1,
		}
		if err := s1.CreateSchedule(ctx, sc); err != nil {
			t.Fatal(err)
		}
		return sc
	}
	on, off := sched("on the repository", r.ID), sched("elsewhere", domain.ResticRepoID{})
	runOn, err := s1.AddRun(ctx, pendingRun(on, schedT0), false)
	if err != nil {
		t.Fatal(err)
	}
	runOff, err := s1.AddRun(ctx, pendingRun(off, schedT0), false)
	if err != nil {
		t.Fatal(err)
	}

	// Two controllers get the same request at once: one job wins.
	jobIDs := []domain.JobID{domain.NewJobID(), domain.NewJobID()}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, s := range []*Store{s1, s2} {
		wg.Go(func() { _, errs[i] = s.MarkDeleting(ctx, r.ID, jobIDs[i], schedT0.Add(time.Minute)) })
	}
	wg.Wait()
	winner := -1
	for i, err := range errs {
		switch {
		case err == nil && winner < 0:
			winner = i
		case err == nil:
			t.Fatal("both marks won")
		case !errors.Is(err, domain.ErrConflict):
			t.Fatalf("the losing mark failed with %v, want ErrConflict", err)
		}
	}
	if winner < 0 {
		t.Fatal("no mark won")
	}
	got, err := s2.GetRepo(ctx, r.ID)
	if err != nil || !got.Deleting || got.DeletionJobID != jobIDs[winner] || got.Version != 2 {
		t.Fatalf("the repository = %+v, %v", got, err)
	}
	if sc, err := s2.GetSchedule(ctx, on.ID); err != nil || sc.Enabled || sc.PausedReason != schedules.ReasonRepositoryDeleting || !sc.NextRunAt.IsZero() || sc.Version != 2 {
		t.Errorf("the repository's schedule = %+v, %v; want it paused once", sc, err)
	}
	if run, err := s2.GetRun(ctx, runOn.ID); err != nil || run.Status != schedules.RunCancelled || run.Reason != schedules.ReasonRepositoryDeleting {
		t.Errorf("its open run = %+v, %v", run, err)
	}
	if sc, _ := s2.GetSchedule(ctx, off.ID); !sc.Enabled || sc.Version != 1 {
		t.Errorf("an unrelated schedule = %+v", sc)
	}
	if run, _ := s2.GetRun(ctx, runOff.ID); run.Status != schedules.RunPending {
		t.Errorf("an unrelated run = %+v", run)
	}

	// The join of ListPendingDeletions: nothing until the job is terminal.
	if p, err := s1.ListPendingDeletions(ctx, schedT0, -1); err != nil || len(p) != 0 {
		t.Fatalf("pending before the job exists = %+v, %v", p, err)
	}
	j := jobs.Job{ID: jobIDs[winner], WorkerID: worker, OwnerID: r.OwnerID, Kind: restic.KindDeleteData, Status: domain.JobFailed,
		Definition: []byte(`{}`), RepositoryID: r.ID, CreatedAt: schedT0, QueuedAt: schedT0, FinishedAt: schedT0.Add(time.Hour),
		Error: jobs.JobError{Code: "repository_mismatch", Message: "the location holds another repository"}, ExitCode: 1, Version: 1}
	if err := s1.CreateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	p, err := s2.ListPendingDeletions(ctx, schedT0, -1)
	if err != nil || len(p) != 1 || p[0].RepoID != r.ID || p[0].JobID != j.ID || p[0].JobStatus != domain.JobFailed ||
		p[0].JobErrorCode != "repository_mismatch" || p[0].JobErrorMessage != "the location holds another repository" || p[0].OwnerID != r.OwnerID {
		t.Fatalf("pending = %+v, %v", p, err)
	}
	// Two controllers finish it: one compare-and-swap wins.
	for i, s := range []*Store{s1, s2} {
		wg.Go(func() {
			errs[i] = s.FinishDeletion(ctx, r.ID, j.ID, restic.DeletionOutcome{Error: "deleting the data failed"}, schedT0.Add(2*time.Hour))
		})
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) || !errors.Is(errors.Join(errs...), domain.ErrConflict) {
		t.Errorf("FinishDeletion from two pools = %v, %v; want one winner and one conflict", errs[0], errs[1])
	}
	if got, _ := s1.GetRepo(ctx, r.ID); got.Deleting || got.DeletionError != "deleting the data failed" || got.Version != 3 || got.PasswordEnc == nil {
		t.Errorf("after the failed deletion = %+v", got)
	}
}

// A repository being created and its remote being deleted race: either
// the repository exists and the remote stays, or the remote is deleted
// and the repository was refused. Never a live repository on a deleted
// remote.
func TestResticCreateRacesRemoteDelete(t *testing.T) {
	s1, s2 := twoStores(t)
	ctx := t.Context()
	for i := range 20 {
		r := resticTestRepo(t, s1, fmt.Sprintf("race-%d", i))
		var wg sync.WaitGroup
		var createErr, deleteErr error
		wg.Go(func() { createErr = s1.CreateRepo(ctx, r) })
		wg.Go(func() { deleteErr = s2.DeleteRemote(ctx, r.RemoteID, testTime()) })
		wg.Wait()
		switch {
		case createErr == nil && errors.Is(deleteErr, domain.ErrFailedPrecondition):
		case errors.Is(createErr, domain.ErrNotFound) && deleteErr == nil:
		default:
			t.Fatalf("round %d: CreateRepo = %v, DeleteRemote = %v; want exactly one to win", i, createErr, deleteErr)
		}
		remote, err := s1.GetRemote(ctx, r.RemoteID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s1.GetRepo(ctx, r.ID); (err == nil) == remote.Deleted() {
			t.Fatalf("round %d: repository exists %v on a remote deleted %v", i, err == nil, remote.Deleted())
		}
	}
}
