-- name: CreateJob :execrows
-- Inserts only if the worker exists and is not removed. FOR SHARE makes a
-- concurrent RemoveWorker wait until this insert commits, so the removal's
-- job release cannot miss the new job. The job's remotes follow in the same
-- transaction (InsertJobRemote).
INSERT INTO jobs (
    id, worker_id, owner_id, kind, status, definition, search_text, epoch, cancel_requested_at,
    created_at, queued_at, dispatched_at, started_at, finished_at, requeues,
    stat_bytes, stat_total_bytes, stat_files, stat_total_files, stat_checks,
    stat_deletes, stat_renames, stat_errors, stat_elapsed_ns,
    error_code, error_message, warnings, exit_code, output, version
)
SELECT
    sqlc.arg(id)::uuid, sqlc.arg(worker_id)::uuid, sqlc.arg(owner_id)::uuid, sqlc.arg(kind)::text,
    sqlc.arg(status)::text, sqlc.arg(definition)::jsonb, sqlc.arg(search_text)::text, sqlc.arg(epoch)::text,
    sqlc.narg(cancel_requested_at)::timestamptz,
    sqlc.arg(created_at)::timestamptz, sqlc.arg(queued_at)::timestamptz,
    sqlc.narg(dispatched_at)::timestamptz, sqlc.narg(started_at)::timestamptz,
    sqlc.narg(finished_at)::timestamptz, sqlc.arg(requeues)::integer,
    sqlc.arg(stat_bytes)::bigint, sqlc.arg(stat_total_bytes)::bigint, sqlc.arg(stat_files)::bigint,
    sqlc.arg(stat_total_files)::bigint, sqlc.arg(stat_checks)::bigint, sqlc.arg(stat_deletes)::bigint,
    sqlc.arg(stat_renames)::bigint, sqlc.arg(stat_errors)::bigint, sqlc.arg(stat_elapsed_ns)::bigint,
    sqlc.arg(error_code)::text, sqlc.arg(error_message)::text, sqlc.arg(warnings)::text[],
    sqlc.arg(exit_code)::integer, sqlc.narg(output)::bytea, sqlc.arg(version)::bigint
WHERE EXISTS (
    SELECT 1 FROM workers WHERE workers.id = sqlc.arg(worker_id)::uuid AND workers.removed_at IS NULL FOR SHARE
);

-- name: InsertJobRemote :exec
-- A remote a job's definition refers to. A missing remote (or job) violates
-- a foreign key.
INSERT INTO job_remotes (job_id, remote_id) VALUES (sqlc.arg(job_id), sqlc.arg(remote_id))
ON CONFLICT DO NOTHING;

-- name: GetJobRemoteIDs :many
-- The remotes of the jobs, by job then remote ID.
SELECT job_id, remote_id FROM job_remotes
WHERE job_id = ANY (sqlc.arg(job_ids)::uuid[])
ORDER BY job_id, remote_id;

-- name: GetJob :one
SELECT * FROM jobs WHERE id = sqlc.arg(id);

-- name: JobExists :one
SELECT EXISTS (SELECT 1 FROM jobs WHERE id = sqlc.arg(id));

-- name: UpdateJob :one
-- Optimistic update: writes every mutable column if the stored version is the
-- one the caller read. ID, worker, owner, kind, definition and creation time
-- never change.
UPDATE jobs SET
    status = sqlc.arg(status),
    epoch = sqlc.arg(epoch),
    cancel_requested_at = sqlc.arg(cancel_requested_at),
    queued_at = sqlc.arg(queued_at),
    dispatched_at = sqlc.arg(dispatched_at),
    started_at = sqlc.arg(started_at),
    finished_at = sqlc.arg(finished_at),
    requeues = sqlc.arg(requeues),
    stat_bytes = sqlc.arg(stat_bytes),
    stat_total_bytes = sqlc.arg(stat_total_bytes),
    stat_files = sqlc.arg(stat_files),
    stat_total_files = sqlc.arg(stat_total_files),
    stat_checks = sqlc.arg(stat_checks),
    stat_deletes = sqlc.arg(stat_deletes),
    stat_renames = sqlc.arg(stat_renames),
    stat_errors = sqlc.arg(stat_errors),
    stat_elapsed_ns = sqlc.arg(stat_elapsed_ns),
    error_code = sqlc.arg(error_code),
    error_message = sqlc.arg(error_message),
    warnings = sqlc.arg(warnings),
    exit_code = sqlc.arg(exit_code),
    output = sqlc.arg(output),
    version = version + 1
WHERE id = sqlc.arg(id) AND version = sqlc.arg(expected_version)
RETURNING *;

-- name: ListJobs :many
-- Every NULL or empty filter matches all jobs; a NULL row_limit means no
-- limit. query is a LIKE pattern body the caller escaped with backslashes,
-- so it matches as a plain substring of search_text (a pg_trgm index is
-- deferred). The owner filter matches jobs_owner_idx, the remote filter
-- job_remotes_remote_idx.
SELECT * FROM jobs
WHERE (sqlc.narg(owner_id)::uuid IS NULL OR owner_id = sqlc.narg(owner_id)::uuid)
  AND (sqlc.narg(worker_id)::uuid IS NULL OR worker_id = sqlc.narg(worker_id)::uuid)
  AND (cardinality(sqlc.arg(statuses)::text[]) = 0 OR status = ANY (sqlc.arg(statuses)::text[]))
  AND (cardinality(sqlc.arg(kinds)::text[]) = 0 OR kind = ANY (sqlc.arg(kinds)::text[]))
  AND (sqlc.narg(remote_id)::uuid IS NULL OR EXISTS (
      SELECT 1 FROM job_remotes
      WHERE job_remotes.job_id = jobs.id AND job_remotes.remote_id = sqlc.narg(remote_id)::uuid
  ))
  AND (sqlc.narg(query)::text IS NULL OR search_text LIKE '%' || sqlc.narg(query)::text || '%' ESCAPE '\')
  AND (sqlc.narg(created_after)::timestamptz IS NULL OR created_at >= sqlc.narg(created_after)::timestamptz)
  AND (sqlc.narg(created_before)::timestamptz IS NULL OR created_at < sqlc.narg(created_before)::timestamptz)
  AND (sqlc.narg(before)::uuid IS NULL OR id < sqlc.narg(before)::uuid)
ORDER BY id DESC
LIMIT sqlc.narg(row_limit)::bigint;

-- name: ListUnfinishedJobs :many
-- The predicate matches the partial index jobs_worker_unfinished_idx.
SELECT * FROM jobs
WHERE worker_id = sqlc.arg(worker_id) AND status IN ('queued', 'dispatched', 'running')
ORDER BY id;

-- name: ListQueuedJobs :many
-- A NULL row_limit means no limit.
SELECT * FROM jobs
WHERE status = 'queued' AND queued_at < sqlc.arg(before)::timestamptz
ORDER BY queued_at, id
LIMIT sqlc.narg(row_limit)::bigint;
