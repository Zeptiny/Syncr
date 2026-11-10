-- Restic repositories and their snapshot cache (migration 00007), for the Go
-- adapter in restic.go.

-- name: CreateRepository :execrows
-- Inserts a live repository that is not deleting (the statistics and the
-- deletion columns keep their defaults), only if the remote exists and is
-- not deleted. FOR SHARE makes a concurrent DeleteRemote (which locks the
-- remote FOR UPDATE and checks RepositoryUsingRemote) wait until this
-- insert commits. 0 rows: the remote is missing or deleted (ErrNotFound); a
-- unique violation is a taken name, location or restic ID
-- (ErrAlreadyExists), a foreign key violation a missing owner
-- (ErrNotFound).
INSERT INTO restic_repositories (
    id, owner_id, name, description, remote_id, path, password_enc, restic_id, format_version,
    created_at, updated_at, version
)
SELECT
    sqlc.arg(id)::uuid, sqlc.arg(owner_id)::uuid, sqlc.arg(name)::text, sqlc.arg(description)::text,
    sqlc.arg(remote_id)::uuid, sqlc.arg(path)::text, sqlc.arg(password_enc)::bytea, sqlc.arg(restic_id)::text,
    sqlc.arg(format_version)::integer, sqlc.arg(created_at)::timestamptz, sqlc.arg(updated_at)::timestamptz,
    sqlc.arg(version)::bigint
WHERE EXISTS (
    SELECT 1 FROM remotes WHERE remotes.id = sqlc.arg(remote_id)::uuid AND remotes.deleted_at IS NULL FOR SHARE
);

-- name: GetRepository :one
-- A repository (deleted ones included) with its statistics: the cached
-- snapshots, the newest backup and check jobs (by ID: UUIDv7, creation
-- order), the schedules that use it and its change mark (what makes the
-- snapshot cache stale, as RepositoryChangeMark). The LEFT JOIN LATERAL
-- columns may be NULL (no snapshot, no job): coalesce gives sqlc non-null
-- types, and the job's NULL id (Valid false) means "none".
SELECT sqlc.embed(r),
       coalesce(sn.snapshot_count, 0)::int AS snapshot_count, sn.latest_at,
       coalesce(lt.latest_bytes, 0)::bigint AS latest_bytes,
       lb.id AS last_backup_id, coalesce(lb.status, '')::text AS last_backup_status,
       lb.created_at AS last_backup_created_at, lb.finished_at AS last_backup_finished_at,
       lc.id AS last_check_id, coalesce(lc.status, '')::text AS last_check_status,
       lc.created_at AS last_check_created_at, lc.finished_at AS last_check_finished_at,
       coalesce(sc.schedule_count, 0)::int AS schedule_count,
       coalesce(ch.change_jobs, 0)::bigint AS change_jobs, ch.change_finished_at
FROM restic_repositories r
LEFT JOIN LATERAL (
    SELECT count(*)::int AS snapshot_count, max(s.taken_at)::timestamptz AS latest_at
    FROM restic_snapshots s WHERE s.repository_id = r.id
) sn ON true
LEFT JOIN LATERAL (
    SELECT (s.summary->>'total_bytes_processed')::bigint AS latest_bytes
    FROM restic_snapshots s WHERE s.repository_id = r.id
    ORDER BY s.taken_at DESC, s.snapshot_id LIMIT 1
) lt ON true
LEFT JOIN LATERAL (
    SELECT j.id, j.status, j.created_at, j.finished_at FROM jobs j
    WHERE j.repository_id = r.id AND j.kind = 'restic_backup' ORDER BY j.id DESC LIMIT 1
) lb ON true
LEFT JOIN LATERAL (
    SELECT j.id, j.status, j.created_at, j.finished_at FROM jobs j
    WHERE j.repository_id = r.id AND j.kind = 'restic_check' ORDER BY j.id DESC LIMIT 1
) lc ON true
LEFT JOIN LATERAL (
    SELECT count(*)::int AS schedule_count FROM schedules sch WHERE sch.repository_id = r.id
) sc ON true
LEFT JOIN LATERAL (
    SELECT count(*)::bigint AS change_jobs, max(j.finished_at)::timestamptz AS change_finished_at FROM jobs j
    WHERE j.repository_id = r.id AND j.status IN ('succeeded', 'failed', 'cancelled', 'interrupted')
      AND (j.dispatched_at IS NOT NULL OR j.started_at IS NOT NULL) AND j.kind = ANY (sqlc.arg(mutating_kinds)::text[])
) ch ON true
WHERE r.id = sqlc.arg(id)::uuid;

