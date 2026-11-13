# Configuration

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
  download_base_url: ""      # SYNCR_WORKERS_DOWNLOAD_BASE_URL: a mirror for the install and upgrade commands; empty: GitHub, the controller's own release
  lost_after: 1h             # SYNCR_WORKERS_LOST_AFTER: end the active jobs of a worker out of contact this long (at least 5m)
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
schedules:
  tick: 10s                  # SYNCR_SCHEDULES_TICK: scheduler pass interval (1s to 5m; saves and finished jobs wake it earlier)
jobs:
  default_max_runtime: 0     # SYNCR_JOBS_DEFAULT_MAX_RUNTIME: time limit of jobs that set none (0: none; else 1m to 8784h, 366 days)
  sample_retention: 720h     # SYNCR_JOBS_SAMPLE_RETENTION: keep a finished job's progress chart this long (at least 1h; 0: forever)
notify:
  network_policy: private    # SYNCR_NOTIFY_NETWORK_POLICY: private (no loopback, link-local, metadata), public (no private networks either), any
  allowed_networks: []       # SYNCR_NOTIFY_ALLOWED_NETWORKS: CIDRs/IPs allowed anyway (comma-separated), e.g. 127.0.0.1 for a local relay
  max_attempts: 8            # SYNCR_NOTIFY_MAX_ATTEMPTS: tries per notification (backoff 30s doubling to 1h, about an hour in all)
  concurrency: 4             # SYNCR_NOTIFY_CONCURRENCY: notifications sent at once
  send_timeout: 15s          # SYNCR_NOTIFY_SEND_TIMEOUT: one send or test send (1s to 1m)
  retention: 720h            # SYNCR_NOTIFY_RETENTION: how long the delivery log keeps sent and dead notifications
probe:
  timeout: 30s               # SYNCR_PROBE_TIMEOUT: a folder listing or a remote test through a worker (1s to 90s)
  restic_timeout: 60s        # SYNCR_PROBE_RESTIC_TIMEOUT: creating or opening a repository, listing and browsing snapshots (1s to 90s)
  scan_timeout: 45s          # SYNCR_PROBE_SCAN_TIMEOUT: folder sizes, snapshot searches and comparisons, which stop early with what they found (1s to 90s)
dev:
  memory_store: false        # SYNCR_DEV_MEMORY_STORE: demos/tests only, instead of database.url; data is lost on exit
  seed: false                # SYNCR_DEV_SEED: demo data with public passwords (needs memory_store); never expose it
```

The controller refuses to start without `secrets.key` and without either
`database.url` or `dev.memory_store: true`. `syncr-controller -healthcheck`
calls the `/healthz` of the controller configured the same way (for
container health checks; the image has no shell). Behind a reverse proxy, the
proxy's read/idle timeout must exceed 60 s (workers long-poll for 25 s, and
folder sizes, searches and comparisons answer within `probe.scan_timeout`;
if you raise it, keep the proxy above it plus 15 s) and it must accept
request bodies of 16 MiB; set `http.public_url` to its https URL,
and `http.client_ip_header` and `http.trusted_proxies` so the login and
enrollment rate limits see clients, not the proxy. The controller refuses a
`public_url` workers could not use (plain http except on loopback) and warns
at start when session cookies would not be Secure.

Folder sizes, snapshot searches and snapshot comparisons
([browsing files](browsing.md)) walk whole trees, so they are bounded by
`probe.scan_timeout` rather than the other probe timeouts. The worker stops
shortly before it (at 40.5 s of 45 s: the limit minus a tenth, at least
2 s) and answers with what it found, which the UI marks as "at least" or
"stopped early"; it is not an error. Each worker runs one such scan at a
time. Raising the limit (at most 90 s) gets more complete answers from
big folders and buckets, at the cost of longer waits, and needs the
reverse proxy's read timeout raised with it.

Notification targets are checked against `notify.network_policy` at
connect time (the address actually connected to, so DNS names cannot get
around it), redirects are not followed, and proxies (`HTTPS_PROXY`) are not
used for notifications. Links in messages use `http.public_url`. The binary
embeds the time zone database, so schedule time zones work in the distroless
image and on Windows.

A worker that stays out of contact (no report, no successful passive pull)
for `workers.lost_after` loses its jobs: dispatched and running ones end
`interrupted` (`worker_lost`, notified like any interruption), so schedules
waiting on them move on. A job you cancel on a worker that cannot be reached
ends `cancelled` after 5 minutes. Time while the controller itself is down
does not count. If the worker comes back later, it is told to stop those
jobs and its late results are ignored. Raise the value for machines that
sleep through long jobs (e.g. laptops); queued jobs still fail after the
15 minute dispatch timeout.

A job's time limit (the "Time limit" of its definition) is enforced by its
worker: once the job has run that long, its processes are stopped and it
ends `failed` (`time_limit`), notified like any failure, so the next job on
its restic repository can start. `jobs.default_max_runtime` gives a limit to
every job whose definition sets none; jobs the controller starts itself (a
repository's data deletion) never get it. A worker built before 2.0 cannot
enforce a limit and fails such jobs as `worker_outdated`. Once an hour the
controller deletes the progress samples (the chart) of jobs that finished
more than `jobs.sample_retention` ago; the jobs and their final counters
are kept.

`api.admin_token` (`SYNCR_ADMIN_TOKEN`) of Milestone 2 is gone: it is ignored
with a warning. Create a personal API token in the UI instead.

The database URL contains the password; it is never logged or printed. One
controller runs per database: at start it takes a lock held for its
lifetime, then applies the embedded schema migrations. A second controller
on the same database waits for the lock and serves nothing until the first
one stops ([ADR 0015](adr/0015-single-controller-guard.md)). Reach
Postgres directly or through a session-mode pool (not PgBouncer's
transaction mode). For local development, `make dev-up` starts Postgres
with the credentials from `deploy/docker-compose.dev.yml`.

The secrets key encrypts the credentials of remotes and workers at rest. Back
it up: without it, stored secrets cannot be read (jobs using such a remote
fail with an internal error instead of retrying).

The worker accepts the same `log` section plus `data_dir`, `local_roots`,
`tools`, job settings and the `link` section `syncr-worker setup` writes; see
[worker configuration](worker-config.md).
