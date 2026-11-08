# Syncr

Self-hosted manager for [rclone](https://rclone.org/) sync jobs and
[restic](https://restic.net/) backups: one controller with a web UI, any
number of workers on servers and desktops (Linux first, Windows second).

> **Status: v2 rewrite in progress. Milestone 3 complete: web UI, users,
> stored remotes and jobs that use them.** Workers enroll and run rclone jobs
> over the link (active or passive); schedules, restic repositories and
> notifications come next. The previous Django version lives on `main` (tag
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
[child-process engines](docs/adr/0003-child-process-engines.md),
[Postgres store](docs/adr/0004-postgres-store.md),
[runtime wiring and shutdown order](docs/adr/0005-runtime-wiring.md),
[users and sessions](docs/adr/0006-users-sessions.md),
[backend registry and remotes](docs/adr/0007-backend-registry-remotes.md),
[web UI](docs/adr/0008-web-ui.md).

## Quick start

**1. Start the controller and Postgres** (on the server, from a checkout):

```sh
cp deploy/.env.example deploy/.env   # fill in POSTGRES_PASSWORD, SYNCR_SECRETS_KEY (and SYNCR_PUBLIC_URL behind a proxy)
chmod 600 deploy/.env
docker compose -f deploy/docker-compose.yml up -d --build --wait
curl http://127.0.0.1:8080/healthz   # ok
```

Back up `SYNCR_SECRETS_KEY`: it encrypts the stored credentials of remotes
and workers. The controller serves the web UI and the API with plain HTTP on
port 8080. Browsers should, and workers on other machines must, reach it over
HTTPS: put a TLS-terminating reverse proxy in front of it (read/idle timeout
above 40 s, request bodies up to 16 MiB) and set `SYNCR_PUBLIC_URL` to its
`https://` address, or let the controller serve TLS itself
(`SYNCR_HTTP_TLS_CERT_FILE`, `SYNCR_HTTP_TLS_KEY_FILE`; see the comments in
[`deploy/docker-compose.yml`](deploy/docker-compose.yml)).

**2. Create the first admin.** Until an account exists, the controller logs
a one-time setup code at every start:

```sh
docker compose -f deploy/docker-compose.yml logs controller | grep setup_code
# {"level":"WARN","msg":"first-run setup: open the web UI and enter this setup code","setup_code":"7QK2M-XC4PA-..."}
```

Open the web UI (`http://127.0.0.1:8080`, or your `SYNCR_PUBLIC_URL`). The
setup wizard asks for the code, then for the admin's username and password
(12 characters or more). Admins add more users under **Users**.

**3. Add a worker.** In the UI open **Workers, Add worker**. The **Active**
tab creates a single-use enrollment token and shows the install command for
Linux or Windows (system service or per-user). Run it on the machine that
should run jobs; see [Linux](docs/setup-linux.md) and
[Windows](docs/setup-windows.md) for the details and the passive mode (the
controller calls the worker). If `syncr-worker` is already installed, run
only its last line:

```sh
sudo syncr-worker setup --controller https://syncr.example.com --token syncr_et_... --local-root /srv/data
# On the controller's own machine, plain http on loopback works:
#   --controller http://127.0.0.1:8080
```

`setup` enrolls the worker, provisions the pinned rclone and restic, installs
and starts the service (`--no-service` skips that; then run
`syncr-worker run --config <path>`), and checks the result. The worker shows
as online within seconds.

**4. Add remotes and run jobs.** Under **Remotes** store your storage
backends (S3 and compatibles, SFTP, Backblaze B2, WebDAV, local folders of a
worker, and unions of them). Credentials are encrypted at rest, never shown
again and only sent to the worker that runs a job. **Jobs, Run job** copies,
syncs or moves between a remote or a worker folder (inside its
`local_roots`) and another, on a worker you pick; the job page follows its
progress live.

**Scripting the API.** Everything the UI does goes through the Connect API
([`proto/syncr/v1/`](proto/syncr/v1/)). For scripts, create a personal API
token under **Settings, API tokens** (it acts as you, with your role) and
send it as a bearer token:

```sh
export SYNCR_TOKEN='syncr_pat_...'   # shown once when you create it
curl -sS -H "Authorization: Bearer $SYNCR_TOKEN" -H 'Content-Type: application/json' \
  -d '{}' http://127.0.0.1:8080/syncr.v1.WorkerAdminService/ListWorkers
# {"workers":[{"id":"01a1a7c0-...", "name":"nas", "mode":"WORKER_MODE_ACTIVE", "status":"WORKER_STATUS_ONLINE", ...}]}

curl -sS -H "Authorization: Bearer $SYNCR_TOKEN" -H 'Content-Type: application/json' \
  -d '{"workerId":"<worker id>","definition":{"rcloneTransfer":{"mode":"RCLONE_TRANSFER_MODE_COPY",
       "source":{"path":"/srv/data/photos"},"destination":{"remoteId":"<remote id>","path":"photos"}}}}' \
  http://127.0.0.1:8080/syncr.v1.JobService/RunJob
# {"job":{"id":"01a1a7c0-...", "status":"JOB_STATUS_QUEUED", ...}}
```

A location is a path on one of your remotes (`remoteId`, path relative to its
root) or, without `remoteId`, an absolute path on the worker inside its
`local_roots`. `JobService.WatchJob` streams status changes and live
progress (a Connect server stream; use a Connect or gRPC client rather than
plain `curl`). Calls with the browser's session cookie need the
`X-Syncr-CSRF: 1` header; bearer calls do not. API tokens cannot create or
delete tokens or change the password.

## Repository layout

```
proto/syncr/v1/           wire contracts (api, auth, users, remotes, link)
gen/go/                   generated protobuf + Connect code (committed)
web/                      React web UI (Vite, TypeScript); package web embeds web/dist with -tags webui
cmd/syncr-controller/     controller composition root
cmd/syncr-worker/         worker composition root and CLI (run, setup, tools, version)
internal/domain/          entity IDs, job status state machine, worker mode/status, domain errors
internal/link/            link helpers shared by both sides (job kinds, error codes, auth headers)
internal/app/users/       accounts, sessions, API tokens, first-run setup
internal/app/remotes/     stored remotes: validation, encrypted secrets, unions, resolution for dispatch
internal/app/workers/     worker registry: enrollment, registration, authentication, link state
internal/app/jobs/        job lifecycle: create, cancel, watch, every status change, progress samples
internal/backend/         backend type registry (fields, validation, rclone mapping); builtin/ has the types
internal/jobkind/         job kinds: check stored definitions, build worker specs from resolved remotes
internal/controller/dispatch/ controller side of the worker link: ingest, commands, reconcile, long polls, passive loop
internal/controller/dispatch/active/  Connect handlers for active workers (Report, Poll) and enrollment
internal/controller/dispatch/passive/ pinned HTTPS client and supervisor for passive workers
internal/controller/config/   controller configuration
internal/adapters/api/    Connect handlers, access policy, sessions, the web UI handler and health endpoint
internal/adapters/password/   argon2id password hashing
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
internal/platform/        leaf helpers: config loading, logging, redaction, HTTP server, client IP, rate limits, version
internal/devseed/         demo data for UI development and screenshots (dev.seed, in-memory store only)
internal/e2e/             link end-to-end tests in one process; e2e/system runs the real binaries
deploy/                   Dockerfiles, the quick-start compose file and the dev compose stack
docs/adr/                 architecture decision records
```

## Development

Requirements: Go (version from `go.mod`), Node 22 (for the web UI),
[buf](https://buf.build/), [golangci-lint](https://golangci-lint.run/) v2,
Docker (for the dev stack), GNU make (optional: CI does not use it). Install
the web dependencies with npm 11 (`cd web && npx npm@11.6.2 ci`, which
`make web` and `make generate` do): the npm 10 bundled with Node 22 rejects
the lockfile.

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

`SYNCR_SETUP_CODE` presets the setup code (16 or more characters) instead of
a random one, for scripted setups and tests.

**Web UI development.** `make dev-seed` (or
`SYNCR_DEV_MEMORY_STORE=true SYNCR_DEV_SEED=true SYNCR_SECRETS_KEY=... go run ./cmd/syncr-controller`)
starts a controller whose in-memory store holds fixed demo data: users
`admin` / `syncr-demo-admin` and `alice` / `syncr-demo-alice`, four workers
(online, offline, passive, pending), remotes of every type and about 25 jobs,
one of which keeps running with live progress. These passwords are public:
never expose a seeded controller. `dev.seed` is refused without
`dev.memory_store`. Then run `npm run dev` in `web/`: Vite serves the UI on
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
  (testcontainers), completes the first-run setup and creates an API token
  through the API, enrolls workers with `syncr-worker setup --no-service` in
  both link modes, runs rclone jobs over stored remotes (local and MinIO)
  through the API, and kills and restarts the controller and the worker
  mid-job. It needs the go toolchain, Docker and both tool variables.
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
  public_url: ""             # SYNCR_PUBLIC_URL: e.g. https://syncr.example.com behind a proxy (install commands, Secure cookie)
api:
  request_timeout: 30s       # SYNCR_API_REQUEST_TIMEOUT
  watch_max_duration: 1h     # SYNCR_API_WATCH_MAX_DURATION: WatchJob streams end after this (clients re-watch)
  watch_keepalive: 30s       # SYNCR_API_WATCH_KEEPALIVE
auth:
  cookie_secure: auto        # SYNCR_AUTH_COOKIE_SECURE: auto (TLS or https public_url), always, never
  session_idle_timeout: 168h # SYNCR_AUTH_SESSION_IDLE_TIMEOUT: a session unused this long ends
  session_max_age: 720h      # SYNCR_AUTH_SESSION_MAX_AGE: a session ends this long after the login
  setup_code: ""             # SYNCR_SETUP_CODE: preset first-run setup code (16+ chars); default: random, logged
workers:
  download_base_url: https://github.com/Zeptiny/Syncr/releases/latest/download  # SYNCR_WORKERS_DOWNLOAD_BASE_URL (install commands)
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
  seed: false                # SYNCR_DEV_SEED: demo data with public passwords (needs memory_store); never expose it
```

The controller refuses to start without `secrets.key` and without either
`database.url` or `dev.memory_store: true`. `syncr-controller -healthcheck`
calls the `/healthz` of the controller configured the same way (for
container health checks; the image has no shell). Behind a reverse proxy, the
proxy's read/idle timeout must exceed 40 s (workers long-poll for 25 s) and it
must accept request bodies of 16 MiB; set `http.public_url` to its https URL,
and `http.client_ip_header` and `http.trusted_proxies` so the login and
enrollment rate limits see clients, not the proxy. The controller refuses a
`public_url` workers could not use (plain http except on loopback) and warns
at start when session cookies would not be Secure.

`api.admin_token` (`SYNCR_ADMIN_TOKEN`) of Milestone 2 is gone: it is ignored
with a warning. Create a personal API token in the UI instead.

The database URL contains the password; it is never logged or printed. The
Postgres store applies its embedded schema migrations under an advisory lock,
so several controllers can start at once. For local
development, `make dev-up` starts Postgres with the credentials from
`deploy/docker-compose.dev.yml`.

The secrets key encrypts the credentials of remotes and workers at rest. Back
it up: without it, stored secrets cannot be read (jobs using such a remote
fail with an internal error instead of retrying).

The worker accepts the same `log` section plus `data_dir`, `local_roots`,
`tools`, job settings and the `link` section `syncr-worker setup` writes; see
[worker configuration](docs/worker-config.md).

### Images

```sh
docker build -f deploy/Dockerfile.controller -t syncr-controller .   # web UI embedded, distroless, non-root, HEALTHCHECK via -healthcheck
docker build -f deploy/Dockerfile.worker -t syncr-worker .           # Debian slim, rclone v1.75.2, restic 0.19.1, non-root
```

[`deploy/docker-compose.yml`](deploy/docker-compose.yml) runs the controller
with Postgres (see [Quick start](#quick-start)) and has an optional, commented
worker service that enrolls on first start and keeps its configuration in a
volume. The worker image's default command is
`run --config /var/lib/syncr/worker.yaml`; enroll it once with
`setup --no-service --config /var/lib/syncr/worker.yaml ...`.
