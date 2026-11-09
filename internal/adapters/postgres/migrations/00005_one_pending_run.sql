-- +goose Up

-- A schedule has at most one pending run (instead of one open run): an
-- occurrence due while a run is launching opens a new pending run, which
-- the overlap policy then holds (queue) or launches alongside (allow),
-- instead of merging into a run that is past launching. Launching runs are
-- not limited (overlap allow may launch several at once).
DROP INDEX schedule_runs_one_open_key;
CREATE UNIQUE INDEX schedule_runs_one_pending_key ON schedule_runs (schedule_id) WHERE status = 'pending';

-- +goose Down
DROP INDEX schedule_runs_one_pending_key;
CREATE UNIQUE INDEX schedule_runs_one_open_key ON schedule_runs (schedule_id) WHERE status IN ('pending', 'launching');
