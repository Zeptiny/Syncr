-- +goose Up

-- The hooks a job's worker ran for it (jobs.Job.HookRuns), in the order they
-- ran: a JSON array of objects with name, phase (before, on_success,
-- on_failure, after), status (succeeded, failed, timed_out, cancelled),
-- exit_code, started_at, duration_ns, message, output (the end of what the
-- command printed, at most 4096 bytes) and output_truncated. Empty for a job
-- without hooks and until its worker has reported its result. Jobs that ran
-- before this column existed had no hooks.
--
-- A job has at most 32 runs (8 hooks in each of the 4 phases), so the column
-- stays small enough to be read with the rest of the row.
ALTER TABLE jobs ADD COLUMN hook_runs jsonb NOT NULL DEFAULT '[]'::jsonb;

-- +goose Down
ALTER TABLE jobs DROP COLUMN hook_runs;
