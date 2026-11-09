-- +goose Up

-- Milestone 5: restic repositories with a snapshot cache, the jobs and
-- schedules that use them, and the concurrency keys that serialize jobs per
-- repository (the conventions of 00001 apply).

-- Repositories are soft-deleted (deleted_at) so job history keeps its foreign
-- keys; deleting one clears its password and its snapshot cache. Data in the
-- backend is deleted only by an explicit restic_delete_data job (deleting =
-- true while it is pending); the row is soft-deleted when that job succeeds.
CREATE TABLE restic_repositories (
    id                      uuid        PRIMARY KEY,
    owner_id                uuid        NOT NULL REFERENCES users (id),
    name                    text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 64),
    description             text        NOT NULL DEFAULT '' CHECK (char_length(description) <= 500),
    remote_id               uuid        NOT NULL REFERENCES remotes (id),
    -- Path on the remote, relative to its root, normalized ('' is the root).
    path                    text        NOT NULL DEFAULT '' CHECK (char_length(path) <= 1024),
    -- The repository password, encrypted with the Cipher (associated data
    -- 'restic-password:<id>'); NULL once the repository is deleted.
    password_enc            bytea,
    -- restic's repository ID (its config file) and format version.
    restic_id               text        NOT NULL CHECK (restic_id ~ '^[0-9a-f]{64}$'),
    format_version          integer     NOT NULL DEFAULT 0 CHECK (format_version >= 0),
    snapshots_refreshed_at  timestamptz,
    snapshots_truncated     boolean     NOT NULL DEFAULT false,
    created_at              timestamptz NOT NULL,
    updated_at              timestamptz NOT NULL,
    deleted_at              timestamptz,
    version                 bigint      NOT NULL DEFAULT 1 CHECK (version >= 1),
    -- Data deletion. deletion_job_id has no foreign key: it is reserved before
    -- the job exists; after a failed attempt it still names that job.
    deleting                boolean     NOT NULL DEFAULT false,
    deletion_job_id         uuid,
    deletion_requested_at   timestamptz,
    deletion_error          text        NOT NULL DEFAULT '' CHECK (char_length(deletion_error) <= 1000),
    data_deleted_at         timestamptz,
    CHECK ((deleted_at IS NULL) = (password_enc IS NOT NULL)),
    CHECK (NOT deleting OR (deletion_job_id IS NOT NULL AND deletion_requested_at IS NOT NULL AND deleted_at IS NULL)),
    CHECK (data_deleted_at IS NULL OR deleted_at IS NOT NULL)
);
CREATE UNIQUE INDEX restic_repositories_owner_name_key ON restic_repositories (owner_id, lower(name)) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX restic_repositories_location_key ON restic_repositories (remote_id, path) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX restic_repositories_owner_restic_id_key ON restic_repositories (owner_id, restic_id) WHERE deleted_at IS NULL;
CREATE INDEX restic_repositories_owner_idx ON restic_repositories (owner_id) WHERE deleted_at IS NULL;
-- FinishDeletions scans pending deletions.
CREATE INDEX restic_repositories_deleting_idx ON restic_repositories (deletion_requested_at) WHERE deleting;

-- The snapshot cache, replaced as a whole by each listing.
CREATE TABLE restic_snapshots (
    repository_id    uuid        NOT NULL REFERENCES restic_repositories (id) ON DELETE CASCADE,
    snapshot_id      text        NOT NULL CHECK (snapshot_id ~ '^[0-9a-f]{64}$'),
    taken_at         timestamptz NOT NULL,
    hostname         text        NOT NULL DEFAULT '',
    username         text        NOT NULL DEFAULT '',
    paths            text[]      NOT NULL DEFAULT '{}',
    tags             text[]      NOT NULL DEFAULT '{}',
    parent           text        NOT NULL DEFAULT '',
    program_version  text        NOT NULL DEFAULT '',
    -- restic's snapshot summary (restic >= 0.17) with restic's JSON field
    -- names; NULL when the snapshot has none.
    summary          jsonb,
    PRIMARY KEY (repository_id, snapshot_id)
);
CREATE INDEX restic_snapshots_time_idx ON restic_snapshots (repository_id, taken_at DESC);

-- Jobs: the repository a restic job uses, and its concurrency keys (set at
-- creation, never changed).
ALTER TABLE jobs
    ADD COLUMN repository_id uuid REFERENCES restic_repositories (id),
    ADD COLUMN concurrency_keys text[] NOT NULL DEFAULT '{}';
CREATE INDEX jobs_repository_idx ON jobs (repository_id, id DESC) WHERE repository_id IS NOT NULL;
-- Key overlap (&&) for blockers, the dispatch sweep and the wake-up.
CREATE INDEX jobs_concurrency_keys_idx ON jobs USING gin (concurrency_keys) WHERE cardinality(concurrency_keys) > 0;

ALTER TABLE schedules ADD COLUMN repository_id uuid REFERENCES restic_repositories (id);
CREATE INDEX schedules_repository_idx ON schedules (repository_id) WHERE repository_id IS NOT NULL;

-- +goose Down
-- Reverse order. Restic jobs and schedules cannot be represented without
-- these columns: they are dropped with their deliveries (job_samples and
-- job_remotes cascade; schedule runs and links cascade; jobs.schedule_id is
-- set NULL).
DELETE FROM deliveries WHERE job_id IN (SELECT id FROM jobs WHERE repository_id IS NOT NULL);
DELETE FROM jobs WHERE repository_id IS NOT NULL;
DELETE FROM schedules WHERE repository_id IS NOT NULL;
DROP INDEX schedules_repository_idx;
ALTER TABLE schedules DROP COLUMN repository_id;
DROP INDEX jobs_concurrency_keys_idx;
DROP INDEX jobs_repository_idx;
ALTER TABLE jobs DROP COLUMN concurrency_keys, DROP COLUMN repository_id;
DROP TABLE restic_snapshots;
DROP TABLE restic_repositories;
