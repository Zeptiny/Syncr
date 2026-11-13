# Development

Requirements: Go (version from `go.mod`), Node 26 LTS with its bundled npm
(for the web UI; `web/.nvmrc` pins it for nvm),
[buf](https://buf.build/), [golangci-lint](https://golangci-lint.run/) v2,
Docker (for the dev stack), GNU make (optional: CI does not use it). Install
the web dependencies with `cd web && npm ci` (`make web` and `make generate`
do this).

```sh
make help              # list targets
make generate          # npm ci in web/, buf generate (Go and TypeScript), sqlc
make lint              # buf lint + golangci-lint (with layering rules)
make test              # unit tests
make test-race         # unit tests with the race detector (needs cgo)
make tools             # fetch the pinned rclone and restic into .cache/tools
make test-integration  # real rclone/restic (fetched as above), MinIO/SFTP via Docker
make web               # build the web UI into web/dist
make web-check         # typecheck, lint, format-check and test the web UI
make build             # bin/syncr-controller (with the UI after `make web`), bin/syncr-worker
make dev-up            # Postgres 17, MinIO and SFTP on 127.0.0.1 (see deploy/docker-compose.dev.yml)
make dev-seed          # a controller with demo data on the in-memory store (see below)
make release-snapshot  # the release archives and checksums.txt in dist/ (GoReleaser, nothing published)
```

The controller embeds the web UI only when built with `-tags webui` after
`npm run build` in `web/` (that is what `make web build`, CI, the release and
the Docker image do); a plain `go build` serves a placeholder page, so Go work
needs no Node.

Run the controller and check it. It needs a secrets key and a database (or,
for a throw-away demo, the in-memory store):

```sh
make dev-up                       # Postgres on 127.0.0.1:5432
export SYNCR_SECRETS_KEY=$(openssl rand -base64 32)
export SYNCR_DATABASE_URL='postgres://syncr:syncr-dev@127.0.0.1:5432/syncr?sslmode=disable'
./bin/syncr-controller            # listens on :8080; logs the first-run setup code
# or, without Postgres (everything is lost on exit):
#   SYNCR_DEV_MEMORY_STORE=true ./bin/syncr-controller   (unset SYNCR_DATABASE_URL)
curl localhost:8080/healthz
curl -H 'Content-Type: application/json' -d '{}' \
  localhost:8080/syncr.v1.SystemService/GetInfo
```

`SYNCR_SETUP_CODE` presets the setup code (16 or more characters besides
dashes and spaces) instead of a random one, for scripted setups and tests.
Case, dashes and spaces are ignored when the code is entered.

**Web UI development.** `make dev-seed` (or
`SYNCR_DEV_MEMORY_STORE=true SYNCR_DEV_SEED=true SYNCR_SECRETS_KEY=... go run ./cmd/syncr-controller`)
starts a controller whose in-memory store holds fixed demo data: users
`admin` / `syncr-demo-admin` and `alice` / `syncr-demo-alice`, four labelled
workers (online, offline, passive, pending; a fifth was removed), remotes of
every type, a contact of every channel, six schedules with a month of runs
(missed, coalesced, failed, paused ones among them), about 150 jobs over 30
days, one of which keeps running with live progress, and their notifications
in every delivery status. Notification channels only log what they would
send. These passwords are public: never expose a seeded controller.
`dev.seed` is refused without `dev.memory_store`. Then run `npm run dev` in `web/`: Vite serves the UI on
<http://127.0.0.1:5173> and proxies the API to `SYNCR_DEV_API` (default
`http://127.0.0.1:8080`). `VITE_SYNCR_MOCK=1 npm run dev` uses an in-browser
mock API instead.

The worker resolves the pinned rclone and restic, builds its runners and
starts the link configured by `syncr-worker setup` (an unenrolled worker
idles and says so). It exits non-zero with an explanation if a tool is
missing:

```sh
./bin/syncr-worker tools fetch    # install the pinned tools (or set tools.download: true)
./bin/syncr-worker setup --no-service --controller http://127.0.0.1:8080 --token <token> \
  --local-root /tmp/syncr-data --data-dir /tmp/syncr-worker --config /tmp/syncr-worker/worker.yaml
./bin/syncr-worker run --config /tmp/syncr-worker/worker.yaml
./bin/syncr-worker tools status   # what each tool source offers
./bin/syncr-worker version
```

## Running integration tests

Unit tests (`go test ./...`) need nothing but Go. Integration tests are behind
the `integration` build tag and run the real rclone and restic through the real
supervisor, runners and `local_roots` policy:

