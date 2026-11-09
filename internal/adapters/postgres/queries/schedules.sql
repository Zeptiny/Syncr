-- name: CreateSchedule :exec
INSERT INTO schedules (
    id, owner_id, name, description, cron, timezone, kind, definition, search_text,
    worker_id, worker_label, missed_action, max_delay_ns, overlap, enabled, paused_reason,
    next_run_at, last_fired_at, created_at, updated_at, version
) VALUES (
    sqlc.arg(id)::uuid, sqlc.arg(owner_id)::uuid, sqlc.arg(name)::text, sqlc.arg(description)::text,
    sqlc.arg(cron)::text, sqlc.arg(timezone)::text, sqlc.arg(kind)::text, sqlc.arg(definition)::jsonb,
    sqlc.arg(search_text)::text, sqlc.narg(worker_id)::uuid, sqlc.narg(worker_label)::text,
    sqlc.arg(missed_action)::text, sqlc.arg(max_delay_ns)::bigint, sqlc.arg(overlap)::text,
    sqlc.arg(enabled)::boolean, sqlc.arg(paused_reason)::text,
    sqlc.narg(next_run_at)::timestamptz, sqlc.narg(last_fired_at)::timestamptz,
    sqlc.arg(created_at)::timestamptz, sqlc.arg(updated_at)::timestamptz, sqlc.arg(version)::bigint
);

-- name: InsertScheduleRemote :exec
-- A missing remote violates the foreign key (deleted remotes still exist).
INSERT INTO schedule_remotes (schedule_id, remote_id) VALUES (sqlc.arg(schedule_id)::uuid, sqlc.arg(remote_id)::uuid);

-- name: InsertScheduleContact :exec
-- A missing contact violates the foreign key, a contact linked twice the
-- primary key, events outside the schedule events the check.
INSERT INTO schedule_contacts (schedule_id, contact_id, events)
VALUES (sqlc.arg(schedule_id)::uuid, sqlc.arg(contact_id)::uuid, sqlc.arg(events)::text[]);

-- name: DeleteScheduleRemotes :exec
DELETE FROM schedule_remotes WHERE schedule_id = sqlc.arg(schedule_id)::uuid;

-- name: DeleteScheduleContacts :exec
DELETE FROM schedule_contacts WHERE schedule_id = sqlc.arg(schedule_id)::uuid;

-- name: ListScheduleRemotes :many
SELECT * FROM schedule_remotes
WHERE schedule_id = ANY (sqlc.arg(schedule_ids)::uuid[])
ORDER BY schedule_id, remote_id;

-- name: ListScheduleContacts :many
SELECT * FROM schedule_contacts
WHERE schedule_id = ANY (sqlc.arg(schedule_ids)::uuid[])
ORDER BY schedule_id, contact_id;

-- name: GetSchedule :one
SELECT * FROM schedules WHERE id = sqlc.arg(id)::uuid;

-- name: GetSchedules :many
SELECT * FROM schedules WHERE id = ANY (sqlc.arg(ids)::uuid[]) ORDER BY id;

-- name: ScheduleExists :one
SELECT EXISTS (SELECT 1 FROM schedules WHERE id = sqlc.arg(id)::uuid);

-- name: ListSchedules :many
-- Every NULL filter matches all schedules. The remote filter matches
-- schedule_remotes_remote_idx, the contact filter
-- schedule_contacts_contact_idx.
SELECT * FROM schedules
WHERE (sqlc.narg(owner_id)::uuid IS NULL OR owner_id = sqlc.narg(owner_id)::uuid)
  AND (sqlc.narg(remote_id)::uuid IS NULL OR EXISTS (
      SELECT 1 FROM schedule_remotes sr
      WHERE sr.schedule_id = schedules.id AND sr.remote_id = sqlc.narg(remote_id)::uuid
  ))
  AND (sqlc.narg(contact_id)::uuid IS NULL OR EXISTS (
      SELECT 1 FROM schedule_contacts sc
      WHERE sc.schedule_id = schedules.id AND sc.contact_id = sqlc.narg(contact_id)::uuid
  ))
ORDER BY lower(name) COLLATE "C", id;

-- name: LockSchedule :one
-- Locks a schedule for the rest of the transaction (UpdateSchedule): the
-- version check and the enabled transition are decided on this row.
SELECT enabled, version FROM schedules WHERE id = sqlc.arg(id)::uuid FOR UPDATE;

