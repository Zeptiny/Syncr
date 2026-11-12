-- +goose Up

-- The dashboard asks for problems, which are rare, in tables that keep
-- every run and job. Without an index on the problem rows these queries
-- walk the whole history: the fewer problems there are, the longer the
-- walk (a LIMIT only stops it once enough rows matched).

-- CountRuns of missed runs in a time range (the dashboard's missed total)
-- and ListRuns of missed and failed runs (its recent problems): the
-- queries' status filters imply the predicate.
CREATE INDEX schedule_runs_problem_idx ON schedule_runs (scheduled_for) WHERE status IN ('missed', 'failed');

-- ListJobs of failed and interrupted jobs, newest first (the dashboard's
-- recent failures, and the job list filtered by those statuses).
CREATE INDEX jobs_failed_idx ON jobs (id DESC) WHERE status IN ('failed', 'interrupted');

-- +goose Down
DROP INDEX jobs_failed_idx;
DROP INDEX schedule_runs_problem_idx;