```sh
make test-integration
# which does, by hand:
go run ./cmd/syncr-worker tools fetch --dir .cache/tools     # pinned, checksum-verified
SYNCR_TEST_RCLONE=.cache/tools/rclone/<version>/<os>-<arch>/rclone \
SYNCR_TEST_RESTIC=.cache/tools/restic/<version>/<os>-<arch>/restic \
  go test -tags integration -count=1 ./...
```

- `SYNCR_TEST_RCLONE` and `SYNCR_TEST_RESTIC` must point at binaries of the
  pinned versions (`syncr-worker tools status` shows them). Tests that need a
  tool skip when its variable is unset. Setting both variables makes
  `make test-integration` skip the download.
- The MinIO and SFTP tests start containers with testcontainers, so they need
  Docker with Linux containers. They use the images of
  `deploy/docker-compose.dev.yml` and skip themselves when Docker is
  unavailable. On Windows they always skip (Windows runners only run Windows
  containers); the local-only tests (rclone local transfers, union, cancel,
  restic local lifecycle, `local_roots`, tool provisioning, worker wiring) run
  there for real.
- `internal/e2e/system` tests the real binaries (Linux only): it builds
  `syncr-controller` and `syncr-worker`, runs the controller on Postgres
  (testcontainers), completes the first-run setup and creates an API token
  through the API, enrolls workers with `syncr-worker setup --no-service` in
  both link modes, runs rclone jobs over stored remotes (local and MinIO)
  through the API, kills and restarts the controller and the worker
  mid-job, runs a schedule every minute into MinIO with a signed webhook
  notification, across a worker outage, and keeps a folder of two workers
  in step through MinIO with two-way syncs. It needs the go toolchain, Docker
  and both tool variables.
- CI runs both flavors: `integration-linux` (with Docker) and
  `integration-windows` (local-only).


## Architecture

```
 Browser (React SPA, embedded in the controller)
        │  HTTPS · Connect-RPC
        ▼
 ┌──────────────── syncr-controller (Go) ─────────────────┐
 │ users · remotes (encrypted secrets) · restic repos ·   │
 │ schedules · job state machine · notifications          │──► Postgres
 └────────────────────────┬───────────────────────────────┘
                          │ worker link: unary RPCs over HTTP/1.1
                          │  active:  worker → controller (Report / Poll)
                          │  passive: controller → worker (Command / Pull, pinned TLS)
                          ▼
 syncr-worker (Go, Linux/Windows service) ── supervises rclone / restic child processes
```

- **Controller**: the only component that talks to Postgres. API, scheduler,
  dispatch and the job state machine live here.
- **Worker**: a small supervisor. Every job is a pinned `rclone` or `restic`
  child process with a minimal environment; remotes are passed as
  `RCLONE_CONFIG_<ALIAS>_*` environment variables, never as connection
  strings. Progress comes from the tools' JSON output.
- **Worker link**: the same messages in both directions; workers buffer events
  in an outbox with `(epoch, seq)` so nothing is lost across reconnects, and
  the controller reconciles after restarts.
- **Modularity**: ports and adapters with constructor injection; `cmd/` is the
  only composition root. New backends, job kinds, notifiers and transports are
  added as one package plus one registration line. Layering is enforced by
  `depguard` in golangci-lint.

Design records: [architecture](adr/0001-architecture.md),
[worker link](adr/0002-worker-link.md),
[child-process engines](adr/0003-child-process-engines.md),
[Postgres store](adr/0004-postgres-store.md),
[runtime wiring and shutdown order](adr/0005-runtime-wiring.md),
[users and sessions](adr/0006-users-sessions.md),
[backend registry and remotes](adr/0007-backend-registry-remotes.md),
[web UI](adr/0008-web-ui.md),
[schedules](adr/0009-schedules.md),
[notifications](adr/0010-notifications.md),
[restic repositories](adr/0011-restic-repositories.md),
[worker probes](adr/0012-worker-probes.md),
[concurrency keys](adr/0013-concurrency-keys.md),
[job time limits and retention](adr/0014-job-time-limits-retention.md),
[one controller per database](adr/0015-single-controller-guard.md),
[releases and versioning](adr/0016-releases-versioning.md),
[job hooks](adr/0017-job-hooks.md),
[two-way sync](adr/0018-two-way-sync.md),
[browser operations](adr/0019-browser-operations.md).

## Repository layout

