# ADR 0004: Postgres store with pgx, sqlc and goose

- Status: accepted (unreleased until v2.0.0)
- Date: 2026-11-04

## Context

The controller's use cases persist through repository interfaces
(`workers.Repository`, `workers.TokenRepository`, `jobs.Repository`,
`jobs.SampleRepository`). Their documentation prescribes the concurrency
rules: compare-and-swap on the worker epoch, monotonic cursors, single-use
enrollment tokens, version-checked job updates. Several requests (and, later,
several controllers) race on the same rows, so the rules must hold in the
database, not in process memory. `internal/adapters/memstore` implements them
for tests; `internal/adapters/postgres` is the production implementation and
passes the same contract suite (`internal/adapters/storetest`).

## Decision

**Stack.** `pgx/v5` (`pgxpool`) as the driver, `sqlc` for typed queries
(`internal/adapters/postgres/queries/*.sql` generate into the committed
package `.../postgres/db`; CI fails on drift), `goose` for migrations. The
adapter imports only the domain, the app ports it implements and
`internal/platform/redact`; `cmd/` wires it.

**Schema** (`migrations/00001_init.sql`):

- Real foreign keys: `jobs.worker_id -> workers`, `job_samples.job_id ->
  jobs ON DELETE CASCADE`. Workers are soft-removed (`removed_at`), so job
  history keeps its worker. `enrollment_tokens.used_by` has no foreign key:
  a token is redeemed before the worker row exists.
- Every instant is `timestamptz`. Postgres keeps microseconds, as do the
  system clocks; the adapter truncates on write. The zero Go time is `NULL`
  in nullable columns.
- Every byte and counter column is `bigint`, including final job stats,
  sample counters, `ack_seq` and elapsed time (`*_elapsed_ns`, nanoseconds);
  a test guards this.
- Enum-like values (`workers.mode`, `jobs.status`) have `CHECK` constraints
  mirroring the domain. Statuses are not checked for lifecycle order: the
  state machine lives in `domain`, and a database trigger would duplicate it.
- `workers.info` is one `jsonb` document, not columns: it is what the
  worker reported in its latest Hello, replaced as a whole, never filtered or
  joined in SQL, and it grows with the protocol (new fields need no
  migration; its `epoch` names the process that sent the Hello, and
  migration 00008 filled it in for the rows stored before). The adapter
  owns the document shape (`infoDoc`), decoupled from the Go struct.
  Everything the controller acts on (epoch, ack cursor, last seen, mode,
  removal) is a typed column. `past_epochs` is `text[]`
  because order matters and it is small and bounded.
- Secrets (`secret_enc`, `spec_enc`) are opaque `bytea` ciphertexts; the
  database never sees plaintext.
- Indexes: partial `jobs (worker_id, id) WHERE status IN (queued,
  dispatched, running)` for the dispatcher, partial `jobs (queued_at, id)
  WHERE status = 'queued'` for the sweep, `jobs (worker_id, id DESC)` for
  listing, `enrollment_tokens (expires_at)` for purging. A test checks the
  dispatcher query actually uses its partial index.

**Concurrency.** There are no transactions spanning calls, no application
locks and no retry loops. Each atomic port method is one statement at the
default READ COMMITTED level, so competing writers queue on the row lock and
re-evaluate the `WHERE` clause against the committed row:

- epoch CAS: `UPDATE workers SET epoch = $to, ..., ack_seq = 0 WHERE id = $1
  AND removed_at IS NULL AND epoch = $from RETURNING *`
- ack cursor: `SET ack_seq = GREATEST(ack_seq, $seq) ... AND epoch = $e
  RETURNING ack_seq`; last seen: `SET last_seen = GREATEST(last_seen, $at)`
- token redemption: `UPDATE enrollment_tokens SET used_at, used_by WHERE
  hash = $1 AND used_at IS NULL AND expires_at > $at RETURNING *`
- job update: `UPDATE jobs SET ..., version = version + 1 WHERE id = $1 AND
  version = $2 RETURNING *`
- job creation: `INSERT ... SELECT ... WHERE EXISTS (SELECT 1 FROM workers
  WHERE id = $w AND removed_at IS NULL FOR SHARE)`; the share lock makes a
  concurrent `RemoveWorker` wait for the insert, so removal's job release
  cannot miss the new job.
- samples: `INSERT ... ON CONFLICT (job_id, t) DO NOTHING`.

A conditional update that matches no row is a "no rows" result, which the
adapter interprets with one follow-up query (worker or job exists?) to choose
between `ErrNotFound` and `ErrConflict`. That second query is not atomic with
the first, which is harmless: both outcomes tell the caller to re-read.

**Error mapping** (`errors.go`): unique violation -> `ErrAlreadyExists`;
foreign key violation and no rows -> `ErrNotFound`; failed CAS ->
`ErrConflict`; check, not-null and bad-value (NUL byte, bad UTF-8) errors ->
`ErrInvalidArgument`; anything else, including context errors, is wrapped
unchanged. The server's `DETAIL` (which can echo row values) and the
connection string are never put in errors. Free text that comes from tool
output (job error message, warnings, worker info) is sanitized rather than
rejected, so a stray NUL byte cannot wedge a job in a non-terminal state.

**Migrations.** SQL files embedded with `embed.FS`, applied by
`Store.Migrate` through goose's provider. They are forward-only at runtime:
the controller never runs a down migration, and a schema change that cannot be
rolled out compatibly (drop, rename) takes two releases (add, then remove).
Down scripts exist for development and are tested in a down/up round trip.
`Migrate` takes a session-level advisory lock (`pg_advisory_lock`, blocking)
before reading the schema version, so controllers starting together serialize
and the later ones find nothing to do; the lock vanishes with its session if a
controller dies mid-migration, and the migrations themselves run in
transactions. Migrations use their own two connections rather than the pool,
so they cannot be starved by (or deadlock with) request traffic. Callers bound
the wait with the context.

## Consequences

- The memory and Postgres stores are interchangeable behind the ports; any
  new port rule is added to `storetest` first and both must pass.
- `ListJobs` uses optional-filter predicates (`$1 IS NULL OR ...`) in one
  static statement. That is simple and safe; if the planner picks poor plans
  at large volumes, split the query per filter combination.
- Sample retention is implemented (Milestone 6, ADR 0014): once an hour
  the controller deletes the samples of the jobs that finished more than
  `jobs.sample_retention` ago (default 30 days; 0 keeps them).
  `PurgeJobSamples` is one `DELETE` per batch of at most 50 jobs, oldest
  finished first, through `jobs_finished_idx` and the `(job_id, t)` primary
  key, repeated until a batch deletes nothing; unfinished jobs are never
  touched. Job rows and their final counters stay, and so do schedule runs:
  purging jobs would corrupt the restic snapshot cache's change mark (ADR
  0014). `ListJobSamples` thins a long chart at read time to at most
  `max_points` samples (default 1000, at most 5000), always with the first
  and the last, by the same rank rule in both stores.
- Tests run the contract suite and the migration tests against a real
  `postgres:17-alpine` (testcontainers): one container per package run, one
  database per test cloned from a migrated template database.
