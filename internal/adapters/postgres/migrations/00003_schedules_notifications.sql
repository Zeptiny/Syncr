-- +goose Up

-- Milestone 4: worker labels and unique names, notification contacts,
-- schedules with their runs, the notification outbox, and the indexes of
-- the dashboard (the conventions of 00001 apply).

-- Labels are set by admins (normalized: lower case, sorted, unique).
ALTER TABLE workers ADD COLUMN labels text[] NOT NULL DEFAULT '{}';
-- Worker names become unique among live workers. Existing duplicates
-- (case-insensitive; the oldest worker keeps the name) get the first
-- numeric suffix that no live worker has, like Enroll does: "nas" becomes
-- "nas-2", then "nas-3", the name cut to stay within 64 characters. Each
-- candidate is checked against the names as renamed so far.
-- +goose StatementBegin
DO $$
DECLARE
    dup record;
    candidate text;
    n integer;
BEGIN
    FOR dup IN
        SELECT w.id, w.name FROM workers w
         WHERE w.removed_at IS NULL AND EXISTS (
             SELECT 1 FROM workers o
              WHERE o.removed_at IS NULL AND lower(o.name) = lower(w.name) AND o.id < w.id
         )
         ORDER BY w.id
    LOOP
        n := 2;
        LOOP
            candidate := rtrim(left(dup.name, 63 - length(n::text))) || '-' || n;
            EXIT WHEN NOT EXISTS (
                SELECT 1 FROM workers o WHERE o.removed_at IS NULL AND lower(o.name) = lower(candidate)
            );
            n := n + 1;
        END LOOP;
        UPDATE workers SET name = candidate WHERE id = dup.id;
    END LOOP;
END
$$;
-- +goose StatementEnd
CREATE UNIQUE INDEX workers_name_key ON workers (lower(name)) WHERE removed_at IS NULL;

-- Notification contacts: instances of a channel type, private to their
-- owner, deleted for real (their links and deliveries go with them).
CREATE TABLE contacts (
    id            uuid        PRIMARY KEY,
    owner_id      uuid        NOT NULL REFERENCES users (id),
    name          text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 64),
    channel_type  text        NOT NULL,
    -- Non-secret options keyed by field name.
    options       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    -- JSON object of secret values, encrypted with the Cipher; NULL when
    -- there are none.
    secrets_enc   bytea,
    -- Names of the secret fields that have a value (not secret).
    secret_keys   text[]      NOT NULL DEFAULT '{}',
    -- The channel's non-secret one-line description.
    summary       text        NOT NULL DEFAULT '',
    enabled       boolean     NOT NULL DEFAULT true,
    -- Events of the owner's manual jobs the contact is notified of.
    job_events    text[]      NOT NULL DEFAULT '{}'
                  CHECK (job_events <@ ARRAY['succeeded', 'failed', 'interrupted']::text[]),
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    -- Optimistic concurrency token (UpdateContact).
    version       bigint      NOT NULL DEFAULT 1 CHECK (version >= 1)
);
CREATE UNIQUE INDEX contacts_owner_name_key ON contacts (owner_id, lower(name));

