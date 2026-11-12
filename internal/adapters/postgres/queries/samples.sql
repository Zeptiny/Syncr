-- name: AppendJobSample :exec
-- Idempotent on (job_id, t). A missing job violates the foreign key.
INSERT INTO job_samples (
    job_id, t, bytes, total_bytes, files, total_files, checks, deletes, renames, errors, elapsed_ns
) VALUES (
    sqlc.arg(job_id), sqlc.arg(t), sqlc.arg(bytes), sqlc.arg(total_bytes), sqlc.arg(files),
    sqlc.arg(total_files), sqlc.arg(checks), sqlc.arg(deletes), sqlc.arg(renames),
    sqlc.arg(errors), sqlc.arg(elapsed_ns)
)
ON CONFLICT (job_id, t) DO NOTHING;

-- name: LatestJobSample :one
SELECT * FROM job_samples WHERE job_id = sqlc.arg(job_id) ORDER BY t DESC LIMIT 1;

-- name: LatestJobSamples :many
-- The latest sample of each job in job_ids that has one: one primary key
-- probe per job (LIMIT 1 under the lateral join), however many samples a
-- job has.
SELECT s.job_id, s.t, s.bytes, s.total_bytes, s.files, s.total_files, s.checks,
       s.deletes, s.renames, s.errors, s.elapsed_ns
FROM unnest(sqlc.arg(job_ids)::uuid[]) AS ids (job_id)
CROSS JOIN LATERAL (
    SELECT * FROM job_samples js WHERE js.job_id = ids.job_id ORDER BY js.t DESC LIMIT 1
) s;

-- name: ListJobSamples :many
-- The job's samples, oldest first, evenly thinned to max_points when it has
-- more (jobs.SampleRepository.ListJobSamples has the rule; max_points <= 0
-- keeps every sample), each with the job's sample count. The count is a
-- CTE of the same statement (one snapshot), so it agrees with the rows;
-- both read the primary key (job_id, t) and nothing else. The CASE keeps
-- the division from running when total - 1 may be 0 (OR does not promise
-- an evaluation order); ranks from 1 are positive, so integer division is
-- floor.
WITH counted AS (
    SELECT count(*) AS total FROM job_samples cs WHERE cs.job_id = sqlc.arg(job_id)::uuid
),
ranked AS (
    SELECT js.job_id, js.t, js.bytes, js.total_bytes, js.files, js.total_files, js.checks,
           js.deletes, js.renames, js.errors, js.elapsed_ns,
           row_number() OVER (ORDER BY js.t) - 1 AS rank
    FROM job_samples js
    WHERE js.job_id = sqlc.arg(job_id)::uuid
)
SELECT r.job_id, r.t, r.bytes, r.total_bytes, r.files, r.total_files, r.checks,
       r.deletes, r.renames, r.errors, r.elapsed_ns, c.total
FROM ranked r CROSS JOIN counted c
WHERE CASE
    WHEN sqlc.arg(max_points)::bigint <= 0 OR c.total <= sqlc.arg(max_points)::bigint THEN true
    WHEN r.rank = 0 THEN true
    ELSE r.rank * (sqlc.arg(max_points)::bigint - 1) / (c.total - 1)
        > (r.rank - 1) * (sqlc.arg(max_points)::bigint - 1) / (c.total - 1)
END
ORDER BY r.t;

-- name: PurgeJobSamples :one
-- Deletes the samples of at most max_jobs jobs that finished before
-- finished_before, come after the cursor (after_finished_at, after_id) in
-- (finished_at, id) order and still have samples, taken in that order, and
-- returns how many samples it deleted and the cursor of the last of those
-- jobs (NULL when there was none). jobs_finished_idx walks the finished
-- jobs from the cursor's finished_at on (the plain range condition is its
-- index bound; the row comparison then skips the cursor's own ties), so a
-- purge that goes on from the cursor never walks the jobs it purged
-- before. The EXISTS probe and the delete use the samples' primary key.
-- Unfinished jobs have no finished_at and are never matched.
WITH due AS (
    SELECT j.id, j.finished_at
    FROM jobs j
    WHERE j.finished_at < sqlc.arg(finished_before)::timestamptz
      AND j.finished_at >= sqlc.arg(after_finished_at)::timestamptz
      AND (j.finished_at, j.id) > (sqlc.arg(after_finished_at)::timestamptz, sqlc.arg(after_id)::uuid)
      AND EXISTS (SELECT 1 FROM job_samples s WHERE s.job_id = j.id)
    ORDER BY j.finished_at, j.id
    LIMIT sqlc.arg(max_jobs)::bigint
), deleted AS (
    DELETE FROM job_samples WHERE job_id IN (SELECT id FROM due)
    RETURNING 1
), last AS (
    SELECT id, finished_at FROM due ORDER BY finished_at DESC, id DESC LIMIT 1
)
SELECT
    (SELECT count(*) FROM deleted)::bigint AS deleted,
    (SELECT finished_at FROM last)::timestamptz AS last_finished_at,
    (SELECT id FROM last)::uuid AS last_id;
