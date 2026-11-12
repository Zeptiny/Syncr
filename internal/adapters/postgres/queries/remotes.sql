-- name: CreateRemote :exec
INSERT INTO remotes (
    id, owner_id, name, backend_type, options, secrets_enc, secret_keys,
    created_at, updated_at, deleted_at, version
) VALUES (
    sqlc.arg(id)::uuid, sqlc.arg(owner_id)::uuid, sqlc.arg(name)::text, sqlc.arg(backend_type)::text,
    sqlc.arg(options)::jsonb, sqlc.narg(secrets_enc)::bytea, sqlc.arg(secret_keys)::text[],
    sqlc.arg(created_at)::timestamptz, sqlc.arg(updated_at)::timestamptz,
    sqlc.narg(deleted_at)::timestamptz, sqlc.arg(version)::bigint
);

-- name: InsertUnionMember :execrows
-- Inserts only if the member remote exists and is not deleted. FOR SHARE
-- makes a concurrent SoftDeleteRemote of the member (which locks it FOR
-- UPDATE) wait until this transaction commits, and then see the member row.
INSERT INTO union_members (union_remote_id, ord, member_remote_id, path, mode)
SELECT sqlc.arg(union_remote_id)::uuid, sqlc.arg(ord)::integer, sqlc.arg(member_remote_id)::uuid,
       sqlc.arg(path)::text, sqlc.arg(mode)::text
WHERE EXISTS (
    SELECT 1 FROM remotes
    WHERE remotes.id = sqlc.arg(member_remote_id)::uuid AND remotes.deleted_at IS NULL
    FOR SHARE
);

-- name: DeleteUnionMembers :exec
DELETE FROM union_members WHERE union_remote_id = sqlc.arg(union_remote_id)::uuid;

-- name: GetRemote :one
SELECT * FROM remotes WHERE id = sqlc.arg(id)::uuid;

-- name: GetRemotes :many
SELECT * FROM remotes WHERE id = ANY(sqlc.arg(ids)::uuid[]) ORDER BY id;

-- name: ListUnionMembers :many
SELECT * FROM union_members
WHERE union_remote_id = ANY(sqlc.arg(union_ids)::uuid[])
ORDER BY union_remote_id, ord;

-- name: ListRemotes :many
SELECT * FROM remotes
WHERE deleted_at IS NULL
  AND (sqlc.narg(owner_id)::uuid IS NULL OR owner_id = sqlc.narg(owner_id)::uuid)
  AND (sqlc.narg(backend_type)::text IS NULL OR backend_type = sqlc.narg(backend_type)::text)
ORDER BY lower(name) COLLATE "C", id;

-- name: UpdateRemote :one
UPDATE remotes SET
    name = sqlc.arg(name)::text,
    options = sqlc.arg(options)::jsonb,
    secrets_enc = sqlc.narg(secrets_enc)::bytea,
    secret_keys = sqlc.arg(secret_keys)::text[],
    updated_at = sqlc.arg(updated_at)::timestamptz,
    version = version + 1
WHERE id = sqlc.arg(id)::uuid AND version = sqlc.arg(expected_version)::bigint AND deleted_at IS NULL
RETURNING *;

-- name: RemoteIsLive :one
-- Tells a missing or deleted remote (no row, or false) from a version
-- mismatch after UpdateRemote matched nothing.
SELECT (deleted_at IS NULL)::boolean AS live FROM remotes WHERE id = sqlc.arg(id)::uuid;

-- name: LockLiveRemote :one
-- Locks a live remote against concurrent member inserts (see
-- InsertUnionMember) before SoftDeleteRemote checks its unions.
SELECT id FROM remotes WHERE id = sqlc.arg(id)::uuid AND deleted_at IS NULL FOR UPDATE;

-- name: SoftDeleteRemote :exec
UPDATE remotes SET deleted_at = sqlc.arg(deleted_at)::timestamptz, secrets_enc = NULL, version = version + 1
WHERE id = sqlc.arg(id)::uuid AND deleted_at IS NULL;

-- name: ListUnionsUsing :many
SELECT r.* FROM remotes r
WHERE r.deleted_at IS NULL
  AND EXISTS (
      SELECT 1 FROM union_members m
      WHERE m.union_remote_id = r.id AND m.member_remote_id = sqlc.arg(member_remote_id)::uuid
  )
ORDER BY lower(r.name) COLLATE "C", r.id;

-- name: RepositoryUsingRemote :one
-- A live restic repository on the remote, which blocks its deletion
-- (DeleteRemote runs it in its transaction, after locking the remote).
SELECT name FROM restic_repositories
WHERE remote_id = sqlc.arg(remote_id)::uuid AND deleted_at IS NULL
ORDER BY lower(name) COLLATE "C", id
LIMIT 1;