-- Schedules: a job definition (secret-free, like jobs.definition) on a cron
-- expression in a time zone, targeting one worker or a label.
CREATE TABLE schedules (
    id             uuid        PRIMARY KEY,
    owner_id       uuid        NOT NULL REFERENCES users (id),
    name           text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 64),
    description    text        NOT NULL DEFAULT '',
    cron           text        NOT NULL,
    timezone       text        NOT NULL,
    kind           text        NOT NULL,
    definition     jsonb       NOT NULL,
    search_text    text        NOT NULL DEFAULT '',
    worker_id      uuid        REFERENCES workers (id),
    worker_label   text,
    CHECK ((worker_id IS NULL) <> (worker_label IS NULL)),
    missed_action  text        NOT NULL CHECK (missed_action IN ('run_late', 'skip')),
    max_delay_ns   bigint      NOT NULL DEFAULT 0 CHECK (max_delay_ns >= 0),
    overlap        text        NOT NULL CHECK (overlap IN ('skip', 'queue', 'allow')),
    enabled        boolean     NOT NULL,
    -- Reason code of an automatic pause; '' otherwise.
    paused_reason  text        NOT NULL DEFAULT '',
    -- The scheduler's fire state (compare-and-swapped by FireSchedule).
    next_run_at    timestamptz,
    last_fired_at  timestamptz,
    CHECK (enabled OR next_run_at IS NULL),
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    -- Optimistic concurrency token of the configuration (UpdateSchedule);
    -- fires do not change it.
    version        bigint      NOT NULL DEFAULT 1 CHECK (version >= 1)
);
CREATE UNIQUE INDEX schedules_owner_name_key ON schedules (owner_id, lower(name));
-- ListDueSchedules.
CREATE INDEX schedules_due_idx ON schedules (next_run_at) WHERE enabled AND next_run_at IS NOT NULL;
-- ListUpcoming per owner.
CREATE INDEX schedules_owner_next_idx ON schedules (owner_id, next_run_at) WHERE enabled AND next_run_at IS NOT NULL;

-- The remotes a schedule's definition refers to.
CREATE TABLE schedule_remotes (
    schedule_id  uuid NOT NULL REFERENCES schedules (id) ON DELETE CASCADE,
    remote_id    uuid NOT NULL REFERENCES remotes (id),
    PRIMARY KEY (schedule_id, remote_id)
);
CREATE INDEX schedule_remotes_remote_idx ON schedule_remotes (remote_id);

-- The contacts a schedule notifies, with the events of each link.
CREATE TABLE schedule_contacts (
    schedule_id  uuid   NOT NULL REFERENCES schedules (id) ON DELETE CASCADE,
    contact_id   uuid   NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
    events       text[] NOT NULL CHECK (
        cardinality(events) > 0 AND events <@ ARRAY['succeeded', 'failed', 'interrupted', 'missed']::text[]
    ),
    PRIMARY KEY (schedule_id, contact_id)
);
CREATE INDEX schedule_contacts_contact_idx ON schedule_contacts (contact_id);

-- jobs.schedule_id is added before schedule_runs, whose rows name jobs.
ALTER TABLE jobs
    ADD COLUMN schedule_id uuid REFERENCES schedules (id) ON DELETE SET NULL,
    -- When the notification planner committed the job's deliveries.
    ADD COLUMN notified_at timestamptz;
-- Never notify old history.
UPDATE jobs SET notified_at = now() WHERE status IN ('succeeded', 'failed', 'cancelled', 'interrupted');
-- ListJobs per schedule: newest first.
CREATE INDEX jobs_schedule_idx ON jobs (schedule_id, id DESC) WHERE schedule_id IS NOT NULL;
-- HasActiveRun.
CREATE INDEX jobs_schedule_active_idx ON jobs (schedule_id)
    WHERE schedule_id IS NOT NULL AND status IN ('queued', 'dispatched', 'running');
-- PendingJobOutcomes.
CREATE INDEX jobs_notify_idx ON jobs (finished_at)
    WHERE notified_at IS NULL AND status IN ('succeeded', 'failed', 'cancelled', 'interrupted');
-- JobActivity (all users, and per owner).
CREATE INDEX jobs_finished_idx ON jobs (finished_at) WHERE finished_at IS NOT NULL;
CREATE INDEX jobs_owner_finished_idx ON jobs (owner_id, finished_at) WHERE finished_at IS NOT NULL;

