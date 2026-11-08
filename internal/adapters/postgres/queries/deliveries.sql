-- name: PendingJobOutcomes :many
-- Terminal jobs that were not notified yet, with their worker's and
-- schedule's names (the predicate matches the partial index
-- jobs_notify_idx). A NULL row_limit means no limit.
SELECT j.id, j.owner_id, j.kind, j.status, j.worker_id, w.name AS worker_name,
       j.schedule_id, s.name AS schedule_name, j.error_code, j.error_message,
       j.stat_bytes, j.stat_files, j.stat_errors, j.started_at, j.finished_at
FROM jobs j
JOIN workers w ON w.id = j.worker_id
LEFT JOIN schedules s ON s.id = j.schedule_id
WHERE j.notified_at IS NULL AND j.status IN ('succeeded', 'failed', 'cancelled', 'interrupted')
ORDER BY j.finished_at NULLS FIRST, j.id
LIMIT sqlc.narg(row_limit)::bigint;

-- name: PendingRunOutcomes :many
-- Missed and failed runs that were not notified yet, with their schedule
-- (the predicate matches the partial index schedule_runs_notify_idx).
SELECT r.id, r.schedule_id, s.name AS schedule_name, s.owner_id, r.status, r.reason, r.message,
       r.scheduled_for, r.coalesced, r.worker_id, r.updated_at
FROM schedule_runs r
JOIN schedules s ON s.id = r.schedule_id
WHERE r.notified_at IS NULL AND r.status IN ('missed', 'failed')
ORDER BY r.updated_at, r.id
LIMIT sqlc.narg(row_limit)::bigint;

-- name: ScheduleSubscribers :many
-- The enabled contacts linked to each schedule, with the links' events.
SELECT sc.schedule_id, sc.contact_id, sc.events
FROM schedule_contacts sc
JOIN contacts c ON c.id = sc.contact_id
WHERE sc.schedule_id = ANY (sqlc.arg(schedule_ids)::uuid[]) AND c.enabled
ORDER BY sc.schedule_id, sc.contact_id;

-- name: ManualSubscribers :many
-- Each owner's enabled contacts that subscribed to manual jobs.
SELECT owner_id, id AS contact_id, job_events
FROM contacts
WHERE owner_id = ANY (sqlc.arg(owner_ids)::uuid[]) AND enabled AND cardinality(job_events) > 0
ORDER BY owner_id, id;

-- name: LockJobOutcome :one
-- Serializes the commits of one job's outcome. NO KEY UPDATE (what the
-- mark takes anyway) leaves the deliveries' foreign key checks unblocked.
SELECT id FROM jobs WHERE id = sqlc.arg(id)::uuid FOR NO KEY UPDATE;

-- name: LockRunOutcome :one
SELECT id FROM schedule_runs WHERE id = sqlc.arg(id)::uuid FOR NO KEY UPDATE;

-- name: InsertDelivery :execrows
-- Inserts a planned delivery unless its contact was deleted meanwhile (FOR
-- KEY SHARE makes a concurrent delete of the contact wait until this
-- transaction ends, or, if it committed first, find no row) or it was
-- planned already (the dedupe key).
INSERT INTO deliveries (
    id, contact_id, owner_id, event, dedupe_key, job_id, schedule_id, payload, status, attempts,
    max_attempts, next_attempt_at, lease_until, last_attempt_at, delivered_at, last_error, created_at
)
SELECT sqlc.arg(id)::uuid, sqlc.arg(contact_id)::uuid, sqlc.arg(owner_id)::uuid, sqlc.arg(event)::text,
       sqlc.arg(dedupe_key)::text, sqlc.narg(job_id)::uuid, sqlc.narg(schedule_id)::uuid, sqlc.arg(payload)::jsonb,
       sqlc.arg(status)::text, sqlc.arg(attempts)::integer, sqlc.arg(max_attempts)::integer,
       sqlc.arg(next_attempt_at)::timestamptz, sqlc.narg(lease_until)::timestamptz,
       sqlc.narg(last_attempt_at)::timestamptz, sqlc.narg(delivered_at)::timestamptz,
       sqlc.arg(last_error)::text, sqlc.arg(created_at)::timestamptz
WHERE EXISTS (SELECT 1 FROM contacts WHERE contacts.id = sqlc.arg(contact_id)::uuid FOR KEY SHARE)
ON CONFLICT (contact_id, dedupe_key) DO NOTHING;

