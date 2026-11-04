# Syncr

Self-hosted manager for [rclone](https://rclone.org/) sync jobs and
[restic](https://restic.net/) backups: one controller with a web UI, any
number of workers on servers and desktops (Linux first, Windows second).

> **Status: v2 rewrite in progress. Milestone 1 complete: the worker can run
> rclone/restic jobs locally; the link and controller come next.** The worker
> assembles the pinned tools, the `local_roots` policy and the rclone and
> restic runners at startup, but nothing sends it jobs yet (the worker link is
> Milestone 2), so this branch is not usable end to end. The previous Django
> version lives on `main` (tag `django-final`).

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

Design records: [architecture](docs/adr/0001-architecture.md),
[worker link](docs/adr/0002-worker-link.md),
[child-process engines](docs/adr/0003-child-process-engines.md).

## Repository layout

```
proto/syncr/v1/           wire contracts (api.proto, link.proto)
gen/go/                   generated protobuf + Connect code (committed)
cmd/syncr-controller/     controller composition root
cmd/syncr-worker/         worker composition root and CLI (run, setup, tools, version)
internal/domain/          entity IDs, job status state machine, worker mode/status, domain errors
internal/link/            link helpers shared by both sides (job kinds, error codes, auth headers)
internal/app/workers/     worker registry: enrollment, registration, authentication, link state
internal/app/jobs/        job lifecycle: create, cancel, watch, every status change, progress samples
internal/controller/dispatch/ controller side of the worker link: ingest, commands, reconcile, long polls, passive loop
internal/controller/config/   controller configuration
internal/adapters/api/    Connect handlers and health endpoint
internal/adapters/memstore/   in-memory repositories and live job broker (tests, dev)
internal/adapters/storetest/  contract test suite every store adapter runs
internal/adapters/secrets/    AES-256-GCM encryption of secrets at rest, with key IDs
internal/adapters/idgen/      entity ID generators
internal/worker/proc/     child-process supervisor (env allowlist, line streaming, cancellation)
internal/worker/runner/   Runner contract and registry; runner/rclone and runner/restic implement it
internal/worker/tools/    pinned rclone/restic provisioning (configured, bundled, managed download, PATH)
internal/worker/localfs/  local_roots policy for local paths
internal/worker/rcloneenv/ env-defined rclone remotes (RCLONE_CONFIG_*), secrets handling
internal/worker/paths/    default data, scratch, cache and tools directories
internal/worker/config/   worker configuration
internal/platform/        leaf helpers: config loading, logging, redaction, HTTP server, version
deploy/                   Dockerfiles and the dev compose stack
docs/adr/                 architecture decision records
```

## Development

Requirements: Go (version from `go.mod`), [buf](https://buf.build/),
[golangci-lint](https://golangci-lint.run/) v2, Docker (for the dev stack),
GNU make (optional: CI does not use it).

```sh
make help              # list targets
make generate          # buf generate (plugins pinned via `go tool`)
make lint              # buf lint + golangci-lint (with layering rules)
make test              # unit tests
make test-race         # unit tests with the race detector (needs cgo)
make tools             # fetch the pinned rclone and restic into .cache/tools
make test-integration  # real rclone/restic (fetched as above), MinIO/SFTP via Docker
make build             # bin/syncr-controller, bin/syncr-worker (GOOS/GOARCH to cross-compile)
make dev-up            # Postgres 17, MinIO and SFTP on 127.0.0.1 (see deploy/docker-compose.dev.yml)
```

Run the controller and check it:

```sh
./bin/syncr-controller            # listens on :8080 by default
curl localhost:8080/healthz
curl -H 'Content-Type: application/json' -d '{}' \
  localhost:8080/syncr.v1.SystemService/GetInfo
```

The worker resolves the pinned rclone and restic, builds its runners and then
idles until stopped (the link arrives in Milestone 2). It exits non-zero with
an explanation if a tool is missing:

```sh
./bin/syncr-worker tools fetch    # install the pinned tools (or set tools.download: true)
./bin/syncr-worker run --config worker.yaml
./bin/syncr-worker tools status   # what each tool source offers
./bin/syncr-worker version
```

### Running integration tests

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
- CI runs both flavors: `integration-linux` (with Docker) and
  `integration-windows` (local-only).

### Configuration

Both binaries read an optional YAML file (`--config path` or
`SYNCR_CONFIG`), then apply `SYNCR_*` environment overrides, then validate.
Unknown keys are rejected.

```yaml
# controller.yaml
http:
  addr: ":8080"              # SYNCR_HTTP_ADDR
  read_header_timeout: 10s   # SYNCR_HTTP_READ_HEADER_TIMEOUT
  shutdown_timeout: 15s      # SYNCR_HTTP_SHUTDOWN_TIMEOUT
log:
  level: info                # SYNCR_LOG_LEVEL: debug, info, warn, error
  format: text               # SYNCR_LOG_FORMAT: text or json
secrets:
  key: ""                    # SYNCR_SECRETS_KEY: 32 random bytes, base64 (openssl rand -base64 32)
  key_id: 1                  # SYNCR_SECRETS_KEY_ID
  previous_keys: []          # SYNCR_SECRETS_PREVIOUS_KEYS: "<id>:<key>,..." kept for decryption while rotating
```

The secrets key encrypts job specs and worker secrets at rest. Back it up:
without it, stored secrets cannot be read.

The worker accepts the same `log` section plus `data_dir`, `local_roots` and a
`tools` section; see [worker configuration](docs/worker-config.md).

### Images

```sh
docker build -f deploy/Dockerfile.controller -t syncr-controller .   # distroless, non-root
docker build -f deploy/Dockerfile.worker -t syncr-worker .           # Debian slim, rclone v1.75.2, restic 0.19.1, non-root
```
