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

-- name: PurgeJobSamples :execrows
-- Deletes the samples of at most max_jobs jobs that finished before
-- finished_before and still have samples, those that finished first first.
-- jobs_finished_idx walks the finished jobs in order; the EXISTS probe and
-- the delete use the samples' primary key. Unfinished jobs have no
-- finished_at and are never matched.
DELETE FROM job_samples
WHERE job_id IN (
    SELECT j.id
    FROM jobs j
    WHERE j.finished_at < sqlc.arg(finished_before)::timestamptz
      AND EXISTS (SELECT 1 FROM job_samples s WHERE s.job_id = j.id)
    ORDER BY j.finished_at
    LIMIT sqlc.arg(max_jobs)::bigint
);
