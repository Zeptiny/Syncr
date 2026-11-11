-- +goose Up

-- Deleting a schedule sets deliveries.schedule_id to NULL (ON DELETE SET
-- NULL): without an index on the column that walks the whole delivery log,
-- which took 0.15 to 1.1 seconds per schedule at the measured sizes. Most
-- deliveries are of jobs run by hand, so the index is partial.
CREATE INDEX deliveries_schedule_idx ON deliveries (schedule_id) WHERE schedule_id IS NOT NULL;

-- +goose Down
DROP INDEX deliveries_schedule_idx;
