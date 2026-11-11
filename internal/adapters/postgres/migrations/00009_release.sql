-- +goose Up

-- Deleting a schedule sets deliveries.schedule_id to NULL (ON DELETE SET
-- NULL): without an index on the column that walks the whole delivery log,
-- which took 0.15 to 1.1 seconds per schedule at the measured sizes. Most
-- deliveries are of jobs run by hand, so the index is partial.
CREATE INDEX deliveries_schedule_idx ON deliveries (schedule_id) WHERE schedule_id IS NOT NULL;

-- The time limit a job's worker was sent when it accepted the job (its
-- definition's max_runtime, else the controller's jobs.default_max_runtime
-- of that moment), in nanoseconds; 0 means none. Job.time_limit shows it,
-- so a finished job keeps the limit it ran with when the default changes
-- later. Jobs dispatched before this column existed had no time limits.
ALTER TABLE jobs ADD COLUMN time_limit_ns bigint NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE jobs DROP COLUMN time_limit_ns;
DROP INDEX deliveries_schedule_idx;