-- name: ListRepositories :many
-- Live repositories (deleting ones included) with their statistics (see
-- GetRepository). Every NULL filter matches all; the owner filter matches
-- restic_repositories_owner_idx.
SELECT sqlc.embed(r),
       coalesce(sn.snapshot_count, 0)::int AS snapshot_count, sn.latest_at,
       coalesce(lt.latest_bytes, 0)::bigint AS latest_bytes,
       lb.id AS last_backup_id, coalesce(lb.status, '')::text AS last_backup_status,
       lb.created_at AS last_backup_created_at, lb.finished_at AS last_backup_finished_at,
       lc.id AS last_check_id, coalesce(lc.status, '')::text AS last_check_status,
       lc.created_at AS last_check_created_at, lc.finished_at AS last_check_finished_at,
       coalesce(sc.schedule_count, 0)::int AS schedule_count,
       coalesce(ch.change_jobs, 0)::bigint AS change_jobs, ch.change_finished_at
FROM restic_repositories r
LEFT JOIN LATERAL (
    SELECT count(*)::int AS snapshot_count, max(s.taken_at)::timestamptz AS latest_at
    FROM restic_snapshots s WHERE s.repository_id = r.id
) sn ON true
LEFT JOIN LATERAL (
    SELECT (s.summary->>'total_bytes_processed')::bigint AS latest_bytes
    FROM restic_snapshots s WHERE s.repository_id = r.id
    ORDER BY s.taken_at DESC, s.snapshot_id LIMIT 1
) lt ON true
LEFT JOIN LATERAL (
    SELECT j.id, j.status, j.created_at, j.finished_at FROM jobs j
    WHERE j.repository_id = r.id AND j.kind = 'restic_backup' ORDER BY j.id DESC LIMIT 1
) lb ON true
LEFT JOIN LATERAL (
    SELECT j.id, j.status, j.created_at, j.finished_at FROM jobs j
    WHERE j.repository_id = r.id AND j.kind = 'restic_check' ORDER BY j.id DESC LIMIT 1
) lc ON true
LEFT JOIN LATERAL (
    SELECT count(*)::int AS schedule_count FROM schedules sch WHERE sch.repository_id = r.id
) sc ON true
LEFT JOIN LATERAL (
    SELECT count(*)::bigint AS change_jobs, max(j.finished_at)::timestamptz AS change_finished_at FROM jobs j
    WHERE j.repository_id = r.id AND j.status IN ('succeeded', 'failed', 'cancelled', 'interrupted')
      AND (j.dispatched_at IS NOT NULL OR j.started_at IS NOT NULL) AND j.kind = ANY (sqlc.arg(mutating_kinds)::text[])
) ch ON true
WHERE r.deleted_at IS NULL
  AND (sqlc.narg(owner_id)::uuid IS NULL OR r.owner_id = sqlc.narg(owner_id)::uuid)
  AND (sqlc.narg(remote_id)::uuid IS NULL OR r.remote_id = sqlc.narg(remote_id)::uuid)
ORDER BY lower(r.name) COLLATE "C", r.id;

-- name: GetRepositoryRow :one
-- A repository (deleted ones included) without statistics: ownership
-- checks and Resolve.
SELECT * FROM restic_repositories WHERE id = sqlc.arg(id)::uuid;

-- name: ListRepositoryRows :many
-- Live repositories (deleting ones included) without statistics, in the
-- order of ListRepositories: checks of names and locations.
SELECT * FROM restic_repositories r
WHERE r.deleted_at IS NULL
  AND (sqlc.narg(owner_id)::uuid IS NULL OR r.owner_id = sqlc.narg(owner_id)::uuid)
  AND (sqlc.narg(remote_id)::uuid IS NULL OR r.remote_id = sqlc.narg(remote_id)::uuid)
