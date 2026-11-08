-- name: JobActivity :many
-- The time-zone form of date_trunc (PostgreSQL 12+) buckets by local hours
-- and days, so 23- and 25-hour DST days and +05:30 zones come out right.
-- jobs_finished_idx and jobs_owner_finished_idx serve the range.
SELECT date_trunc(sqlc.arg(unit)::text, finished_at, sqlc.arg(tz)::text)::timestamptz AS bucket,
       status,
       count(*)::bigint AS jobs,
       coalesce(sum(stat_bytes), 0)::bigint AS bytes,
       coalesce(sum(stat_files), 0)::bigint AS files
FROM jobs
WHERE finished_at IS NOT NULL
  AND finished_at >= sqlc.arg(from_t) AND finished_at < sqlc.arg(until_t)
  AND (sqlc.narg(owner_id)::uuid IS NULL OR owner_id = sqlc.narg(owner_id)::uuid)
GROUP BY 1, 2
ORDER BY 1, 2;

-- name: CountUnfinishedJobs :many
SELECT status, count(*)::bigint AS jobs
FROM jobs
WHERE status IN ('queued', 'dispatched', 'running')
  AND (sqlc.narg(owner_id)::uuid IS NULL OR owner_id = sqlc.narg(owner_id)::uuid)
GROUP BY status
ORDER BY status;
