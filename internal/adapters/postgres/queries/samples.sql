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
SELECT * FROM job_samples WHERE job_id = sqlc.arg(job_id) ORDER BY t;
