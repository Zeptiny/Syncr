-- name: CreateContact :exec
INSERT INTO contacts (
    id, owner_id, name, channel_type, options, secrets_enc, secret_keys,
    summary, enabled, job_events, created_at, updated_at, version
) VALUES (
    sqlc.arg(id)::uuid, sqlc.arg(owner_id)::uuid, sqlc.arg(name)::text, sqlc.arg(channel_type)::text,
    sqlc.arg(options)::jsonb, sqlc.narg(secrets_enc)::bytea, sqlc.arg(secret_keys)::text[],
    sqlc.arg(summary)::text, sqlc.arg(enabled)::boolean, sqlc.arg(job_events)::text[],
    sqlc.arg(created_at)::timestamptz, sqlc.arg(updated_at)::timestamptz, sqlc.arg(version)::bigint
);

-- name: GetContact :one
SELECT * FROM contacts WHERE id = sqlc.arg(id)::uuid;

-- name: GetContacts :many
SELECT * FROM contacts WHERE id = ANY (sqlc.arg(ids)::uuid[]) ORDER BY id;

-- name: ListContacts :many
-- A NULL filter matches all contacts. The order is Go's: the lower-cased
-- name compared bytewise, then the ID.
SELECT * FROM contacts
WHERE (sqlc.narg(owner_id)::uuid IS NULL OR owner_id = sqlc.narg(owner_id)::uuid)
  AND (sqlc.narg(channel_type)::text IS NULL OR channel_type = sqlc.narg(channel_type)::text)
ORDER BY lower(name) COLLATE "C", id;

-- name: UpdateContact :one
UPDATE contacts SET
    name = sqlc.arg(name)::text,
    options = sqlc.arg(options)::jsonb,
    secrets_enc = sqlc.narg(secrets_enc)::bytea,
    secret_keys = sqlc.arg(secret_keys)::text[],
    summary = sqlc.arg(summary)::text,
    enabled = sqlc.arg(enabled)::boolean,
    job_events = sqlc.arg(job_events)::text[],
    updated_at = sqlc.arg(updated_at)::timestamptz,
    version = version + 1
WHERE id = sqlc.arg(id)::uuid AND version = sqlc.arg(expected_version)::bigint
RETURNING *;

-- name: ContactExists :one
-- Tells a missing contact from a version mismatch after UpdateContact
-- matched nothing.
SELECT EXISTS (SELECT 1 FROM contacts WHERE id = sqlc.arg(id)::uuid)::boolean AS found;

-- name: DeleteContact :execrows
-- The contact's schedule links and deliveries go with it (their foreign
-- keys are ON DELETE CASCADE).
DELETE FROM contacts WHERE id = sqlc.arg(id)::uuid;

-- name: ContactSchedules :many
-- The schedules linked to each contact, by lower-cased name, then ID.
SELECT sc.contact_id, s.id AS schedule_id, s.name AS schedule_name
FROM schedule_contacts sc
JOIN schedules s ON s.id = sc.schedule_id
WHERE sc.contact_id = ANY (sqlc.arg(contact_ids)::uuid[])
ORDER BY sc.contact_id, lower(s.name) COLLATE "C", s.id;
