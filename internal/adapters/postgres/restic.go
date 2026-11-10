package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Zeptiny/Syncr/internal/adapters/postgres/db"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// This file implements restic.Repository over the restic_repositories and
// restic_snapshots tables (migration 00007) with the queries of
// queries/restic.sql.
//
// A repository's state changes are single conditional UPDATE statements
// (live, not deleting, deleting with this job), so competing controllers
// are serialized by the row lock; when one matches nothing, a second read
// (GetRepositoryState) tells the documented errors apart. Changes that
// span tables run in one transaction: a deletion with the snapshot cache,
// MarkDeleting with the repository's schedules and their open runs, and
// the replacement of the snapshot cache, which locks the repository row
// first so that concurrent replacements never mix.

// CreateRepo implements restic.Repository.
func (s *Store) CreateRepo(ctx context.Context, r restic.Repo) error {
	op := fmt.Sprintf("create repository %s", r.ID)
	n, err := s.q.CreateRepository(ctx, db.CreateRepositoryParams{
		ID:            pgID(r.ID),
		OwnerID:       pgID(r.OwnerID),
		Name:          r.Name,
		Description:   r.Description,
		RemoteID:      pgID(r.RemoteID),
		Path:          r.Path,
		PasswordEnc:   r.PasswordEnc,
		ResticID:      r.ResticID,
		FormatVersion: int32(min(max(r.FormatVersion, -1), 1<<31-1)),
		CreatedAt:     pgTime(r.CreatedAt),
		UpdatedAt:     pgTime(r.UpdatedAt),
		Version:       r.Version,
	})
	if err != nil {
		return mapErr(op, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: remote %s: %w", op, r.RemoteID, domain.ErrNotFound)
	}
	return nil
}

// GetRepo implements restic.Repository.
func (s *Store) GetRepo(ctx context.Context, id domain.ResticRepoID) (restic.Repo, error) {
	return getRepo(ctx, s.q, id)
}

// getRepo reads a repository with its statistics.
func getRepo(ctx context.Context, q *db.Queries, id domain.ResticRepoID) (restic.Repo, error) {
	op := fmt.Sprintf("get repository %s", id)
	row, err := q.GetRepository(ctx, db.GetRepositoryParams{ID: pgID(id), MutatingKinds: restic.MutatingKinds()})
	if err != nil {
		return restic.Repo{}, mapErr(op, err)
	}
	r, err := repoWithStats(repoStatsRow(row))
	if err != nil {
		return restic.Repo{}, fmt.Errorf("%s: %w", op, err)
	}
	return r, nil
}

// GetRepoRow implements restic.Repository.
func (s *Store) GetRepoRow(ctx context.Context, id domain.ResticRepoID) (restic.Repo, error) {
	op := fmt.Sprintf("get repository %s", id)
	row, err := s.q.GetRepositoryRow(ctx, pgID(id))
	if err != nil {
		return restic.Repo{}, mapErr(op, err)
	}
	r, err := repoFrom(row)
	if err != nil {
		return restic.Repo{}, fmt.Errorf("%s: %w", op, err)
	}
	return r, nil
}

// GetRepos implements restic.Repository.
func (s *Store) GetRepos(ctx context.Context, ids []domain.ResticRepoID) ([]restic.Repo, error) {
	const op = "get repositories"
	out := []restic.Repo{}
	if len(ids) == 0 {
		return out, nil
	}
	pgIDs := make([]pgtype.UUID, len(ids))
	for i, id := range ids {
		pgIDs[i] = pgID(id)
	}
	rows, err := s.q.GetRepositories(ctx, pgIDs)
	if err != nil {
		return nil, mapErr(op, err)
	}
	for _, row := range rows {
		r, err := repoFrom(row)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// ListRepos implements restic.Repository.
func (s *Store) ListRepos(ctx context.Context, f restic.ListFilter) ([]restic.Repo, error) {
	const op = "list repositories"
	rows, err := s.q.ListRepositories(ctx, db.ListRepositoriesParams{
		OwnerID: pgOptID(f.OwnerID), RemoteID: pgOptID(f.RemoteID), MutatingKinds: restic.MutatingKinds(),
	})
	if err != nil {
		return nil, mapErr(op, err)
	}
	out := make([]restic.Repo, 0, len(rows))
	for _, row := range rows {
		r, err := repoWithStats(repoStatsRow(row))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// ListRepoRows implements restic.Repository.
func (s *Store) ListRepoRows(ctx context.Context, f restic.ListFilter) ([]restic.Repo, error) {
	const op = "list repositories"
	rows, err := s.q.ListRepositoryRows(ctx, db.ListRepositoryRowsParams{OwnerID: pgOptID(f.OwnerID), RemoteID: pgOptID(f.RemoteID)})
	if err != nil {
		return nil, mapErr(op, err)
	}
	out := make([]restic.Repo, 0, len(rows))
	for _, row := range rows {
		r, err := repoFrom(row)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// UpdateRepo implements restic.Repository: UPDATE ... WHERE id AND version
// AND live RETURNING. When it matches nothing, a second query tells a
// missing or deleted repository (ErrNotFound) from a version mismatch
// (ErrConflict).
func (s *Store) UpdateRepo(ctx context.Context, r restic.Repo) (restic.Repo, error) {
	op := fmt.Sprintf("update repository %s", r.ID)
	var out restic.Repo
	err := s.inTx(ctx, op, func(q *db.Queries) error {
		_, err := q.UpdateRepository(ctx, db.UpdateRepositoryParams{
			ID:              pgID(r.ID),
			ExpectedVersion: r.Version,
			Name:            r.Name,
			Description:     r.Description,
			PasswordEnc:     r.PasswordEnc,
			UpdatedAt:       pgTime(r.UpdatedAt),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			st, serr := q.GetRepositoryState(ctx, pgID(r.ID))
			switch {
			case errors.Is(serr, pgx.ErrNoRows) || (serr == nil && st.DeletedAt.Valid):
				return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
			case serr != nil:
				return mapErr(op, serr)
			}
			return fmt.Errorf("%s: not at version %d: %w", op, r.Version, domain.ErrConflict)
		}
		if err != nil {
			return mapErr(op, err)
		}
		out, err = getRepo(ctx, q, r.ID)
		return err
	})
	if err != nil {
		return restic.Repo{}, err
	}
	return out, nil
}

// DeleteRepo implements restic.Repository.
func (s *Store) DeleteRepo(ctx context.Context, id domain.ResticRepoID, at time.Time) error {
	op := fmt.Sprintf("delete repository %s", id)
	return s.inTx(ctx, op, func(q *db.Queries) error {
		n, err := q.DeleteRepository(ctx, db.DeleteRepositoryParams{ID: pgID(id), At: pgTime(at)})
		if err != nil {
			return mapErr(op, err)
		}
		if n == 0 {
			st, err := q.GetRepositoryState(ctx, pgID(id))
			switch {
			case err != nil:
				return mapErr(op, err) // no row: it never existed
			case st.DeletedAt.Valid:
				return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
			}
			var job domain.JobID
			_ = decodeID(&job, st.DeletionJobID)
			return fmt.Errorf("%s: its data is being deleted (job %s): %w", op, job, domain.ErrFailedPrecondition)
		}
		return mapErr(op, q.DeleteRepositorySnapshots(ctx, pgID(id)))
	})
}

// MarkDeleting implements restic.Repository: the repository, its enabled
// schedules and their open runs change in one transaction.
func (s *Store) MarkDeleting(ctx context.Context, id domain.ResticRepoID, jobID domain.JobID, at time.Time) (restic.Repo, error) {
	op := fmt.Sprintf("mark repository %s deleting", id)
	if jobID.IsZero() {
		return restic.Repo{}, fmt.Errorf("%s: %w: no job", op, domain.ErrInvalidArgument)
	}
	var out restic.Repo
	err := s.inTx(ctx, op, func(q *db.Queries) error {
		n, err := q.MarkRepositoryDeleting(ctx, db.MarkRepositoryDeletingParams{ID: pgID(id), JobID: pgID(jobID), At: pgTime(at)})
		if err != nil {
			return mapErr(op, err)
		}
		if n == 0 {
			st, err := q.GetRepositoryState(ctx, pgID(id))
			switch {
			case err != nil:
				return mapErr(op, err) // no row: it never existed
			case st.DeletedAt.Valid:
				return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
			}
			return fmt.Errorf("%s: it is already deleting: %w", op, domain.ErrConflict)
		}
		// The repository row is locked by the update above: a schedule
		// cannot be created on it meanwhile without waiting (the foreign
		// key takes a share lock on the row), and then it is rejected by
		// the use case, which reads the repository as deleting.
		if err := q.PauseRepositorySchedules(ctx, db.PauseRepositorySchedulesParams{ID: pgID(id), At: pgTime(at)}); err != nil {
			return mapErr(op, err)
		}
		if err := q.CancelRepositoryOpenRuns(ctx, db.CancelRepositoryOpenRunsParams{ID: pgID(id), At: pgTime(at)}); err != nil {
			return mapErr(op, err)
		}
		out, err = getRepo(ctx, q, id)
		return err
	})
	if err != nil {
		return restic.Repo{}, err
	}
	return out, nil
}

// ListPendingDeletions implements restic.Repository.
func (s *Store) ListPendingDeletions(ctx context.Context, startedBefore time.Time, limit int) ([]restic.PendingDeletion, error) {
	const op = "list pending deletions"
	rows, err := s.q.ListPendingDeletions(ctx, db.ListPendingDeletionsParams{StartedBefore: pgTime(startedBefore), RowLimit: limitArg(limit)})
	if err != nil {
		return nil, mapErr(op, err)
	}
	out := make([]restic.PendingDeletion, 0, len(rows))
	for _, row := range rows {
		p := restic.PendingDeletion{RequestedAt: timeFrom(row.DeletionRequestedAt)}
		if row.JobStatus != nil {
			p.JobStatus = domain.JobStatus(*row.JobStatus)
		}
		if row.JobErrorCode != nil {
			p.JobErrorCode = *row.JobErrorCode
		}
		if row.JobErrorMessage != nil {
			p.JobErrorMessage = *row.JobErrorMessage
		}
		if err := errors.Join(decodeID(&p.RepoID, row.ID), decodeID(&p.OwnerID, row.OwnerID), decodeID(&p.JobID, row.DeletionJobID)); err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// FinishDeletion implements restic.Repository: a compare-and-swap on the
// repository deleting with jobID.
func (s *Store) FinishDeletion(ctx context.Context, id domain.ResticRepoID, jobID domain.JobID, o restic.DeletionOutcome, at time.Time) error {
	op := fmt.Sprintf("finish deletion of repository %s by job %s", id, jobID)
	lost := fmt.Errorf("%s: not deleting with that job: %w", op, domain.ErrConflict)
	if !o.Succeeded {
		n, err := s.q.FinishRepositoryDeletionFailed(ctx, db.FinishRepositoryDeletionFailedParams{
			ID: pgID(id), JobID: pgID(jobID), At: pgTime(at), Message: clipRunes(cleanText(o.Error), restic.MaxDeletionErrorLen), Cancelled: o.Cancelled,
		})
		if err != nil {
			return mapErr(op, err)
		}
		if n == 0 {
			return lost
		}
		return nil
	}
	return s.inTx(ctx, op, func(q *db.Queries) error {
		n, err := q.FinishRepositoryDeletionSucceeded(ctx, db.FinishRepositoryDeletionSucceededParams{ID: pgID(id), JobID: pgID(jobID), At: pgTime(at)})
		if err != nil {
			return mapErr(op, err)
		}
		if n == 0 {
			return lost
		}
		return mapErr(op, q.DeleteRepositorySnapshots(ctx, pgID(id)))
	})
}

// clipRunes returns s cut to at most n runes.
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// ReplaceSnapshots implements restic.Repository. The repository row is
// locked for the transaction, so concurrent replacements run one after
// the other and the cache is always one of the sets.
func (s *Store) ReplaceSnapshots(ctx context.Context, id domain.ResticRepoID, snaps []restic.Snapshot, truncated bool, refreshedAt time.Time, mark restic.ChangeMark) error {
	op := fmt.Sprintf("replace snapshots of repository %s", id)
	rows := make([]db.InsertSnapshotsParams, 0, len(snaps))
	seen := make(map[string]bool, len(snaps))
	for _, sn := range snaps {
		// The schema checks both too; checking here names the problem and
		// keeps the copy from failing half-way.
		switch {
		case !isHex64(sn.ID):
			return fmt.Errorf("%s: %w: a snapshot ID is not 64 hex digits", op, domain.ErrInvalidArgument)
		case seen[sn.ID]:
			return fmt.Errorf("%s: snapshot %s: %w", op, sn.ID, domain.ErrAlreadyExists)
		}
		seen[sn.ID] = true
		summary, err := encodeSummary(sn.Summary)
		if err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
		rows = append(rows, db.InsertSnapshotsParams{
			RepositoryID:   pgID(id),
			SnapshotID:     sn.ID,
			TakenAt:        pgTime(sn.Time),
			Hostname:       cleanText(sn.Hostname),
			Username:       cleanText(sn.Username),
			Paths:          textArray(cleanTexts(sn.Paths)),
			Tags:           textArray(cleanTexts(sn.Tags)),
			Parent:         cleanText(sn.Parent),
			ProgramVersion: cleanText(sn.ProgramVersion),
			Summary:        summary,
		})
	}
	return s.inTx(ctx, op, func(q *db.Queries) error {
		if _, err := q.LockLiveRepository(ctx, pgID(id)); err != nil {
			return mapErr(op, err) // no row: missing or deleted
		}
		if err := q.DeleteRepositorySnapshots(ctx, pgID(id)); err != nil {
			return mapErr(op, err)
		}
		if len(rows) > 0 {
			if _, err := q.InsertSnapshots(ctx, rows); err != nil {
				return mapErr(op, err)
			}
		}
		n, err := q.SetSnapshotsRefreshed(ctx, db.SetSnapshotsRefreshedParams{
			ID: pgID(id), RefreshedAt: pgTime(refreshedAt), Truncated: truncated,
			MarkJobs: max(mark.Jobs, 0), MarkFinishedAt: pgOptTime(mark.LastFinishedAt),
		})
		if err != nil {
			return mapErr(op, err)
		}
		if n == 0 {
			return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
		}
		return nil
	})
}

// ListSnapshots implements restic.Repository.
func (s *Store) ListSnapshots(ctx context.Context, id domain.ResticRepoID) (restic.SnapshotList, error) {
	op := fmt.Sprintf("list snapshots of repository %s", id)
	st, err := s.q.GetSnapshotsState(ctx, pgID(id))
	if err != nil {
		return restic.SnapshotList{}, mapErr(op, err)
	}
	rows, err := s.q.ListSnapshots(ctx, pgID(id))
	if err != nil {
		return restic.SnapshotList{}, mapErr(op, err)
	}
	cur, err := s.q.RepositoryChangeMark(ctx, db.RepositoryChangeMarkParams{RepositoryID: pgID(id), Kinds: restic.MutatingKinds()})
	if err != nil {
		return restic.SnapshotList{}, mapErr(op, err)
	}
	out := restic.SnapshotList{
		Snapshots:   make([]restic.Snapshot, 0, len(rows)),
		RefreshedAt: timeFrom(st.SnapshotsRefreshedAt),
		Truncated:   st.SnapshotsTruncated,
		Mark:        restic.ChangeMark{Jobs: st.SnapshotsMarkJobs, LastFinishedAt: timeFrom(st.SnapshotsMarkFinishedAt)},
		CurrentMark: restic.ChangeMark{Jobs: cur.ChangeJobs, LastFinishedAt: timeFrom(cur.ChangeFinishedAt)},
	}
	for _, row := range rows {
		sn := restic.Snapshot{
			ID: row.SnapshotID, Time: timeFrom(row.TakenAt), Hostname: row.Hostname, Username: row.Username,
			Parent: row.Parent, ProgramVersion: row.ProgramVersion, Paths: nilIfEmpty(row.Paths), Tags: nilIfEmpty(row.Tags),
		}
		if sn.Summary, err = decodeSummary(row.Summary); err != nil {
			return restic.SnapshotList{}, fmt.Errorf("%s: snapshot %s: %w", op, row.SnapshotID, err)
		}
		out.Snapshots = append(out.Snapshots, sn)
	}
	return out, nil
}

// isHex64 reports whether s is 64 lower-case hex digits.
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// summaryDoc is the stored form of restic.SnapshotSummary
// (restic_snapshots.summary jsonb), with the field names restic uses in a
// snapshot's summary: the statistics query reads total_bytes_processed
// from it.
type summaryDoc struct {
	BackupStart         time.Time `json:"backup_start,omitzero"`
	BackupEnd           time.Time `json:"backup_end,omitzero"`
	FilesNew            int64     `json:"files_new"`
	FilesChanged        int64     `json:"files_changed"`
	FilesUnmodified     int64     `json:"files_unmodified"`
	DirsNew             int64     `json:"dirs_new"`
	DirsChanged         int64     `json:"dirs_changed"`
	DirsUnmodified      int64     `json:"dirs_unmodified"`
	DataBlobs           int64     `json:"data_blobs"`
	TreeBlobs           int64     `json:"tree_blobs"`
	DataAdded           int64     `json:"data_added"`
	DataAddedPacked     int64     `json:"data_added_packed"`
	TotalFilesProcessed int64     `json:"total_files_processed"`
	TotalBytesProcessed int64     `json:"total_bytes_processed"`
}

// encodeSummary encodes a snapshot summary; nil is SQL NULL.
func encodeSummary(sum *restic.SnapshotSummary) ([]byte, error) {
	if sum == nil {
		return nil, nil
	}
	b, err := json.Marshal(summaryDoc{
		BackupStart: sum.BackupStart.UTC(), BackupEnd: sum.BackupEnd.UTC(),
		FilesNew: sum.FilesNew, FilesChanged: sum.FilesChanged, FilesUnmodified: sum.FilesUnmodified,
		DirsNew: sum.DirsNew, DirsChanged: sum.DirsChanged, DirsUnmodified: sum.DirsUnmodified,
		DataBlobs: sum.DataBlobs, TreeBlobs: sum.TreeBlobs, DataAdded: sum.DataAdded, DataAddedPacked: sum.DataAddedPacked,
		TotalFilesProcessed: sum.TotalFilesProcessed, TotalBytesProcessed: sum.TotalBytesProcessed,
	})
	if err != nil {
		return nil, fmt.Errorf("encode snapshot summary: %w", err)
	}
	return b, nil
}

// decodeSummary is the inverse of encodeSummary.
func decodeSummary(b []byte) (*restic.SnapshotSummary, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var doc summaryDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("decode snapshot summary: %w", err)
	}
	return &restic.SnapshotSummary{
		BackupStart: zeroOrUTC(doc.BackupStart), BackupEnd: zeroOrUTC(doc.BackupEnd),
		FilesNew: doc.FilesNew, FilesChanged: doc.FilesChanged, FilesUnmodified: doc.FilesUnmodified,
		DirsNew: doc.DirsNew, DirsChanged: doc.DirsChanged, DirsUnmodified: doc.DirsUnmodified,
		DataBlobs: doc.DataBlobs, TreeBlobs: doc.TreeBlobs, DataAdded: doc.DataAdded, DataAddedPacked: doc.DataAddedPacked,
		TotalFilesProcessed: doc.TotalFilesProcessed, TotalBytesProcessed: doc.TotalBytesProcessed,
	}, nil
}

func zeroOrUTC(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC()
}

// repoFrom converts a repository row, without statistics.
func repoFrom(row db.ResticRepository) (restic.Repo, error) {
	r := restic.Repo{
		Name:                row.Name,
		Description:         row.Description,
		Path:                row.Path,
		PasswordEnc:         row.PasswordEnc,
		ResticID:            row.ResticID,
		FormatVersion:       int(row.FormatVersion),
		Deleting:            row.Deleting,
		DeletionRequestedAt: timeFrom(row.DeletionRequestedAt),
		DeletionError:       row.DeletionError,
		DeletionCancelled:   row.DeletionCancelled,
		DataDeletedAt:       timeFrom(row.DataDeletedAt),
		CreatedAt:           timeFrom(row.CreatedAt),
		UpdatedAt:           timeFrom(row.UpdatedAt),
		DeletedAt:           timeFrom(row.DeletedAt),
		Version:             row.Version,
	}
	if err := errors.Join(
		decodeID(&r.ID, row.ID), decodeID(&r.OwnerID, row.OwnerID), decodeID(&r.RemoteID, row.RemoteID),
		decodeID(&r.DeletionJobID, row.DeletionJobID),
	); err != nil {
		return restic.Repo{}, err
	}
	return r, nil
}

// repoStats is a repository row with the statistics columns of
// GetRepository and ListRepositories.
type repoStats struct {
	repo                                      db.ResticRepository
	snapshotCount, scheduleCount              int32
	latestAt, changeFinishedAt                pgtype.Timestamptz
	latestBytes, changeJobs                   int64
	lastBackupID, lastCheckID                 pgtype.UUID
	lastBackupStatus, lastCheckStatus         string
	lastBackupCreatedAt, lastBackupFinishedAt pgtype.Timestamptz
	lastCheckCreatedAt, lastCheckFinishedAt   pgtype.Timestamptz
}

// repoStatsRow adapts the two row types, which have the same fields.
func repoStatsRow[R db.GetRepositoryRow | db.ListRepositoriesRow](row R) repoStats {
	g := db.GetRepositoryRow(row)
	return repoStats{
		repo: g.ResticRepository, snapshotCount: g.SnapshotCount, scheduleCount: g.ScheduleCount,
		latestAt: g.LatestAt, changeFinishedAt: g.ChangeFinishedAt, latestBytes: g.LatestBytes, changeJobs: g.ChangeJobs,
		lastBackupID: g.LastBackupID, lastCheckID: g.LastCheckID,
		lastBackupStatus: g.LastBackupStatus, lastCheckStatus: g.LastCheckStatus,
		lastBackupCreatedAt: g.LastBackupCreatedAt, lastBackupFinishedAt: g.LastBackupFinishedAt,
		lastCheckCreatedAt: g.LastCheckCreatedAt, lastCheckFinishedAt: g.LastCheckFinishedAt,
	}
}

// repoWithStats converts a repository row with its statistics.
func repoWithStats(row repoStats) (restic.Repo, error) {
	r, err := repoFrom(row.repo)
	if err != nil {
		return restic.Repo{}, err
	}
	r.Stats = restic.RepoStats{
		SnapshotCount:        int(row.snapshotCount),
		LatestSnapshotAt:     timeFrom(row.latestAt),
		LatestSnapshotBytes:  row.latestBytes,
		SnapshotsRefreshedAt: timeFrom(row.repo.SnapshotsRefreshedAt),
		SnapshotsTruncated:   row.repo.SnapshotsTruncated,
		SnapshotsMark:        restic.ChangeMark{Jobs: row.repo.SnapshotsMarkJobs, LastFinishedAt: timeFrom(row.repo.SnapshotsMarkFinishedAt)},
		ChangeMark:           restic.ChangeMark{Jobs: row.changeJobs, LastFinishedAt: timeFrom(row.changeFinishedAt)},
		ScheduleCount:        int(row.scheduleCount),
	}
	jobRef := func(dst *restic.JobRef, id pgtype.UUID, status string, created, finished pgtype.Timestamptz) error {
		if !id.Valid {
			return nil
		}
		*dst = restic.JobRef{Status: domain.JobStatus(status), CreatedAt: timeFrom(created), FinishedAt: timeFrom(finished)}
		return decodeID(&dst.ID, id)
	}
	if err := errors.Join(
		jobRef(&r.Stats.LastBackup, row.lastBackupID, row.lastBackupStatus, row.lastBackupCreatedAt, row.lastBackupFinishedAt),
		jobRef(&r.Stats.LastCheck, row.lastCheckID, row.lastCheckStatus, row.lastCheckCreatedAt, row.lastCheckFinishedAt),
	); err != nil {
		return restic.Repo{}, err
	}
	return r, nil
}
