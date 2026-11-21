-- name: JobActivity :many
-- The time-zone form of date_trunc (PostgreSQL 12+) buckets by local hours
-- and days, so 23- and 25-hour DST days and +05:30 zones come out right.
-- jobs_finished_idx and jobs_totals_idx serve the range. Run times and
-- queue waits are whole microseconds (jobs.ActivityRepository).
SELECT date_trunc(sqlc.arg(unit)::text, finished_at, sqlc.arg(tz)::text)::timestamptz AS bucket,
       status,
       count(*)::bigint AS jobs,
       coalesce(sum(stat_bytes), 0)::bigint AS bytes,
       coalesce(sum(stat_files), 0)::bigint AS files,
       count(run_us)::bigint AS started,
       coalesce(sum(run_us), 0)::bigint AS run_us,
       count(wait_us)::bigint AS dispatched,
       coalesce(sum(wait_us), 0)::bigint AS wait_us
FROM (
    SELECT finished_at, status, stat_bytes, stat_files,
           CASE WHEN started_at IS NOT NULL AND finished_at >= started_at
                THEN (extract(epoch FROM finished_at - started_at) * 1000000)::bigint END AS run_us,
           CASE WHEN dispatched_at IS NOT NULL AND dispatched_at >= created_at
                THEN (extract(epoch FROM dispatched_at - created_at) * 1000000)::bigint END AS wait_us
    FROM jobs
    WHERE finished_at IS NOT NULL
      AND finished_at >= sqlc.arg(from_t) AND finished_at < sqlc.arg(until_t)
      AND (sqlc.narg(owner_id)::uuid IS NULL OR owner_id = sqlc.narg(owner_id)::uuid)
) j
GROUP BY 1, 2
ORDER BY 1, 2;

-- name: JobTotals :many
-- The finished jobs of the range (a NULL bound is open) by kind, worker,
-- status, error code and run-time bucket: the number of bounds (whole
-- microseconds, ascending) below the run time, so a bound counts the run
-- times at most it as Prometheus's "le" buckets do; -1 for jobs without a
-- run time. jobs_totals_idx holds every column read, so the metrics'
-- all-time totals are an index-only scan, not a scan of the jobs' rows.
SELECT kind, worker_id, status, error_code, run_bucket,
       count(*)::bigint AS jobs,
       coalesce(sum(stat_bytes), 0)::bigint AS bytes,
       coalesce(sum(stat_files), 0)::bigint AS files,
       count(run_us)::bigint AS started,
       coalesce(sum(run_us), 0)::bigint AS run_us,
       count(wait_us)::bigint AS dispatched,
       coalesce(sum(wait_us), 0)::bigint AS wait_us
FROM (
    SELECT kind, worker_id, status, error_code, stat_bytes, stat_files, run_us, wait_us,
           (CASE WHEN run_us IS NULL THEN -1
                 ELSE (SELECT count(*) FROM unnest(sqlc.arg(bounds_us)::bigint[]) AS b WHERE b < run_us)
            END)::integer AS run_bucket
    FROM (
        SELECT kind, worker_id, status, error_code, stat_bytes, stat_files,
               CASE WHEN started_at IS NOT NULL AND finished_at >= started_at
                    THEN (extract(epoch FROM finished_at - started_at) * 1000000)::bigint END AS run_us,
               CASE WHEN dispatched_at IS NOT NULL AND dispatched_at >= created_at
                    THEN (extract(epoch FROM dispatched_at - created_at) * 1000000)::bigint END AS wait_us
        FROM jobs
        WHERE finished_at IS NOT NULL
          AND (sqlc.narg(from_t)::timestamptz IS NULL OR finished_at >= sqlc.narg(from_t)::timestamptz)
          AND (sqlc.narg(until_t)::timestamptz IS NULL OR finished_at < sqlc.narg(until_t)::timestamptz)
          AND (sqlc.narg(owner_id)::uuid IS NULL OR owner_id = sqlc.narg(owner_id)::uuid)
    ) f
) j
GROUP BY kind, worker_id, status, error_code, run_bucket;

-- name: UnfinishedJobs :many
-- jobs_worker_unfinished_idx's predicate: the planner may read only the
-- unfinished jobs.
SELECT status, kind, worker_id, count(*)::bigint AS jobs, min(queued_at)::timestamptz AS oldest_queued_at
FROM jobs
WHERE status IN ('queued', 'dispatched', 'running')
  AND (sqlc.narg(owner_id)::uuid IS NULL OR owner_id = sqlc.narg(owner_id)::uuid)
GROUP BY status, kind, worker_id;
