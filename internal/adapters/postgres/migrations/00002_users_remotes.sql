-- +goose Up

-- Milestone 3: users, sessions, API tokens, remotes, and jobs owned by users
-- that refer to remotes by ID (the conventions of 00001 apply).

-- Usernames are stored lower case; the CHECK mirrors the users use case.
CREATE TABLE users (
    id                   uuid        PRIMARY KEY,
    username             text        NOT NULL CHECK (username ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
    display_name         text        NOT NULL DEFAULT '',
    role                 text        NOT NULL CHECK (role IN ('admin', 'user')),
    -- argon2id PHC string.
    password_hash        text        NOT NULL,
    disabled_at          timestamptz,
    created_at           timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL,
    password_changed_at  timestamptz NOT NULL,
    last_login_at        timestamptz,
    -- Optimistic concurrency token (UpdateUser).
    version              bigint      NOT NULL DEFAULT 1 CHECK (version >= 1)
);
CREATE UNIQUE INDEX users_username_key ON users (username);

-- Browser sessions. Only the SHA-256 hash of the session token is stored.
CREATE TABLE sessions (
    id_hash          bytea       PRIMARY KEY CHECK (octet_length(id_hash) = 32),
    user_id          uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at       timestamptz NOT NULL,
    last_seen_at     timestamptz NOT NULL,
    idle_expires_at  timestamptz NOT NULL,
    expires_at       timestamptz NOT NULL,
    user_agent       text        NOT NULL DEFAULT '',
    client_ip        text        NOT NULL DEFAULT ''
);
CREATE INDEX sessions_user_idx ON sessions (user_id);

-- Personal API tokens. Only the SHA-256 hash of the token is stored.
CREATE TABLE api_tokens (
    id            uuid        PRIMARY KEY,
    user_id       uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name          text        NOT NULL,
    prefix        text        NOT NULL,
    hash          bytea       NOT NULL UNIQUE CHECK (octet_length(hash) = 32),
    created_at    timestamptz NOT NULL,
    expires_at    timestamptz,
    last_used_at  timestamptz
);
CREATE INDEX api_tokens_user_idx ON api_tokens (user_id);

-- Remotes are soft-deleted (deleted_at), so job history keeps its foreign
-- keys; deleting one clears its secrets.
CREATE TABLE remotes (
    id            uuid        PRIMARY KEY,
    owner_id      uuid        NOT NULL REFERENCES users (id),
    name          text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 64),
    backend_type  text        NOT NULL,
    -- Non-secret options keyed by field name.
    options       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    -- JSON object of secret values, encrypted with the Cipher; NULL when
    -- there are none and once the remote is deleted.
    secrets_enc   bytea,
    -- Names of the secret fields that have a value (not secret).
    secret_keys   text[]      NOT NULL DEFAULT '{}',
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    deleted_at    timestamptz,
    -- Optimistic concurrency token (UpdateRemote).
    version       bigint      NOT NULL DEFAULT 1 CHECK (version >= 1)
);
-- Names are unique per owner, case-insensitively, among live remotes.
CREATE UNIQUE INDEX remotes_owner_name_key ON remotes (owner_id, lower(name)) WHERE deleted_at IS NULL;

-- Members of union remotes, in order (ord).
CREATE TABLE union_members (
    union_remote_id   uuid    NOT NULL REFERENCES remotes (id) ON DELETE CASCADE,
    ord               integer NOT NULL CHECK (ord >= 0),
    member_remote_id  uuid    NOT NULL REFERENCES remotes (id),
    path              text    NOT NULL DEFAULT '',
    mode              text    NOT NULL CHECK (mode IN ('read_write', 'read_only', 'no_create')),
    PRIMARY KEY (union_remote_id, ord),
    CHECK (union_remote_id <> member_remote_id)
);
CREATE INDEX union_members_member_idx ON union_members (member_remote_id);

-- No production data exists before Milestone 3: development jobs are dropped.
-- Jobs no longer store an encrypted spec with inline credentials; they store
-- their owner and their secret-free definition (protojson of
-- syncr.v1.JobDefinition), and the controller builds the spec at dispatch.
TRUNCATE job_samples, jobs;
ALTER TABLE jobs
    DROP COLUMN spec_enc,
    ADD COLUMN owner_id uuid NOT NULL REFERENCES users (id),
    ADD COLUMN definition jsonb NOT NULL,
    -- Lower-case text the job list search matches (kind, mode, paths and
    -- remote names at creation time).
    ADD COLUMN search_text text NOT NULL DEFAULT '';
-- ListJobs per owner: newest first (keyset on id).
CREATE INDEX jobs_owner_idx ON jobs (owner_id, id DESC);

-- The remotes a job's definition refers to.
CREATE TABLE job_remotes (
    job_id     uuid NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    remote_id  uuid NOT NULL REFERENCES remotes (id),
    PRIMARY KEY (job_id, remote_id)
);
CREATE INDEX job_remotes_remote_idx ON job_remotes (remote_id, job_id DESC);


-- +goose Down
-- Reverse order. Jobs of Milestone 3 cannot be represented in the 00001
-- schema (they have no encrypted spec), so they are dropped as well.
DROP TABLE job_remotes;
TRUNCATE job_samples, jobs;
DROP INDEX jobs_owner_idx;
ALTER TABLE jobs
    DROP COLUMN search_text,
    DROP COLUMN definition,
    DROP COLUMN owner_id,
    ADD COLUMN spec_enc bytea NOT NULL;
DROP TABLE union_members;
DROP TABLE remotes;
DROP TABLE api_tokens;
DROP TABLE sessions;
DROP TABLE users;