-- name: LockScheduleRow :one
-- Locks a schedule before its runs are written (AddRun), so every
-- transaction that writes a schedule and its runs locks the schedule row
-- first and two of them cannot deadlock.
SELECT id FROM schedules WHERE id = sqlc.arg(id)::uuid FOR NO KEY UPDATE;

-- name: LockRunSchedule :one
-- Locks the schedule of a run before the run is written (UpdateRun), for
-- the same lock order; the run row itself is not locked. No row: the run
-- does not exist.
SELECT s.id FROM schedules s
WHERE s.id = (SELECT r.schedule_id FROM schedule_runs r WHERE r.id = sqlc.arg(run_id)::uuid)
FOR NO KEY UPDATE OF s;

-- name: UpdateSchedule :one
-- The configuration of a schedule locked by LockSchedule. next_run_at is
-- only written when reschedule is set, so an edit racing a fire cannot
-- rewind it; last_fired_at, owner_id and created_at are never written.
UPDATE schedules SET
    name = sqlc.arg(name)::text,
    description = sqlc.arg(description)::text,
    cron = sqlc.arg(cron)::text,
    timezone = sqlc.arg(timezone)::text,
    kind = sqlc.arg(kind)::text,
    definition = sqlc.arg(definition)::jsonb,
    search_text = sqlc.arg(search_text)::text,
    worker_id = sqlc.narg(worker_id)::uuid,
    worker_label = sqlc.narg(worker_label)::text,
    missed_action = sqlc.arg(missed_action)::text,
    max_delay_ns = sqlc.arg(max_delay_ns)::bigint,
    overlap = sqlc.arg(overlap)::text,
    enabled = sqlc.arg(enabled)::boolean,
    paused_reason = sqlc.arg(paused_reason)::text,
    next_run_at = CASE WHEN sqlc.arg(reschedule)::boolean THEN sqlc.narg(next_run_at)::timestamptz ELSE next_run_at END,
    updated_at = sqlc.arg(updated_at)::timestamptz,
    version = version + 1
WHERE id = sqlc.arg(id)::uuid AND version = sqlc.arg(expected_version)::bigint
RETURNING *;

-- name: SetScheduleEnabled :one
-- Pauses (next_run_at NULL) or resumes a schedule, without a version check.
UPDATE schedules SET
    enabled = sqlc.arg(enabled)::boolean,
    next_run_at = sqlc.narg(next_run_at)::timestamptz,
    paused_reason = sqlc.arg(paused_reason)::text,
    updated_at = sqlc.arg(updated_at)::timestamptz,
    version = version + 1
WHERE id = sqlc.arg(id)::uuid
RETURNING *;

-- name: PauseSchedule :exec
-- The auto-pause of UpdateRun.
UPDATE schedules SET
    enabled = false, next_run_at = NULL, paused_reason = sqlc.arg(paused_reason)::text,
    updated_at = sqlc.arg(updated_at)::timestamptz, version = version + 1
WHERE id = sqlc.arg(id)::uuid;

-- name: DeleteSchedule :execrows
-- Links and runs go with it (ON DELETE CASCADE); jobs and deliveries keep
-- existing with schedule_id cleared (ON DELETE SET NULL).
DELETE FROM schedules WHERE id = sqlc.arg(id)::uuid;

-- name: ListDueSchedules :many
-- The predicate matches the partial index schedules_due_idx.
SELECT * FROM schedules
WHERE enabled AND next_run_at IS NOT NULL AND next_run_at <= sqlc.arg(now)::timestamptz
ORDER BY next_run_at, id
LIMIT sqlc.narg(row_limit)::bigint;

-- name: AdvanceScheduleFire :execrows
-- The fire's compare-and-swap: concurrent fires of one occurrence wait for
-- the row lock, then find next_run_at changed and match nothing.
UPDATE schedules SET
    next_run_at = sqlc.narg(new_next)::timestamptz,
    last_fired_at = sqlc.arg(fired_at)::timestamptz
WHERE id = sqlc.arg(id)::uuid AND enabled AND next_run_at = sqlc.arg(expected_next)::timestamptz;