-- name: MarkJobNotified :exec
UPDATE jobs SET notified_at = sqlc.arg(at)::timestamptz
WHERE id = sqlc.arg(id)::uuid AND notified_at IS NULL;

-- name: MarkRunNotified :exec
UPDATE schedule_runs SET notified_at = sqlc.arg(at)::timestamptz
WHERE id = sqlc.arg(id)::uuid AND notified_at IS NULL;

-- name: ClaimDeliveries :many
-- Claims due deliveries: pending ones whose attempt is due and sending ones
-- whose lease expired (their deliverer died). SKIP LOCKED makes concurrent
-- claims disjoint: rows another claim has locked are skipped, and a row it
-- claimed after this statement's snapshot fails the re-check of the
-- condition. The predicate matches the partial index deliveries_due_idx;
-- the CTE is materialized so the limit applies once. A NULL row_limit
-- means no limit.
WITH due AS MATERIALIZED (
    SELECT id FROM deliveries
    WHERE status IN ('pending', 'sending')
      AND ((status = 'pending' AND next_attempt_at <= sqlc.arg(now)::timestamptz)
        OR (status = 'sending' AND lease_until <= sqlc.arg(now)::timestamptz))
    ORDER BY next_attempt_at, id
    LIMIT sqlc.narg(row_limit)::bigint
    FOR UPDATE SKIP LOCKED
)
UPDATE deliveries d SET
    status = 'sending',
    attempts = d.attempts + 1,
    lease_until = sqlc.arg(lease_until)::timestamptz,
    last_attempt_at = sqlc.arg(now)::timestamptz
FROM due
WHERE d.id = due.id
RETURNING d.*;

-- name: CompleteDelivery :one
-- Records the outcome of a claimed attempt: a compare-and-swap on the
-- claimed attempt count, so a deliverer whose lease was reclaimed cannot
-- overwrite the newer attempt.
UPDATE deliveries SET
    status = sqlc.arg(status)::text,
    last_error = sqlc.arg(last_error)::text,
    lease_until = NULL,
    delivered_at = CASE WHEN sqlc.arg(status)::text = 'delivered' THEN sqlc.arg(at)::timestamptz ELSE delivered_at END,
    next_attempt_at = CASE WHEN sqlc.arg(status)::text = 'pending' THEN sqlc.arg(next_attempt_at)::timestamptz ELSE next_attempt_at END
WHERE id = sqlc.arg(id)::uuid AND status = 'sending' AND attempts = sqlc.arg(attempts)::integer
RETURNING *;

-- name: DeliveryExists :one
-- Tells a missing delivery from a stale claim or a wrong status after a
-- conditional update matched nothing.
SELECT EXISTS (SELECT 1 FROM deliveries WHERE id = sqlc.arg(id)::uuid)::boolean AS found;

-- name: GetDelivery :one
SELECT * FROM deliveries WHERE id = sqlc.arg(id)::uuid;

-- name: ListDeliveries :many
-- Every NULL or empty filter matches all deliveries; newest first (keyset
-- on id). A NULL row_limit means no limit.
SELECT * FROM deliveries
WHERE (sqlc.narg(owner_id)::uuid IS NULL OR owner_id = sqlc.narg(owner_id)::uuid)
  AND (sqlc.narg(contact_id)::uuid IS NULL OR contact_id = sqlc.narg(contact_id)::uuid)
  AND (cardinality(sqlc.arg(statuses)::text[]) = 0 OR status = ANY (sqlc.arg(statuses)::text[]))
  AND (sqlc.narg(before)::uuid IS NULL OR id < sqlc.narg(before)::uuid)
ORDER BY id DESC
LIMIT sqlc.narg(row_limit)::bigint;

-- name: RetryDelivery :one
UPDATE deliveries SET status = 'pending', attempts = 0, next_attempt_at = sqlc.arg(at)::timestamptz, lease_until = NULL
WHERE id = sqlc.arg(id)::uuid AND status = 'dead'
RETURNING *;

-- name: PurgeDeliveries :execrows
-- The predicate matches the partial index deliveries_purge_idx.
DELETE FROM deliveries
WHERE status IN ('delivered', 'dead') AND created_at < sqlc.arg(before)::timestamptz;
