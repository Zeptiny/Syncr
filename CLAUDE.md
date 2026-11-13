# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Syncr v2: a Go rewrite (on branch `v2`) of a self-hosted manager for rclone sync jobs and restic backups. The old Django version lives on `main`. One `syncr-controller` (Postgres, Connect API, embedded React UI) drives any number of `syncr-worker`s (Linux/Windows services) that run pinned rclone/restic child processes.

## Commands

```sh
make generate            # npm ci in web/, buf generate (Go + TS), sqlc generate (pinned via go run)
make lint                # buf lint + golangci-lint (includes depguard layering rules)
go test ./...            # unit tests; needs only Go
go test ./internal/app/jobs -run TestName        # single Go test
go vet -tags integration ./...                   # CI checks that integration tests compile
make test-integration    # fetches pinned rclone/restic, runs `go test -tags integration -count=1 ./...`
make web-check           # web typecheck, eslint, prettier check, vitest
cd web && npx vitest run src/lib/cron.test.ts    # single web test
make dev-seed            # controller on the in-memory store with demo data (admin / syncr-demo-admin); probes get canned answers (internal/devseed), so the whole UI works without workers
npm --prefix web run dev # Vite on :5173, proxies the API to 127.0.0.1:8080 (VITE_SYNCR_MOCK=1 for an in-browser mock)
make dev-up              # Postgres, MinIO and SFTP for local runs
```

- Integration tests use the `//go:build integration` tag. They need `SYNCR_TEST_RCLONE` and `SYNCR_TEST_RESTIC` set to binaries of the pinned versions (tests that need a tool skip without them). Postgres, MinIO and SFTP tests start containers with testcontainers. `internal/e2e/system` builds and runs the real binaries (restic against MinIO end to end in `restic_test.go`).
- PR screenshots: `node web/scripts/screenshots.mjs` against a freshly started seeded controller built with `-tags webui` (`SYNCR_UI_URL`, `CHROMIUM_PATH`; the set defaults to the newest milestone). `SYNCR_SCREENSHOT_SET=readme` retakes the README shots in `docs/screenshots/readme/` (light and dark); `SYNCR_SCREENSHOT_SET=hooks` takes the hooks shots in `docs/screenshots/hooks/`, and `SYNCR_SCREENSHOT_SET=bisync` the two-way sync shots in `docs/screenshots/bisync/`. Run it once per controller start: repeated sign-ins hit the login rate limit.
- CI also lints with `GOOS=windows` (Windows-only files use build tags) and runs tests on Windows.
- A plain `go build` of the controller serves a placeholder page. It embeds the UI only with `-tags webui` after `make web`.

## Generated code (committed, CI fails on drift)

- `gen/go/` and `web/src/gen/` come from `proto/syncr/v1/*.proto` via `buf generate`. Run `npm ci --prefix web` first, and run `buf format` too.
- `internal/adapters/postgres/db/` comes from `migrations/` (goose, used as the sqlc schema) plus `queries/` via sqlc.
- `web/src/routeTree.gen.ts` comes from `npm --prefix web run generate:routes` (TanStack Router file routes in `web/src/routes/`).

Never hand-edit these files: change the source, then regenerate.

## Architecture

**Ports and adapters, enforced by depguard** (`.golangci.yml`). Check its rules before adding an import:
- `internal/domain`: standard library only (IDs, job status state machine, domain errors).
- `internal/app/*`: use cases. Each declares its own ports (`ports.go`) and may import only domain and the pure cores `backend`, `descriptor`, `notify` and `cronexpr` (the packages themselves, not their subpackages). App tests may use only memstore, broker, idgen, secrets and fakes.
- `internal/adapters/*`: implement the app ports and never import each other or `controller`/`worker`.
- `internal/platform/*`: generic leaves with no Syncr business code. `internal/link` is shared by both sides and imports neither.
- Controller code and worker code never import each other.
- `cmd/syncr-controller/wire.go` and `cmd/syncr-worker/wire.go` are the only composition roots. Constructor injection; shutdown order is described in ADR 0005.