```
proto/syncr/v1/           wire contracts (api, auth, users, remotes, schedules, notify, dashboard, link)
gen/go/                   generated protobuf + Connect code (committed)
web/                      React web UI (Vite, TypeScript); package web embeds web/dist with -tags webui
cmd/syncr-controller/     controller composition root
cmd/syncr-worker/         worker composition root and CLI (run, setup, service, tools, upgrade, version)
internal/domain/          entity IDs, job status state machine, worker mode/status, domain errors
internal/link/            link helpers shared by both sides (job kinds, error codes, auth headers)
internal/app/users/       accounts, sessions, API tokens, first-run setup
internal/app/remotes/     stored remotes: validation, encrypted secrets, unions, resolution for dispatch
internal/app/workers/     worker registry: enrollment, registration, authentication, link state
internal/app/jobs/        job lifecycle: create, cancel, watch, every status change, progress samples
internal/backend/         backend type registry (fields, validation, rclone mapping); builtin/ has the types
internal/jobkind/         job kinds: check stored definitions, build worker specs from resolved remotes
internal/app/schedules/   schedules and their runs: validation, fire and missed-run/overlap policies
internal/app/restic/      restic repositories: create or connect, encrypted password, snapshot cache, deletion
internal/app/contacts/    notification contacts: validation, encrypted secrets, test sends
internal/app/notifications/ durable notification outbox: planning, delivery with retries, delivery log
internal/cronexpr/        cron expressions with time-zone and daylight-saving rules
internal/descriptor/      typed configuration fields shared by backend types and channels
internal/notify/          notification channel contract, registry, message and rendering
internal/controller/scheduler/ fires due schedules and launches their runs on workers (restart-safe)
internal/controller/dispatch/ controller side of the worker link: ingest, commands, reconcile, long polls, passive loop
internal/controller/dispatch/active/  Connect handlers for active workers (Report, Poll) and enrollment
internal/controller/dispatch/passive/ pinned HTTPS client and supervisor for passive workers
internal/controller/probe/    probes: folder listings, remote tests and restic listings through a worker; the data deleter
internal/controller/config/   controller configuration
internal/adapters/api/    Connect handlers, access policy, sessions, the web UI handler and health endpoint
internal/adapters/password/   argon2id password hashing
internal/adapters/postgres/   Postgres store (pgx, sqlc, goose migrations)
internal/adapters/memstore/   in-memory repositories (tests, dev.memory_store)
internal/adapters/broker/     in-process live job broker (WatchJob)
internal/adapters/storetest/  contract test suite every store adapter runs
internal/adapters/secrets/    AES-256-GCM encryption of secrets at rest, with key IDs
internal/adapters/idgen/      entity ID generators
internal/adapters/notify/     built-in notification channels: discord, email, webhook
internal/worker/agent/    runs the controller's commands through the runners; outbox events
internal/worker/probe/    runs the controller's probes with the runners' tools and local_roots
internal/worker/hooks/    runs the hooks of the worker's configuration that jobs name
internal/worker/outbox/   event buffer with (epoch, seq), acks and coalescing
internal/worker/linkcore/ link envelopes for both transports, no networking
internal/worker/transport/ active (Report/Poll loops) and passive (TLS server) transports
internal/worker/setup/    `syncr-worker setup`: enroll, provision, configure, install, self-check
internal/worker/upgrade/  `syncr-worker upgrade`: download, verify, provision tools, swap, restart or roll back
internal/worker/service/  systemd / Windows Service / per-user service management
internal/worker/tlscert/  self-signed certificate of a passive worker
internal/worker/proc/     child-process supervisor (env allowlist, line streaming, cancellation)
internal/worker/runner/   Runner contract and registry; runner/rclone and runner/restic implement it
internal/worker/bisyncstate/ two-way sync state per pair (lock, rclone work dir, filters, retention)
internal/worker/tools/    pinned rclone/restic provisioning (configured, bundled, managed download, PATH)
internal/worker/localfs/  local_roots policy for local paths
internal/worker/rcloneenv/ env-defined rclone remotes (RCLONE_CONFIG_*), secrets handling
internal/worker/paths/    default data, scratch, cache and tools directories
internal/worker/config/   worker configuration
internal/platform/        leaf helpers: config loading, logging, redaction, HTTP server, client IP, rate limits, network policy (netguard), version
internal/devseed/         demo data for UI development and screenshots (dev.seed, in-memory store only)
internal/e2e/             link end-to-end tests in one process; e2e/system runs the real binaries
deploy/                   Dockerfiles, the quick-start compose file (with a build-from-source override) and the dev compose stack
docs/adr/                 architecture decision records
docs/releases/            release notes per version, the release process and the release candidate checklist
```
