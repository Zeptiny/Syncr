# Syncr

Self-hosted manager for [rclone](https://rclone.org/) sync jobs and
[restic](https://restic.net/) backups: one controller with a web UI, any
number of workers on servers and desktops (Linux first, Windows second).

> **Status: v2 rewrite in progress. Milestone 2 complete: workers enroll and
> run jobs over the link (active or passive); UI next.** Until Milestone 3
> adds the web UI, users and stored remotes, the controller is driven through
> its Connect API with a shared admin token, and jobs carry their complete
> spec (see [Quick start](#quick-start)). The previous Django version lives on
> `main` (tag `django-final`).

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
[child-process engines](docs/adr/0003-child-process-engines.md),
[Postgres store](docs/adr/0004-postgres-store.md),
[runtime wiring and shutdown order](docs/adr/0005-runtime-wiring.md).

## Quick start

Until the web UI arrives (Milestone 3) the controller is driven with `curl`
against its Connect JSON API, authenticated with the admin token.

**1. Start the controller and Postgres** (on the server, from a checkout):

```sh
cp deploy/.env.example deploy/.env   # fill in POSTGRES_PASSWORD, SYNCR_SECRETS_KEY, SYNCR_ADMIN_TOKEN
chmod 600 deploy/.env
docker compose -f deploy/docker-compose.yml up -d --build --wait
curl http://127.0.0.1:8080/healthz   # ok
```

Back up `SYNCR_SECRETS_KEY`: it encrypts job specs and worker secrets. The
controller serves plain HTTP on port 8080; workers on other machines need
HTTPS, so put a TLS-terminating reverse proxy in front of it (read/idle
timeout above 40 s, request bodies up to 16 MiB) or let the controller serve
TLS itself (`SYNCR_HTTP_TLS_CERT_FILE`, `SYNCR_HTTP_TLS_KEY_FILE`; see the
comments in [`deploy/docker-compose.yml`](deploy/docker-compose.yml)).

**2. Create an enrollment token** (single use, valid for an hour):

```sh
export SYNCR_ADMIN_TOKEN='<the value from deploy/.env>'
curl -sS -H "Authorization: Bearer $SYNCR_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{}' http://127.0.0.1:8080/syncr.v1.WorkerAdminService/CreateEnrollmentToken
# {"token":"syncr_et_...", "expiresAt":"..."}
```

**3. Set up a worker** on the machine that should run jobs, with the
`syncr-worker` binary for its platform (see
[Linux](docs/setup-linux.md), [Windows](docs/setup-windows.md)):

```sh
sudo ./syncr-worker setup --controller https://syncr.example.com --token syncr_et_... --local-root /srv/data
# On the controller's own machine, plain http on loopback works:
#   --controller http://127.0.0.1:8080
# Passive worker (the controller calls it; it needs an open port):
#   --passive --listen :7443 --advertise-url https://this-host:7443 --controller ... --token ...
```

`setup` enrolls the worker, provisions the pinned rclone and restic, installs
and starts the service (`--no-service` skips that; then run
`syncr-worker run --config <path>`), and checks the result. The worker shows
as online within seconds:

```sh
curl -sS -H "Authorization: Bearer $SYNCR_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{}' http://127.0.0.1:8080/syncr.v1.WorkerAdminService/ListWorkers
# {"workers":[{"id":"01a1a7c0-...", "name":"...", "mode":"WORKER_MODE_ACTIVE", "status":"WORKER_STATUS_ONLINE", ...}]}
```

**4. Run a job** (a copy between two directories under the worker's
`local_roots`), then follow it:

```sh
curl -sS -H "Authorization: Bearer $SYNCR_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"workerId":"<worker id>","spec":{"rcloneTransfer":{"mode":"RCLONE_TRANSFER_MODE_COPY",
       "source":{"path":"/srv/data/photos"},"destination":{"path":"/srv/data/photos-copy"}}}}' \
  http://127.0.0.1:8080/syncr.v1.JobService/RunJob
# {"job":{"id":"01a1a7c0-...", "status":"JOB_STATUS_QUEUED", ...}}

curl -sS -H "Authorization: Bearer $SYNCR_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"jobId":"<job id>"}' http://127.0.0.1:8080/syncr.v1.JobService/GetJob
# {"job":{..., "status":"JOB_STATUS_SUCCEEDED", "stats":{"bytes":"3000006", "files":"2", ...}, "exitCode":0}}
```

Remotes (S3, SFTP, ...) go into the spec's `remotes` with their options and
secrets; the spec is stored encrypted and never returned. `JobService.WatchJob`
streams status changes and live progress (a Connect server stream; use a
Connect or gRPC client rather than plain `curl`). The messages are in
[`proto/syncr/v1/api.proto`](proto/syncr/v1/api.proto) and
[`link.proto`](proto/syncr/v1/link.proto).

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
internal/controller/dispatch/active/  Connect handlers for active workers (Report, Poll) and enrollment
internal/controller/dispatch/passive/ pinned HTTPS client and supervisor for passive workers
internal/controller/config/   controller configuration
internal/adapters/api/    Connect handlers and health endpoint
internal/adapters/postgres/   Postgres store (pgx, sqlc, goose migrations)
internal/adapters/memstore/   in-memory repositories (tests, dev.memory_store)
internal/adapters/broker/     in-process live job broker (WatchJob)
internal/adapters/storetest/  contract test suite every store adapter runs
internal/adapters/secrets/    AES-256-GCM encryption of secrets at rest, with key IDs
internal/adapters/idgen/      entity ID generators
internal/worker/agent/    runs the controller's commands through the runners; outbox events
internal/worker/outbox/   event buffer with (epoch, seq), acks and coalescing
internal/worker/linkcore/ link envelopes for both transports, no networking
internal/worker/transport/ active (Report/Poll loops) and passive (TLS server) transports
internal/worker/setup/    `syncr-worker setup`: enroll, provision, configure, install, self-check
internal/worker/service/  systemd / Windows Service / per-user service management
internal/worker/tlscert/  self-signed certificate of a passive worker
internal/worker/proc/     child-process supervisor (env allowlist, line streaming, cancellation)
internal/worker/runner/   Runner contract and registry; runner/rclone and runner/restic implement it
internal/worker/tools/    pinned rclone/restic provisioning (configured, bundled, managed download, PATH)
internal/worker/localfs/  local_roots policy for local paths
internal/worker/rcloneenv/ env-defined rclone remotes (RCLONE_CONFIG_*), secrets handling
internal/worker/paths/    default data, scratch, cache and tools directories
internal/worker/config/   worker configuration
internal/platform/        leaf helpers: config loading, logging, redaction, HTTP server, client IP, version
internal/e2e/             link end-to-end tests in one process; e2e/system runs the real binaries
deploy/                   Dockerfiles, the quick-start compose file and the dev compose stack
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

Run the controller and check it. It needs a secrets key and a database (or,
for a throw-away demo, the in-memory store):

```sh
make dev-up                       # Postgres on 127.0.0.1:5432
export SYNCR_SECRETS_KEY=$(openssl rand -base64 32)
export SYNCR_ADMIN_TOKEN=$(openssl rand -base64 32)
export SYNCR_DATABASE_URL='postgres://syncr:syncr-dev@127.0.0.1:5432/syncr?sslmode=disable'
./bin/syncr-controller            # listens on :8080 by default
# or, without Postgres (everything is lost on exit):
#   SYNCR_DEV_MEMORY_STORE=true ./bin/syncr-controller   (unset SYNCR_DATABASE_URL)
curl localhost:8080/healthz
curl -H 'Content-Type: application/json' -d '{}' \
  localhost:8080/syncr.v1.SystemService/GetInfo
```

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
- `internal/e2e/system` tests the real binaries (Linux only): it builds
  `syncr-controller` and `syncr-worker`, runs the controller on Postgres
  (testcontainers), enrolls workers with `syncr-worker setup --no-service`
  in both link modes, runs rclone and restic (MinIO) jobs through the API, and
  kills and restarts the controller and the worker mid-job. It needs the go
  toolchain, Docker and both tool variables.
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
  read_timeout: 60s          # SYNCR_HTTP_READ_TIMEOUT (0: none; long polls and WatchJob extend their own)
  write_timeout: 60s         # SYNCR_HTTP_WRITE_TIMEOUT (0: none)
  idle_timeout: 120s         # SYNCR_HTTP_IDLE_TIMEOUT: keep above the workers' 15s report interval
  shutdown_timeout: 15s      # SYNCR_HTTP_SHUTDOWN_TIMEOUT
  tls_cert_file: ""          # SYNCR_HTTP_TLS_CERT_FILE: serve HTTPS directly (PEM chain); reloaded on change
  tls_key_file: ""           # SYNCR_HTTP_TLS_KEY_FILE
  client_ip_header: ""       # SYNCR_HTTP_CLIENT_IP_HEADER: e.g. X-Forwarded-For, behind a reverse proxy
  trusted_proxies: []        # SYNCR_HTTP_TRUSTED_PROXIES: CIDRs/IPs allowed to set that header (comma-separated)
api:
  admin_token: ""            # SYNCR_ADMIN_TOKEN: bearer token for the worker and job APIs (32+ chars); unset disables them
  request_timeout: 30s       # SYNCR_API_REQUEST_TIMEOUT
  watch_max_duration: 1h     # SYNCR_API_WATCH_MAX_DURATION: WatchJob streams end after this (clients re-watch)
  watch_keepalive: 30s       # SYNCR_API_WATCH_KEEPALIVE
log:
  level: info                # SYNCR_LOG_LEVEL: debug, info, warn, error
  format: text               # SYNCR_LOG_FORMAT: text or json
secrets:
  key: ""                    # SYNCR_SECRETS_KEY: 32 random bytes, base64 (openssl rand -base64 32)
  key_id: 1                  # SYNCR_SECRETS_KEY_ID
  previous_keys: []          # SYNCR_SECRETS_PREVIOUS_KEYS: "<id>:<key>,..." kept for decryption while rotating
database:
  url: ""                    # SYNCR_DATABASE_URL: postgres://user:password@host:5432/syncr?sslmode=require
  max_conns: 10              # SYNCR_DATABASE_MAX_CONNS: connection pool size
  connect_timeout: 10s       # SYNCR_DATABASE_CONNECT_TIMEOUT
dev:
  memory_store: false        # SYNCR_DEV_MEMORY_STORE: demos/tests only, instead of database.url; data is lost on exit
```

The controller refuses to start without `secrets.key` and without either
`database.url` or `dev.memory_store: true`. `syncr-controller -healthcheck`
calls the `/healthz` of the controller configured the same way (for
container health checks; the image has no shell). Behind a reverse proxy, the
proxy's read/idle timeout must exceed 40 s (workers long-poll for 25 s) and it
must accept request bodies of 16 MiB; set `http.client_ip_header` and
`http.trusted_proxies` so the enrollment rate limit sees clients, not the
proxy.

The database URL contains the password; it is never logged or printed. The
Postgres store applies its embedded schema migrations under an advisory lock,
so several controllers can start at once. For local
development, `make dev-up` starts Postgres with the credentials from
`deploy/docker-compose.dev.yml`.

The secrets key encrypts job specs and worker secrets at rest. Back it up:
without it, stored secrets cannot be read.

The worker accepts the same `log` section plus `data_dir`, `local_roots`,
`tools`, job settings and the `link` section `syncr-worker setup` writes; see
[worker configuration](docs/worker-config.md).

### Images

```sh
docker build -f deploy/Dockerfile.controller -t syncr-controller .   # distroless, non-root, HEALTHCHECK via -healthcheck
docker build -f deploy/Dockerfile.worker -t syncr-worker .           # Debian slim, rclone v1.75.2, restic 0.19.1, non-root
```

[`deploy/docker-compose.yml`](deploy/docker-compose.yml) runs the controller
with Postgres (see [Quick start](#quick-start)) and has an optional, commented
worker service that enrolls on first start and keeps its configuration in a
volume. The worker image's default command is
`run --config /var/lib/syncr/worker.yaml`; enroll it once with
`setup --no-service --config /var/lib/syncr/worker.yaml ...`.