ORDER BY lower(r.name) COLLATE "C", r.id;

-- name: GetRepositories :many
-- The repositories of ids that exist (deleted ones included), by ID,
-- without statistics.
SELECT * FROM restic_repositories WHERE id = ANY (sqlc.arg(ids)::uuid[]) ORDER BY id;

-- name: GetRepositoryState :one
-- What tells the failures of the conditional updates apart: missing (no
-- row), deleted, or deleting.
SELECT deleted_at, deleting, deletion_job_id FROM restic_repositories WHERE id = sqlc.arg(id)::uuid;

-- name: UpdateRepository :one
-- Optimistic update of a live repository's name, description and password.
UPDATE restic_repositories SET
    name = sqlc.arg(name)::text,
    description = sqlc.arg(description)::text,
    password_enc = sqlc.narg(password_enc)::bytea,
    updated_at = sqlc.arg(updated_at)::timestamptz,
    version = version + 1
WHERE id = sqlc.arg(id)::uuid AND version = sqlc.arg(expected_version)::bigint AND deleted_at IS NULL
RETURNING *;

-- name: DeleteRepository :execrows
-- "Forget only": soft-deletes a live repository that is not deleting; the
-- snapshot cache goes in the same transaction (DeleteRepositorySnapshots).
-- 0 rows: GetRepositoryState tells NotFound from FailedPrecondition.
UPDATE restic_repositories SET
    deleted_at = sqlc.arg(at)::timestamptz, updated_at = sqlc.arg(at)::timestamptz,
    password_enc = NULL, version = version + 1
WHERE id = sqlc.arg(id)::uuid AND deleted_at IS NULL AND NOT deleting;

-- name: MarkRepositoryDeleting :execrows
-- Starts a data deletion; MarkDeleting runs it, PauseRepositorySchedules
-- and CancelRepositoryOpenRuns in one transaction. 0 rows:
-- GetRepositoryState tells NotFound from Conflict (already deleting).
UPDATE restic_repositories SET
    deleting = true, deletion_job_id = sqlc.arg(job_id)::uuid, deletion_requested_at = sqlc.arg(at)::timestamptz,
    deletion_error = '', deletion_cancelled = false, updated_at = sqlc.arg(at)::timestamptz, version = version + 1
WHERE id = sqlc.arg(id)::uuid AND deleted_at IS NULL AND NOT deleting;

-- name: PauseRepositorySchedules :exec
-- Pauses the enabled schedules of a repository whose data is being
-- deleted.
UPDATE schedules SET
    enabled = false, next_run_at = NULL, paused_reason = 'repository_deleting',
    updated_at = sqlc.arg(at)::timestamptz, version = version + 1
WHERE repository_id = sqlc.arg(id)::uuid AND enabled;

-- name: CancelRepositoryOpenRuns :exec
-- Cancels the open runs of the repository's schedules (they are paused in
-- the same transaction).
UPDATE schedule_runs SET
    status = 'cancelled', reason = 'repository_deleting', message = '', updated_at = sqlc.arg(at)::timestamptz
WHERE status IN ('pending', 'launching')
  AND schedule_id IN (SELECT sch.id FROM schedules sch WHERE sch.repository_id = sqlc.arg(id)::uuid);

-- name: ListPendingDeletions :many
-- Deleting repositories whose deletion job is terminal, or was never
-- created and was requested before started_before (index
-- restic_repositories_deleting_idx). A NULL row_limit means no limit.
SELECT r.id, r.owner_id, r.deletion_job_id, r.deletion_requested_at,
       j.status AS job_status, j.error_code AS job_error_code, j.error_message AS job_error_message
FROM restic_repositories r
LEFT JOIN jobs j ON j.id = r.deletion_job_id
WHERE r.deleting
  AND ((j.id IS NOT NULL AND j.status IN ('succeeded', 'failed', 'cancelled', 'interrupted'))
       OR (j.id IS NULL AND r.deletion_requested_at < sqlc.arg(started_before)::timestamptz))