-- name: ListUpcoming :many
-- The predicate matches the partial indexes schedules_due_idx and, with an
-- owner, schedules_owner_next_idx.
SELECT id, name, next_run_at, worker_id, worker_label FROM schedules
WHERE enabled AND next_run_at IS NOT NULL
  AND (sqlc.narg(owner_id)::uuid IS NULL OR owner_id = sqlc.narg(owner_id)::uuid)
ORDER BY next_run_at, id
LIMIT sqlc.narg(row_limit)::bigint;

-- name: InsertRun :exec
-- A pending run while the schedule has one violates
-- schedule_runs_one_pending_key; a missing schedule or worker a foreign
-- key.
INSERT INTO schedule_runs (
    id, schedule_id, trigger, scheduled_for, deadline, coalesced, status, reason, message,
    worker_id, job_id, created_at, updated_at
) VALUES (
    sqlc.arg(id)::uuid, sqlc.arg(schedule_id)::uuid, sqlc.arg(trigger)::text,
    sqlc.arg(scheduled_for)::timestamptz, sqlc.arg(deadline)::timestamptz, sqlc.arg(coalesced)::integer,
    sqlc.arg(status)::text, sqlc.arg(reason)::text, sqlc.arg(message)::text,
    sqlc.narg(worker_id)::uuid, sqlc.narg(job_id)::uuid,
    sqlc.arg(created_at)::timestamptz, sqlc.arg(updated_at)::timestamptz
);

-- name: MergePendingRun :one
-- Merges an occurrence (or a manual request) into the schedule's pending
-- run, if it has one (at most one: schedule_runs_one_pending_key). A
-- launching run is past its launch and absorbs nothing. A fire passes
-- open_after (the fire time): only a run whose deadline is after it
-- absorbs the occurrence, and its deadline stays. A manual request passes
-- extend_to (its own deadline) instead: the run may then wait that long
-- (GREATEST ignores a NULL).
UPDATE schedule_runs SET
    coalesced = coalesced + sqlc.arg(add_coalesced)::integer + 1,
    deadline = GREATEST(deadline, sqlc.narg(extend_to)::timestamptz),
    updated_at = sqlc.arg(updated_at)::timestamptz
WHERE schedule_id = sqlc.arg(schedule_id)::uuid AND status = 'pending'
  AND (sqlc.narg(open_after)::timestamptz IS NULL OR deadline > sqlc.narg(open_after)::timestamptz)
RETURNING id;

-- name: SupersedePendingRun :exec
-- A fire ends the schedule's pending run whose deadline is not after the
-- fire time as missed, before inserting the occurrence's own run: the run
-- may not absorb an occurrence that is due after its deadline.
UPDATE schedule_runs SET
    status = 'missed', reason = sqlc.arg(reason)::text, message = sqlc.arg(message)::text,
    updated_at = sqlc.arg(updated_at)::timestamptz
WHERE schedule_id = sqlc.arg(schedule_id)::uuid AND status = 'pending'
  AND deadline <= sqlc.arg(updated_at)::timestamptz;

-- name: UpdateRunStatus :one
-- The compare-and-swap of a run's status; zero (NULL) worker and job IDs
-- keep the stored ones. A run back to pending while another one is
-- violates schedule_runs_one_pending_key. A launching run with a reason
-- (its schedule was paused while it launched) may only be cancelled.
UPDATE schedule_runs SET
    status = sqlc.arg(to_status)::text,
    reason = sqlc.arg(reason)::text,
    message = sqlc.arg(message)::text,
    worker_id = COALESCE(sqlc.narg(worker_id)::uuid, worker_id),
    job_id = COALESCE(sqlc.narg(job_id)::uuid, job_id),
    updated_at = sqlc.arg(updated_at)::timestamptz
WHERE id = sqlc.arg(id)::uuid AND status = sqlc.arg(from_status)::text
  AND (status <> 'launching' OR reason = '' OR sqlc.arg(to_status)::text = 'cancelled')
RETURNING schedule_id;

-- name: RunExists :one
SELECT EXISTS (SELECT 1 FROM schedule_runs WHERE id = sqlc.arg(id)::uuid);

-- name: CancelOpenRun :exec
-- Pausing a schedule cancels its open runs (without a notification: only
-- missed and failed runs notify). A pending run is cancelled at once; a
-- launching run's job may already exist, so it stays launching with the
-- reason 'disabled' (a cancel request): the scheduler cancels its job and
-- then the run.
UPDATE schedule_runs SET
    status = CASE WHEN status = 'pending' THEN 'cancelled' ELSE status END,
    reason = 'disabled', message = '', updated_at = sqlc.arg(updated_at)::timestamptz
