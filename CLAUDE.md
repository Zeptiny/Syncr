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
make dev-seed            # controller on the in-memory store with demo data (admin / syncr-demo-admin)
npm --prefix web run dev # Vite on :5173, proxies the API to 127.0.0.1:8080 (VITE_SYNCR_MOCK=1 for an in-browser mock)
make dev-up              # Postgres, MinIO and SFTP for local runs
```

- Integration tests use the `//go:build integration` tag. They need `SYNCR_TEST_RCLONE` and `SYNCR_TEST_RESTIC` set to binaries of the pinned versions (tests that need a tool skip without them). Postgres, MinIO and SFTP tests start containers with testcontainers. `internal/e2e/system` builds and runs the real binaries.
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
- Job kinds: `internal/jobkind`
- Worker runners: registered in `cmd/syncr-worker/wire.go`

**Stores**: `memstore` (tests, `dev.memory_store`) and `postgres` implement the same ports, and both run the contract suite in `internal/adapters/storetest`. A new store method needs a storetest case and both implementations. Postgres schema changes go in a new numbered goose migration.

**Worker link**: the same messages flow in both modes. In active mode the worker calls the controller (Report/Poll long polls); in passive mode the controller calls the worker over pinned TLS. Workers buffer events in an outbox keyed by `(epoch, seq)`, and the controller reconciles after restarts. Controller side: `internal/controller/dispatch`. Worker side: `internal/worker/{agent,outbox,linkcore,transport}`.

**Worker jobs**: each job is a supervised rclone/restic child process (`internal/worker/proc`, env allowlist). Remotes are passed only as `RCLONE_CONFIG_<ALIAS>_*` env vars, never as connection strings. Progress is parsed from the tools' JSON output.

**Web UI** (`web/`): React, Vite, TanStack Router/Query/Table, Connect-Query over the generated TS clients, and shadcn-style primitives in `src/components/ui`. Feature code lives in `src/features/<area>`; routes in `src/routes` stay thin.

Design rationale lives in `docs/adr/0001`–`0010`. Read the ADR for a subsystem before changing how it behaves. Configuration (YAML plus `SYNCR_*` env overrides; unknown keys are rejected) is documented in README.md and `docs/worker-config.md`.
