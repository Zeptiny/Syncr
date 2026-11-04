-- name: CreateEnrollmentToken :exec
INSERT INTO enrollment_tokens (hash, created_at, expires_at, used_at, used_by)
VALUES (sqlc.arg(hash), sqlc.arg(created_at), sqlc.arg(expires_at), sqlc.arg(used_at), sqlc.arg(used_by));

-- name: ConsumeEnrollmentToken :one
UPDATE enrollment_tokens SET used_at = sqlc.arg(at)::timestamptz, used_by = sqlc.arg(used_by)::uuid
WHERE hash = sqlc.arg(hash) AND used_at IS NULL AND expires_at > sqlc.arg(at)::timestamptz
RETURNING *;

-- name: DeleteEnrollmentTokens :execrows
DELETE FROM enrollment_tokens WHERE expires_at < sqlc.arg(before)::timestamptz;
