-- name: CreateWorker :exec
INSERT INTO workers (
    id, name, mode, url, cert_fingerprint, secret_enc, info,
    epoch, epoch_started_at, past_epochs, ack_seq, last_seen, created_at, removed_at, labels
) VALUES (
    sqlc.arg(id), sqlc.arg(name), sqlc.arg(mode), sqlc.arg(url), sqlc.arg(cert_fingerprint),
    sqlc.arg(secret_enc), sqlc.arg(info), sqlc.arg(epoch), sqlc.arg(epoch_started_at),
    sqlc.arg(past_epochs), sqlc.arg(ack_seq), sqlc.arg(last_seen), sqlc.arg(created_at),
    sqlc.arg(removed_at), sqlc.arg(labels)
);

-- name: GetWorker :one
SELECT * FROM workers WHERE id = sqlc.arg(id);

-- name: WorkerIsActive :one
-- Distinguishes "missing or removed" from "condition failed" after a
-- conditional update matched no row.
SELECT EXISTS (SELECT 1 FROM workers WHERE id = sqlc.arg(id) AND removed_at IS NULL);

-- name: ListWorkers :many
SELECT * FROM workers
WHERE (sqlc.arg(mode)::text = '' OR mode = sqlc.arg(mode)::text)
  AND (sqlc.arg(include_removed)::boolean OR removed_at IS NULL)
  AND (sqlc.arg(label)::text = '' OR sqlc.arg(label)::text = ANY (labels))
ORDER BY id;

-- name: RenameWorker :one
UPDATE workers SET name = sqlc.arg(name)
WHERE id = sqlc.arg(id) AND removed_at IS NULL
RETURNING *;

-- name: SetWorkerLabels :one
UPDATE workers SET labels = sqlc.arg(labels)
WHERE id = sqlc.arg(id) AND removed_at IS NULL
RETURNING *;

-- name: RemoveWorker :one
UPDATE workers SET removed_at = sqlc.arg(removed_at), secret_enc = NULL
WHERE id = sqlc.arg(id) AND removed_at IS NULL
RETURNING *;

-- name: SetWorkerInfo :execrows
UPDATE workers SET info = sqlc.arg(info)
WHERE id = sqlc.arg(id) AND removed_at IS NULL AND epoch = sqlc.arg(epoch);

-- name: AdvanceWorkerEpoch :one
UPDATE workers SET
    epoch = sqlc.arg(to_epoch),
    epoch_started_at = sqlc.arg(started_at),
    past_epochs = sqlc.arg(past_epochs),
    ack_seq = 0
WHERE id = sqlc.arg(id) AND removed_at IS NULL AND epoch = sqlc.arg(from_epoch)
RETURNING *;

-- name: AdvanceWorkerAckSeq :one
UPDATE workers SET ack_seq = GREATEST(ack_seq, sqlc.arg(seq)::bigint)
WHERE id = sqlc.arg(id) AND removed_at IS NULL AND epoch = sqlc.arg(epoch)
RETURNING ack_seq;

-- name: TouchWorker :execrows
-- GREATEST ignores NULL, so a never-seen worker takes the first value.
UPDATE workers SET last_seen = GREATEST(last_seen, sqlc.arg(at)::timestamptz)
WHERE id = sqlc.arg(id) AND removed_at IS NULL;
