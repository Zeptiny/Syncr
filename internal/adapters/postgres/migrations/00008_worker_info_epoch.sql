-- +goose Up

-- The info document now names the epoch whose Hello it holds ("epoch"), so
-- the dispatcher can tell a new worker process's own Hello from the one its
-- predecessor sent (workers.Worker.InfoCurrent). Rows stored before carry
-- the Hello of the worker's current epoch: a worker sends its Hello first,
-- and a running worker does not send it again, so without this its jobs
-- would wait for a Hello until the worker restarts. A worker that never
-- connected (no epoch) keeps the enrollment info without an epoch.
UPDATE workers SET info = jsonb_set(info, '{epoch}', to_jsonb(epoch)) WHERE epoch <> '';

-- +goose Down
UPDATE workers SET info = info - 'epoch' WHERE info ? 'epoch';
