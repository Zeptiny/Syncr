-- +goose Up

-- The controller's /metrics counts every finished job by kind, worker,
-- status, error code and run time on each scrape (queries/activity.sql,
-- JobTotals). This index holds every column that count reads, so it is an
-- index-only scan of the finished jobs instead of a scan of the jobs' rows,
-- which carry the definition, the search text and the output. Keyed like
-- jobs_owner_finished_idx, it also serves one owner's ranges, so it
-- replaces that index.
CREATE INDEX jobs_totals_idx ON jobs (owner_id, finished_at)
    INCLUDE (kind, worker_id, status, error_code, stat_bytes, stat_files, created_at, dispatched_at, started_at)
    WHERE finished_at IS NOT NULL;
DROP INDEX jobs_owner_finished_idx;

-- +goose Down
CREATE INDEX jobs_owner_finished_idx ON jobs (owner_id, finished_at) WHERE finished_at IS NOT NULL;
DROP INDEX jobs_totals_idx;
