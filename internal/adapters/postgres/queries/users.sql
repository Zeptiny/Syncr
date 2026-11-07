-- Users ----------------------------------------------------------------------

-- name: CreateUser :exec
INSERT INTO users (
    id, username, display_name, role, password_hash, disabled_at,
    created_at, updated_at, password_changed_at, last_login_at, version
) VALUES (
    sqlc.arg(id), sqlc.arg(username), sqlc.arg(display_name), sqlc.arg(role), sqlc.arg(password_hash),
    sqlc.arg(disabled_at), sqlc.arg(created_at), sqlc.arg(updated_at), sqlc.arg(password_changed_at),
    sqlc.arg(last_login_at), sqlc.arg(version)
);

-- name: LockUsers :exec
-- Serializes CreateFirstUser transactions (released at commit or rollback).
SELECT pg_advisory_xact_lock(sqlc.arg(lock_id)::bigint);

-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: GetUser :one
SELECT * FROM users WHERE id = sqlc.arg(id);

-- name: GetUserByUsername :one
SELECT * FROM users WHERE username = sqlc.arg(username);

-- name: ListUsers :many
-- Byte order, like the in-memory store (usernames are ASCII).
SELECT * FROM users ORDER BY username COLLATE "C";

-- name: UpdateUser :one
UPDATE users SET
    display_name = sqlc.arg(display_name),
    role = sqlc.arg(role),
    password_hash = sqlc.arg(password_hash),
    disabled_at = sqlc.arg(disabled_at),
    password_changed_at = sqlc.arg(password_changed_at),
    last_login_at = sqlc.arg(last_login_at),
    updated_at = sqlc.arg(updated_at),
    version = version + 1
WHERE id = sqlc.arg(id) AND version = sqlc.arg(expected_version)
RETURNING *;

-- name: UserExists :one
-- Distinguishes "missing" from "version mismatch" after UpdateUser matched
-- no row.
SELECT EXISTS (SELECT 1 FROM users WHERE id = sqlc.arg(id));

-- name: CountActiveAdmins :one
SELECT count(*) FROM users WHERE role = 'admin' AND disabled_at IS NULL;

-- name: DeleteUser :execrows
-- Sessions and API tokens go with the user (ON DELETE CASCADE); remotes and
-- jobs keep it (their foreign keys fail the statement).
DELETE FROM users WHERE id = sqlc.arg(id);

-- Sessions -------------------------------------------------------------------

-- name: CreateSession :exec
INSERT INTO sessions (id_hash, user_id, created_at, last_seen_at, idle_expires_at, expires_at, user_agent, client_ip)
VALUES (
    sqlc.arg(id_hash), sqlc.arg(user_id), sqlc.arg(created_at), sqlc.arg(last_seen_at),
    sqlc.arg(idle_expires_at), sqlc.arg(expires_at), sqlc.arg(user_agent), sqlc.arg(client_ip)
);

-- name: GetSession :one
SELECT * FROM sessions WHERE id_hash = sqlc.arg(id_hash);

-- name: TouchSession :execrows
UPDATE sessions SET last_seen_at = sqlc.arg(at), idle_expires_at = sqlc.arg(idle_expires_at)
WHERE id_hash = sqlc.arg(id_hash);

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id_hash = sqlc.arg(id_hash);

-- name: DeleteUserSessions :execrows
-- keep NULL deletes every session of the user.
DELETE FROM sessions
WHERE user_id = sqlc.arg(user_id)
  AND (sqlc.narg(keep)::bytea IS NULL OR id_hash <> sqlc.narg(keep)::bytea);

-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions WHERE idle_expires_at <= sqlc.arg(now) OR expires_at <= sqlc.arg(now);

-- API tokens -----------------------------------------------------------------

-- name: CreateAPIToken :exec
INSERT INTO api_tokens (id, user_id, name, prefix, hash, created_at, expires_at, last_used_at)
VALUES (
    sqlc.arg(id), sqlc.arg(user_id), sqlc.arg(name), sqlc.arg(prefix), sqlc.arg(hash),
    sqlc.arg(created_at), sqlc.arg(expires_at), sqlc.arg(last_used_at)
);

-- name: GetAPITokenByHash :one
SELECT * FROM api_tokens WHERE hash = sqlc.arg(hash);

-- name: ListAPITokens :many
SELECT * FROM api_tokens WHERE user_id = sqlc.arg(user_id) ORDER BY id DESC;

-- name: DeleteAPIToken :execrows
DELETE FROM api_tokens WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);

-- name: TouchAPIToken :execrows
UPDATE api_tokens SET last_used_at = sqlc.arg(at) WHERE id = sqlc.arg(id);
