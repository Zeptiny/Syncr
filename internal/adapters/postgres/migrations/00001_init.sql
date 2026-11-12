-- +goose Up

-- Schema of the controller store (internal/adapters/postgres). Conventions:
--   * every instant is timestamptz (microsecond precision); NULL means "not
--     set" where the Go value is the zero time;
--   * every byte or counter value is bigint (the old app broke on 32-bit
--     counters);
--   * enum-like text columns carry a CHECK constraint mirroring the domain
--     values.

CREATE TABLE workers (
    id                uuid        PRIMARY KEY,
    name              text        NOT NULL,
    mode              text        NOT NULL CHECK (mode IN ('active', 'passive')),
    -- Passive workers only: https base URL and pinned TLS certificate
    -- (SHA-256 of the DER encoding, lower-case hex). Empty for active ones.
    url               text        NOT NULL DEFAULT '',
    cert_fingerprint  text        NOT NULL DEFAULT '',
    -- Link secret encrypted with the Cipher; NULL once the worker is removed.
    secret_enc        bytea,
    -- What the worker reported in its latest Hello. A snapshot that is
    -- replaced as a whole and never filtered in SQL, so one jsonb document
    -- (new fields need no migration) rather than a dozen columns.
    info              jsonb       NOT NULL DEFAULT '{}'::jsonb,
    -- Link state (see workers.Link). The epoch columns are compared and
    -- advanced by single conditional UPDATE statements.
    epoch             text        NOT NULL DEFAULT '',
    epoch_started_at  timestamptz,
    -- Replaced epochs, most recent first (array order is significant).
    past_epochs       text[]      NOT NULL DEFAULT '{}',
    ack_seq           bigint      NOT NULL DEFAULT 0 CHECK (ack_seq >= 0),
    last_seen         timestamptz,
    created_at        timestamptz NOT NULL,
    removed_at        timestamptz
);

-- Only the hash of a token is stored. used_by deliberately has no foreign
-- key: a token is redeemed before the worker row it names is created.
CREATE TABLE enrollment_tokens (
    hash        bytea       PRIMARY KEY CHECK (octet_length(hash) = 32),
    created_at  timestamptz NOT NULL,
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz,
    used_by     uuid
);

-- Purging deletes by expiry.
CREATE INDEX enrollment_tokens_expires_at_idx ON enrollment_tokens (expires_at);

CREATE TABLE jobs (
    id                    uuid        PRIMARY KEY, -- UUIDv7: id order is creation order
    worker_id             uuid        NOT NULL REFERENCES workers (id),
    kind                  text        NOT NULL,
    status                text        NOT NULL CHECK (status IN
        ('queued', 'dispatched', 'running', 'succeeded', 'failed', 'cancelled', 'interrupted')),
    -- Encoded JobSpec, encrypted with the Cipher.
    spec_enc              bytea       NOT NULL,
    epoch                 text        NOT NULL DEFAULT '',
    cancel_requested_at   timestamptz,
    created_at            timestamptz NOT NULL,
    queued_at             timestamptz NOT NULL,
    dispatched_at         timestamptz,
    started_at            timestamptz,
    finished_at           timestamptz,
    requeues              integer     NOT NULL DEFAULT 0 CHECK (requeues >= 0),
    -- Final stats; elapsed_ns is a time.Duration in nanoseconds.
    stat_bytes            bigint      NOT NULL DEFAULT 0,
    stat_total_bytes      bigint      NOT NULL DEFAULT 0,
    stat_files            bigint      NOT NULL DEFAULT 0,
    stat_total_files      bigint      NOT NULL DEFAULT 0,
    stat_checks           bigint      NOT NULL DEFAULT 0,
    stat_deletes          bigint      NOT NULL DEFAULT 0,
    stat_renames          bigint      NOT NULL DEFAULT 0,
    stat_errors           bigint      NOT NULL DEFAULT 0,
    stat_elapsed_ns       bigint      NOT NULL DEFAULT 0,
    error_code            text        NOT NULL DEFAULT '',
    error_message         text        NOT NULL DEFAULT '',
    warnings              text[]      NOT NULL DEFAULT '{}',
    exit_code             integer     NOT NULL DEFAULT -1,
    -- Encoded link JobOutput; NULL if none.
    output                bytea,
    -- Optimistic concurrency token (UpdateJob).
    version               bigint      NOT NULL DEFAULT 1 CHECK (version >= 0)
);

-- ListJobs: newest first, optionally per worker (keyset on id).
CREATE INDEX jobs_worker_id_idx ON jobs (worker_id, id DESC);

-- ListUnfinishedJobs, the dispatcher's hot path.
CREATE INDEX jobs_worker_unfinished_idx ON jobs (worker_id, id)
    WHERE status IN ('queued', 'dispatched', 'running');

-- ListQueuedJobs, the dispatch sweep.
CREATE INDEX jobs_queued_idx ON jobs (queued_at, id) WHERE status = 'queued';

-- Progress samples; cumulative counters as reported by the worker.
CREATE TABLE job_samples (
    job_id           uuid        NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    t                timestamptz NOT NULL,
    bytes            bigint      NOT NULL,
    total_bytes      bigint      NOT NULL,
    files            bigint      NOT NULL,
    total_files      bigint      NOT NULL,
    checks           bigint      NOT NULL,
    deletes          bigint      NOT NULL,
    renames          bigint      NOT NULL,
    errors           bigint      NOT NULL,
    elapsed_ns       bigint      NOT NULL,
    PRIMARY KEY (job_id, t)
);


-- +goose Down
DROP TABLE job_samples;
DROP TABLE jobs;
DROP TABLE enrollment_tokens;
DROP TABLE workers;
