# Syncr

Self-hosted manager for [rclone](https://rclone.org/) sync jobs and
[restic](https://restic.net/) backups: one controller with a web UI, any
number of workers on servers and desktops (Linux first, Windows second).

> **Status: v2 rewrite in progress.** This branch is a Go rewrite and is not
> usable yet. Milestone 1 (foundation) provides the skeleton: wire contracts,
> binaries that start and stop cleanly, the process supervisor and the job
> runner contract. The previous Django version lives on `main` (tag
> `django-final`).

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
cmd/syncr-worker/         worker composition root and CLI (run, setup, version)
internal/domain/          entity IDs, job status state machine, domain errors
internal/link/            link logic shared by both sides (job kinds today)
internal/adapters/api/    Connect handlers and health endpoint
internal/controller/      controller-only packages (config)
internal/worker/proc/     child-process supervisor (env allowlist, line streaming, cancellation)
internal/worker/runner/   Runner contract and registry for job kinds
internal/worker/config/   worker configuration
internal/platform/        leaf helpers: config loading, logging, redaction, HTTP server, version
deploy/                   Dockerfiles and the dev compose stack
docs/adr/                 architecture decision records
```

## Development

Requirements: Go (version from `go.mod`), [buf](https://buf.build/),
[golangci-lint](https://golangci-lint.run/) v2, Docker (for the dev stack),
GNU make.

```sh
make help              # list targets
make generate          # buf generate (plugins pinned via `go tool`)
make lint              # buf lint + golangci-lint (with layering rules)
make test              # unit tests
make test-race         # unit tests with the race detector (needs cgo)
make test-integration  # real rclone/restic: SYNCR_TEST_RCLONE=… SYNCR_TEST_RESTIC=…
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

The worker currently starts and idles until stopped (the link arrives in
Milestone 2):

```sh
./bin/syncr-worker run
./bin/syncr-worker version
```

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
```

The worker accepts the same `log` section today.

### Images

```sh
docker build -f deploy/Dockerfile.controller -t syncr-controller .   # distroless, non-root
docker build -f deploy/Dockerfile.worker -t syncr-worker .           # Debian slim, rclone v1.75.2, restic 0.19.1, non-root
```