ORDER BY r.deletion_requested_at, r.id
LIMIT sqlc.narg(row_limit)::bigint;

-- name: FinishRepositoryDeletionSucceeded :execrows
-- The data is gone: soft-delete the row and wipe the password; the
-- snapshot cache goes in the same transaction (DeleteRepositorySnapshots).
-- 0 rows: ErrConflict (not deleting, or another attempt's job).
UPDATE restic_repositories SET
    deleted_at = sqlc.arg(at)::timestamptz, data_deleted_at = sqlc.arg(at)::timestamptz,
    password_enc = NULL, deleting = false, updated_at = sqlc.arg(at)::timestamptz, version = version + 1
WHERE id = sqlc.arg(id)::uuid AND deleting AND deletion_job_id = sqlc.arg(job_id)::uuid;

-- name: FinishRepositoryDeletionFailed :execrows
-- The deletion failed or was cancelled: the repository is active again
-- with the error. 0 rows: ErrConflict.
UPDATE restic_repositories SET
    deleting = false, deletion_error = sqlc.arg(message)::text, deletion_cancelled = sqlc.arg(cancelled)::boolean,
    updated_at = sqlc.arg(at)::timestamptz,
    version = version + 1
WHERE id = sqlc.arg(id)::uuid AND deleting AND deletion_job_id = sqlc.arg(job_id)::uuid;

-- name: LockLiveRepository :one
-- Locks a live repository for the rest of the transaction
-- (ReplaceSnapshots), so concurrent replaces serialize.
SELECT id FROM restic_repositories WHERE id = sqlc.arg(id)::uuid AND deleted_at IS NULL FOR UPDATE;

-- name: DeleteRepositorySnapshots :exec
DELETE FROM restic_snapshots WHERE repository_id = sqlc.arg(repository_id)::uuid;

-- name: InsertSnapshots :copyfrom
INSERT INTO restic_snapshots (
    repository_id, snapshot_id, taken_at, hostname, username, paths, tags, parent, program_version, summary
) VALUES (
    sqlc.arg(repository_id), sqlc.arg(snapshot_id), sqlc.arg(taken_at), sqlc.arg(hostname), sqlc.arg(username),
    sqlc.arg(paths), sqlc.arg(tags), sqlc.arg(parent), sqlc.arg(program_version), sqlc.arg(summary)
);

-- name: SetSnapshotsRefreshed :execrows
-- Records a cache replacement of a live repository, with the change mark
-- read before the listing.
UPDATE restic_repositories SET
    snapshots_refreshed_at = sqlc.arg(refreshed_at)::timestamptz, snapshots_truncated = sqlc.arg(truncated)::boolean,
    snapshots_mark_jobs = sqlc.arg(mark_jobs)::bigint, snapshots_mark_finished_at = sqlc.narg(mark_finished_at)::timestamptz
WHERE id = sqlc.arg(id)::uuid AND deleted_at IS NULL;

-- name: GetSnapshotsState :one
-- The cache's refresh time, truncation and change mark (ListSnapshots).
SELECT snapshots_refreshed_at, snapshots_truncated, snapshots_mark_jobs, snapshots_mark_finished_at
FROM restic_repositories WHERE id = sqlc.arg(id)::uuid;

-- name: ListSnapshots :many
-- The snapshot cache, newest first (index restic_snapshots_time_idx).
SELECT * FROM restic_snapshots WHERE repository_id = sqlc.arg(repository_id)::uuid
ORDER BY taken_at DESC, snapshot_id;

-- name: RepositoryChangeMark :one
-- The repository's change mark (restic.ChangeMark, restic.IsChange): the
-- jobs of kinds (the mutating kinds) on it that ended after a worker had
-- accepted them, and their newest finish (NULL if none). Index
-- jobs_repository_idx.
SELECT count(*)::bigint AS change_jobs, max(finished_at)::timestamptz AS change_finished_at FROM jobs
WHERE repository_id = sqlc.arg(repository_id)::uuid AND status IN ('succeeded', 'failed', 'cancelled', 'interrupted')
  AND (dispatched_at IS NOT NULL OR started_at IS NOT NULL) AND kind = ANY (sqlc.arg(kinds)::text[]);