WHERE schedule_id = sqlc.arg(schedule_id)::uuid
  AND (status = 'pending' OR (status = 'launching' AND reason = ''));

-- name: GetRun :one
-- Runs come with their job's status (job_id has no foreign key: a
-- launching run's job may not exist yet).
SELECT sqlc.embed(r), j.status AS job_status
FROM schedule_runs r LEFT JOIN jobs j ON j.id = r.job_id
WHERE r.id = sqlc.arg(id)::uuid;

-- name: ListOpenRuns :many
-- The predicate matches the partial index schedule_runs_open_idx.
SELECT sqlc.embed(r), j.status AS job_status
FROM schedule_runs r LEFT JOIN jobs j ON j.id = r.job_id
WHERE r.status IN ('pending', 'launching')
ORDER BY r.scheduled_for, r.id
LIMIT sqlc.narg(row_limit)::bigint;

-- name: ListRuns :many
-- Every NULL or empty filter matches all runs; a NULL row_limit means no
-- limit. The schedule filter matches schedule_runs_schedule_idx.
SELECT sqlc.embed(r), j.status AS job_status
FROM schedule_runs r LEFT JOIN jobs j ON j.id = r.job_id
WHERE (sqlc.narg(schedule_id)::uuid IS NULL OR r.schedule_id = sqlc.narg(schedule_id)::uuid)
  AND (sqlc.narg(owner_id)::uuid IS NULL OR EXISTS (
      SELECT 1 FROM schedules s WHERE s.id = r.schedule_id AND s.owner_id = sqlc.narg(owner_id)::uuid
  ))
  AND (cardinality(sqlc.arg(statuses)::text[]) = 0 OR r.status = ANY (sqlc.arg(statuses)::text[]))
  AND (sqlc.narg(scheduled_from)::timestamptz IS NULL OR r.scheduled_for >= sqlc.narg(scheduled_from)::timestamptz)
  AND (sqlc.narg(scheduled_until)::timestamptz IS NULL OR r.scheduled_for < sqlc.narg(scheduled_until)::timestamptz)
  AND (sqlc.narg(before)::uuid IS NULL OR r.id < sqlc.narg(before)::uuid)
ORDER BY r.id DESC
LIMIT sqlc.narg(row_limit)::bigint;

-- name: CountRuns :one
SELECT count(*)::bigint FROM schedule_runs r
WHERE (sqlc.narg(schedule_id)::uuid IS NULL OR r.schedule_id = sqlc.narg(schedule_id)::uuid)
  AND (sqlc.narg(owner_id)::uuid IS NULL OR EXISTS (
      SELECT 1 FROM schedules s WHERE s.id = r.schedule_id AND s.owner_id = sqlc.narg(owner_id)::uuid
  ))
  AND (cardinality(sqlc.arg(statuses)::text[]) = 0 OR r.status = ANY (sqlc.arg(statuses)::text[]))
  AND (sqlc.narg(scheduled_from)::timestamptz IS NULL OR r.scheduled_for >= sqlc.narg(scheduled_from)::timestamptz)
  AND (sqlc.narg(scheduled_until)::timestamptz IS NULL OR r.scheduled_for < sqlc.narg(scheduled_until)::timestamptz);

-- name: LatestRuns :many
-- The newest run of each schedule (schedule_runs_schedule_idx).
SELECT DISTINCT ON (r.schedule_id) sqlc.embed(r), j.status AS job_status
FROM schedule_runs r LEFT JOIN jobs j ON j.id = r.job_id
WHERE r.schedule_id = ANY (sqlc.arg(schedule_ids)::uuid[])
ORDER BY r.schedule_id, r.id DESC;

-- name: HasActiveRun :one
-- A launching run, or a job of the schedule that is not finished
-- (jobs_schedule_active_idx).
SELECT (
    EXISTS (SELECT 1 FROM schedule_runs WHERE schedule_id = sqlc.arg(schedule_id)::uuid AND status = 'launching')
    OR EXISTS (
        SELECT 1 FROM jobs
        WHERE schedule_id = sqlc.arg(schedule_id)::uuid AND status IN ('queued', 'dispatched', 'running')
    )
)::boolean AS active;
