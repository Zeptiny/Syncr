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

-- name: ListJobSamples :many
SELECT * FROM job_samples WHERE job_id = sqlc.arg(job_id) ORDER BY t;
