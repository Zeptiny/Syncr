package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// This file implements restic.Repository over the restic_repositories and
// restic_snapshots tables (migration 00007) with the queries of
// queries/restic.sql.
//
// m5: Track C implements every method here. The queries are written
// (sqlc's db.Queries has them, including the InsertSnapshots copy and the
// statistics LATERALs); what is left is the Go mapping and the
// transactions:
//   - CreateRepo: q.CreateRepository; 0 rows is ErrNotFound (remote
//     missing or deleted), mapErr maps unique and foreign key violations.
//   - GetRepo, ListRepos: q.GetRepository / q.ListRepositories with Stats
//     from the row; GetRepos: q.GetRepositories.
//   - UpdateRepo: q.UpdateRepository, then GetRepository for the Stats; no
//     row: q.GetRepositoryState tells ErrNotFound from ErrConflict.
//   - DeleteRepo: one transaction of q.DeleteRepository and
//     q.DeleteRepositorySnapshots; 0 rows: GetRepositoryState tells
//     ErrNotFound from ErrFailedPrecondition (deleting).
//   - MarkDeleting: one transaction of q.MarkRepositoryDeleting,
//     q.PauseRepositorySchedules, q.CancelRepositoryOpenRuns, then
//     GetRepository; 0 rows: GetRepositoryState tells ErrNotFound from
//     ErrConflict (already deleting).
//   - ListPendingDeletions: q.ListPendingDeletions (limitArg).
//   - FinishDeletion: q.FinishRepositoryDeletionSucceeded with
//     q.DeleteRepositorySnapshots in one transaction, or
//     q.FinishRepositoryDeletionFailed with the error clipped to
//     restic.MaxDeletionErrorLen runes; 0 rows is ErrConflict.
//   - ReplaceSnapshots: one transaction of q.LockLiveRepository (no row:
//     ErrNotFound), q.DeleteRepositorySnapshots, q.InsertSnapshots and
//     q.SetSnapshotsRefreshed; summaries are stored as JSON with restic's
//     field names.
//   - ListSnapshots: q.GetSnapshotsState, q.ListSnapshots and
//     q.RepositoryLastChange(restic.MutatingKinds()).
// Then remove errResticPending; the storetest restic suite stops skipping.

// errResticPending is the error of the restic.Repository methods until
// Track C implements them.
var errResticPending = fmt.Errorf("restic repositories: %w (m5: pending track C)", errors.ErrUnsupported)

// CreateRepo implements restic.Repository.
func (s *Store) CreateRepo(context.Context, restic.Repo) error {
	return fmt.Errorf("create repository: %w", errResticPending)
}

// GetRepo implements restic.Repository.
func (s *Store) GetRepo(context.Context, domain.ResticRepoID) (restic.Repo, error) {
	return restic.Repo{}, fmt.Errorf("get repository: %w", errResticPending)
}

// GetRepos implements restic.Repository.
func (s *Store) GetRepos(context.Context, []domain.ResticRepoID) ([]restic.Repo, error) {
	return nil, fmt.Errorf("get repositories: %w", errResticPending)
}

// ListRepos implements restic.Repository.
func (s *Store) ListRepos(context.Context, restic.ListFilter) ([]restic.Repo, error) {
	return nil, fmt.Errorf("list repositories: %w", errResticPending)
}

// UpdateRepo implements restic.Repository.
func (s *Store) UpdateRepo(context.Context, restic.Repo) (restic.Repo, error) {
	return restic.Repo{}, fmt.Errorf("update repository: %w", errResticPending)
}

// DeleteRepo implements restic.Repository.
func (s *Store) DeleteRepo(context.Context, domain.ResticRepoID, time.Time) error {
	return fmt.Errorf("delete repository: %w", errResticPending)
}

// MarkDeleting implements restic.Repository.
func (s *Store) MarkDeleting(context.Context, domain.ResticRepoID, domain.JobID, time.Time) (restic.Repo, error) {
	return restic.Repo{}, fmt.Errorf("mark repository deleting: %w", errResticPending)
}

// ListPendingDeletions implements restic.Repository.
func (s *Store) ListPendingDeletions(context.Context, time.Time, int) ([]restic.PendingDeletion, error) {
	return nil, fmt.Errorf("list pending deletions: %w", errResticPending)
}

// FinishDeletion implements restic.Repository.
func (s *Store) FinishDeletion(context.Context, domain.ResticRepoID, domain.JobID, restic.DeletionOutcome, time.Time) error {
	return fmt.Errorf("finish deletion: %w", errResticPending)
}

// ReplaceSnapshots implements restic.Repository.
func (s *Store) ReplaceSnapshots(context.Context, domain.ResticRepoID, []restic.Snapshot, bool, time.Time) error {
	return fmt.Errorf("replace snapshots: %w", errResticPending)
}

// ListSnapshots implements restic.Repository.
func (s *Store) ListSnapshots(context.Context, domain.ResticRepoID) (restic.SnapshotList, error) {
	return restic.SnapshotList{}, fmt.Errorf("list snapshots: %w", errResticPending)
}