-- Runs of schedules. job_id has no foreign key on purpose: a launching run
-- reserves its job's ID before the job is created (two-phase launch, so a
-- retried launch creates the job once); ListRuns reads the job's status
-- with a LEFT JOIN. Jobs are never deleted.
CREATE TABLE schedule_runs (
    id             uuid        PRIMARY KEY,
    schedule_id    uuid        NOT NULL REFERENCES schedules (id) ON DELETE CASCADE,
    trigger        text        NOT NULL CHECK (trigger IN ('cron', 'manual')),
    scheduled_for  timestamptz NOT NULL,
    deadline       timestamptz NOT NULL,
    coalesced      integer     NOT NULL DEFAULT 0 CHECK (coalesced >= 0),
    status         text        NOT NULL
                   CHECK (status IN ('pending', 'launching', 'launched', 'missed', 'failed', 'cancelled')),
    reason         text        NOT NULL DEFAULT '',
    message        text        NOT NULL DEFAULT '',
    worker_id      uuid        REFERENCES workers (id),
    job_id         uuid,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    -- When the notification planner committed the run's deliveries.
    notified_at    timestamptz,
    CHECK (status NOT IN ('launching', 'launched') OR job_id IS NOT NULL)
);
-- At most one open run per schedule.
CREATE UNIQUE INDEX schedule_runs_one_open_key ON schedule_runs (schedule_id) WHERE status IN ('pending', 'launching');
-- ListOpenRuns.
CREATE INDEX schedule_runs_open_idx ON schedule_runs (scheduled_for) WHERE status IN ('pending', 'launching');
-- ListRuns per schedule: newest first.
CREATE INDEX schedule_runs_schedule_idx ON schedule_runs (schedule_id, id DESC);
-- PendingRunOutcomes.
CREATE INDEX schedule_runs_notify_idx ON schedule_runs (updated_at) WHERE notified_at IS NULL AND status IN ('missed', 'failed');

-- The notification outbox: one row per notification to one contact.
CREATE TABLE deliveries (
    id               uuid        PRIMARY KEY,
    contact_id       uuid        NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
    owner_id         uuid        NOT NULL REFERENCES users (id),
    event            text        NOT NULL CHECK (event IN ('succeeded', 'failed', 'interrupted', 'missed')),
    -- 'job:<id>:<event>' or 'run:<id>:<event>'.
    dedupe_key       text        NOT NULL,
    job_id           uuid        REFERENCES jobs (id),
    schedule_id      uuid        REFERENCES schedules (id) ON DELETE SET NULL,
    -- The notify.Message to send (no secrets).
    payload          jsonb       NOT NULL,
    status           text        NOT NULL CHECK (status IN ('pending', 'sending', 'delivered', 'dead')),
    attempts         integer     NOT NULL DEFAULT 0,
    max_attempts     integer     NOT NULL CHECK (max_attempts >= 1),
    next_attempt_at  timestamptz NOT NULL,
    lease_until      timestamptz,
    last_attempt_at  timestamptz,
    delivered_at     timestamptz,
    last_error       text        NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL
);
CREATE UNIQUE INDEX deliveries_dedupe_key ON deliveries (contact_id, dedupe_key);
-- ClaimDeliveries.
CREATE INDEX deliveries_due_idx ON deliveries (next_attempt_at) WHERE status IN ('pending', 'sending');
-- ListDeliveries per contact: newest first.
CREATE INDEX deliveries_contact_idx ON deliveries (contact_id, id DESC);
-- PurgeDeliveries.
CREATE INDEX deliveries_purge_idx ON deliveries (created_at) WHERE status IN ('delivered', 'dead');

-- DeleteExpiredAPITokens.
CREATE INDEX api_tokens_expires_idx ON api_tokens (expires_at) WHERE expires_at IS NOT NULL;


-- +goose Down
-- Reverse order. Renamed duplicate worker names keep their suffix.
DROP INDEX api_tokens_expires_idx;
DROP TABLE deliveries;
DROP TABLE schedule_runs;
DROP INDEX jobs_owner_finished_idx;
DROP INDEX jobs_finished_idx;
DROP INDEX jobs_notify_idx;
DROP INDEX jobs_schedule_active_idx;
DROP INDEX jobs_schedule_idx;
ALTER TABLE jobs
    DROP COLUMN notified_at,
    DROP COLUMN schedule_id;
DROP TABLE schedule_contacts;
DROP TABLE schedule_remotes;
DROP TABLE schedules;
DROP TABLE contacts;
DROP INDEX workers_name_key;
ALTER TABLE workers DROP COLUMN labels;