**Extension points** (one package plus one registration line each):
- Backend types: `internal/backend/builtin/builtin.go`. A type on a new rclone backend also needs that backend's options reviewed and the backend added to the worker's allowlist (`rcloneenv.SupportedBackends`); a cross-check test enforces this.
- Notification channels: `internal/adapters/notify/builtin/builtin.go`
- Job kinds: `internal/jobkind`. A kind may require link features (`RequiredFeatures`) or, for some definitions, a worker OS (`OSBound`, e.g. `use_fs_snapshot` needs Windows); the API, the scheduler's worker pick and the dispatcher's spec builder all check them.
- Worker runners: registered in `cmd/syncr-worker/wire.go`

**Stores**: `memstore` (tests, `dev.memory_store`) and `postgres` implement the same ports, and both run the contract suite in `internal/adapters/storetest`. A new store method needs a storetest case and both implementations. Postgres schema changes go in a new numbered goose migration; only a migration that is not on `v2` yet (the open milestone's) may still be changed in place.

**Worker link**: the same messages flow in both modes. In active mode the worker calls the controller (Report/Poll long polls); in passive mode the controller calls the worker over pinned TLS. Workers buffer events in an outbox keyed by `(epoch, seq)`, and the controller reconciles after restarts. The stored worker info records the epoch of its Hello (`Worker.InfoCurrent`): until a new worker process's own Hello is stored, the dispatcher sends it only cancels, never StartJobs or probes. Commands share `MaxCommands`: job cancels first, probes up to half of the rest, then starts. Controller side: `internal/controller/dispatch`. Worker side: `internal/worker/{agent,outbox,linkcore,transport}`.

**Worker jobs**: each job is a supervised rclone/restic child process (`internal/worker/proc`, env allowlist). Remotes are passed only as `RCLONE_CONFIG_<ALIAS>_*` env vars, never as connection strings. Progress is parsed from the tools' JSON output.

**Hooks**: commands a worker's operator defines in `worker.yaml` (`hooks`: a name, an argv, a timeout, env) that jobs run around their operation by naming them (`JobDefinition.hooks`: `before`, `on_success`, `on_failure`, `after`). **The controller only ever sends names**: every user may run jobs on every worker, so no command, argument or environment value may come from the API, and hooks get only `SYNCR_*` variables whose values are IDs and enum names (ADR 0017). Worker side: `internal/worker/hooks` runs them through the supervisor, without a shell; the agent runs the phases around the runner (`internal/worker/agent/hooks.go`) and reports every run in `JobResult.hook_runs`. `before` and `on_success` follow the job's context (cancel, time limit, shutdown); `on_failure` and `after` are clean-up and end only with their own timeout. A failing hook fails the job with `HOOK_FAILED`, and a job whose operation succeeded keeps its output whatever its final status. Controller side: limits and name rules are in `internal/domain/hook.go` (shared through `internal/link/hooks.go`); whether a worker defines a definition's hooks (`jobkind.MissingHooks`) and reports the `job.hooks` feature is checked by the API, the scheduler's worker pick and the dispatcher's spec builder, like features and OS; hook runs are stored on the job (`jobs.hook_runs`).

**Two-way sync** (`rclone_bisync`, rclone bisync; ADR 0018): two locations kept in step both ways, with state between runs **on the worker** in `<data_dir>/bisync/<state_key>/` (`internal/worker/bisyncstate`: Syncr's own lock, rclone's work dir, the filters file, `last-run`; pruned after 180 days). The controller computes the state key from both locations and the remotes' non-secret options (`internal/jobkind/bisync_key.go`); the worker only checks its format. A pair runs on one worker, so schedules refuse label targets (`jobkind.SingleWorker`); `resync` and `force` are one-shot and schedules refuse them (`jobkind.OneShot`, `Registry.CheckSchedulable`). Each side holds a `bisync:` concurrency key, so pairs that share a location run one at a time. A pair without a baseline (first run, new key, pruned or interrupted first resync) resyncs on its own (FIRST_RUN), but never into an empty local side: rclone skips its empty-side check during `--resync`, so the worker fails that run with RESYNC_REQUIRED without running rclone (an unmounted drive looks the same), unless every side is local and empty; a missing local side is NOT_FOUND before any resync; an explicit `resync` fills an empty side. After a critical error the worker never resyncs on its own: it reads the pair's state (`bisyncstate.Pair.Resumable`), and when rclone's `--recover` can go on from the previous listings (or the pair has no baseline) the job is EXECUTION_FAILED with the cause to fix; only otherwise RESYNC_REQUIRED. The runner (`internal/worker/runner/rclone/bisync*.go`) always passes `--recover --resilient` and a filters file, clears stale rclone locks under its own lock, refuses a local side that is, holds or is inside the state root (PATH_NOT_ALLOWED), and classifies rclone's abort messages where rclone puts them (never anywhere in a line: file names), tested against recorded 1.75.2 output in `testdata/bisync_*.jsonl` (re-record them on an rclone bump): RESYNC_REQUIRED (29), SAFETY_ABORT (30, what `force` overrides), SIDE_UNAVAILABLE (31). The UI's Resync now and Run once anyway (`web/src/features/jobs/bisync-recovery.tsx`, `planRecovery`) run the schedule's current settings on its page and a job's own only while it is the pair's newest run and its schedule is unchanged. The two rclone paths may be at most 200 bytes together (rclone names its state files after them). User docs: `docs/bisync.md`.

**Restic repositories** (`internal/app/restic`): a remote plus a path and a write-only encrypted password, owner-only (admins included). Create and connect are verified on a worker before anything is stored. The restic job kinds (`internal/jobkind/restic*.go`) carry only the repository ID (plus, for `restic_delete_data`, whether it resumes an earlier attempt); the password is resolved into the spec at dispatch. Repository locations on one remote may not be nested. Deleting a repository forgets it by default; deleting its data is an opt-in `restic_delete_data` job that only `restic.Service.Delete` can start. It deletes only files named like restic's, and finding nothing counts as done only when it resumes an attempt that deleted files on the same worker. The snapshot cache is stale when the repository's change mark (the count of ended jobs that may have changed its snapshots, whatever their status) differs from the mark stored with the cache; no clocks are compared.

**Probes**: short operations a worker answers within the request: folder listings, remote tests, restic snapshots and `ls` (all read-only), and `restic init` or a config read when a repository is created or connected. Controller side: `internal/controller/probe` over the broker in `internal/controller/dispatch`; worker side: `internal/worker/probe`. They are rate-limited per user and bounded by `probe.timeout` and `probe.restic_timeout`, which may not exceed `link.MaxProbeTimeout`; every other probe limit (grace, passive wait, worker retention) derives from it and `link.ProbeGrace`. A caller that gives up on a probe already sent to the worker leaves a cancel_probe. Workers report link features (`internal/link/features.go`); a job or probe that needs a feature the worker lacks fails with `worker_outdated`.

**Concurrency keys**: jobs that share a key run one at a time, first in first out, decided from durable state in the dispatcher. Every restic job holds its repository's key.

**Web UI** (`web/`): React, Vite, TanStack Router/Query/Table, Connect-Query over the generated TS clients, and shadcn-style primitives in `src/components/ui`. Feature code lives in `src/features/<area>`; routes in `src/routes` stay thin.

Design rationale lives in `docs/adr/0001`–`0018`. Read the ADR for a subsystem before changing how it behaves. Configuration (YAML plus `SYNCR_*` env overrides; unknown keys are rejected) is documented in `docs/configuration.md` and `docs/worker-config.md`; restic behavior for users is in `docs/restic.md`, hooks in `docs/hooks.md`, two-way sync in `docs/bisync.md`.
