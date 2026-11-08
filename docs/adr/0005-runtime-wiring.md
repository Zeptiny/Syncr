# ADR 0005: Runtime wiring and shutdown order of the two binaries

- Status: accepted (unreleased until v2.0.0)
- Date: 2026-11-04

## Context

Milestone 2 built the worker link as separate, separately tested packages:
use cases (`internal/app/{workers,jobs}`), the dispatcher and its transports
(`internal/controller/dispatch/{,active,passive}`), the store adapters, the
worker's agent, link core and transports (`internal/worker/...`), and
`syncr-worker setup`. The composition roots (`cmd/syncr-controller`,
`cmd/syncr-worker`) wire them. Several components only behave correctly if
they are wired a particular way: the long polls need a lifetime context and
server timeouts that do not cut them, the job service and the dispatcher must
share one wake-up hub, the passive supervisor must hear about new passive
workers, and both processes must shut down in an order that loses nothing.
This record fixes those rules.

## Decision

### Controller (`cmd/syncr-controller/wire.go`)

```
                         ┌──────────────── one http.Server (h2c; HTTPS with http.tls_cert_file) ───────────────┐
 admin / UI ───────────► │ api.NewHandler: SystemService, /healthz,                                             │
  (session or API token) │   WorkerAdminService, JobService ──► wakingWorkers ──┐   (WatchJob: api BaseContext) │
 active workers ───────► │ active.Server: ControllerLinkService (Report, Poll),  │   (Poll: active Lifetime)    │
                         │   EnrollmentService (rate limit: peer IP or          │                               │
                         │   http.client_ip_header from http.trusted_proxies)    │                               │
                         └───────────────────────────────┬──────────────────────┼───────────────────────────────┘
                                                         ▼                      │ wake (non-blocking) on passive
                                     dispatch.Service ◄── Hub ──► jobs.Service  │ register/enroll, rename, remove
                                     (Report/Poll/Ingest,  (one shared hub:     ▼
                                      Run: dispatch-timeout sweeper)  Waker)  passive.Supervisor ──► one PassiveLoop
                                                         │                 │    per passive worker ──► pinned HTTPS
                                                         ▼                 ▼                           (Command/Pull)
                                     workers.Service ── store (postgres.Open + Migrate, or memstore with
                                                                dev.memory_store) ── secrets.Keyring (secrets.key,
                                                                previous_keys) ── broker (live WatchJob updates)
```

- **Store.** `database.url` selects Postgres (`postgres.Open`, then
  `Migrate` under its advisory lock). Without it the controller refuses to
  start, unless `dev.memory_store: true` explicitly selects the in-memory
  store, which logs a loud warning and loses everything on exit (demos and
  tests only).
- **Secrets.** `secrets.key` is required to serve; `previous_keys` keep
  older ciphertexts readable during a rotation.
- **Services.** `clock.System`, `idgen.UUIDv7`, `crypto/rand.Reader`, and
  one `dispatch.Hub` shared by the job service (as its `Waker`) and the
  dispatcher. The live job broker is its own adapter
  (`internal/adapters/broker`), so a production controller does not depend
  on the in-memory store.
- **HTTP.** One mux: the worker link paths (`active.Server.Mount`) and
  everything else to `api.NewHandler`. `ReadHeaderTimeout` 10 s,
  `ReadTimeout`/`WriteTimeout` 60 s, `IdleTimeout` 120 s (all configurable).
  These only bound ordinary calls: a Poll extends its connection's deadlines
  to the dispatcher's `MaxWait` + 15 s, and a WatchJob stream to
  `api.watch_max_duration` + 30 s, through `http.ResponseController`.
- **Passive wake-up.** The API and the enrollment handler use the workers
  service through a decorator (`wakingWorkers`) that does a non-blocking
  send on the supervisor's `Wake` channel after a passive registration or
  enrollment, a rename and a removal, so a new passive worker is called at
  once rather than at the next 30 s resync.
- **Behind a reverse proxy.** The proxy's read/idle timeout must exceed 40 s
  (25 s long polls plus grace) and it must accept request bodies of 16 MiB
  (link requests). For the enrollment rate limit to see clients rather than
  the proxy, set `http.client_ip_header` and `http.trusted_proxies`; the
  header is honoured only on connections from those addresses, read right to
  left, skipping trusted hops. Without a proxy, `http.tls_cert_file` and
  `http.tls_key_file` make the controller serve HTTPS itself (the files are
  re-read when they change).
- **Users, remotes and jobs (Milestone 3).** `users.Service` gets the
  argon2id hasher (`internal/adapters/password`) and three
  `platform/ratelimit` limiters (login per client IP, per username, setup
  attempts globally); `api.NewSessionAuth` over it is the API's
  authenticator (session cookie with `X-Syncr-CSRF`, or a personal API
  token). `remotes.Service` uses `backend.NewRegistry(builtin.Types()...)` and
  the keyring; it is the jobs service's `RemoteChecker` and, through
  `dispatch.NewSpecs(remotes, jobkind registry)`, resolves a queued job's
  remotes with their credentials whenever the dispatcher builds its
  `StartJob` (the spec is never stored or logged). The API also gets the
  web UI (`web.FS()`, embedded with `-tags webui`), the cookie settings
  (`auth.cookie_secure`, `http.public_url`), the install-command settings
  (`http.public_url`, `workers.download_base_url`, validated at start) and
  the client IP resolver of the rate limits.
- **First run.** While no user exists the controller arms a one-time setup
  code (`auth.setup_code`, or a random one per start) and logs it at WARN as
  `setup_code`. It warns when session cookies would not be Secure, and about
  the removed `api.admin_token`. `dev.seed` (only with `dev.memory_store`)
  fills the store with demo data (`internal/devseed`) before that check.
- **Background loops**, started in this order: the dispatch-timeout
  sweeper, the passive supervisor, the scheduler (ADR 0009), the
  notification outbox (ADR 0010), the hourly session and API token purge
  (`users.Service.PurgeExpired`) and, with `dev.seed`, the demo animation.

**Controller shutdown** (SIGINT/SIGTERM), each step logged:

1. The server's lifetime context ends. It is the API's `BaseContext` (open
   WatchJob streams end with `Unavailable`) and the link's `Lifetime`
   (pending Polls answer at once, empty), so step 2 does not wait for them.
2. `http.Server.Shutdown` with `http.shutdown_timeout`: in-flight Reports and
   API calls finish.
3. The background loops stop: the dispatch-timeout sweeper, the passive
   supervisor (and its loops), the scheduler, the notification outbox (it
   records the outcome of the sends in flight first), the purge and the
   demo animation. They outlive the HTTP server so nothing they start races
   a closed store.
4. The store is closed.

### Worker (`cmd/syncr-worker/link.go`)

```
 runner registry (rclone, restic) ──► agent (MaxConcurrentJobs, ProgressInterval,
                                        Identity: version, OS/arch, hostname, tool versions, local_roots)
                                        │ outbox (epoch, seq)
                                        ▼
                                      linkcore ──► link.mode = active:  transport/active  Report + Poll ──► controller
                                               └─► link.mode = passive: transport/passive TLS server ◄── controller
                                     (link.mode empty: not enrolled; the worker idles and logs a hint)
```

**Worker shutdown** (the service manager's stop, SIGINT/SIGTERM), each step
logged and bounded so the whole stays under the 20 s a service manager allows:

1. The transport's run context ends (the active Report/Poll loops stop; the
   passive server keeps serving).
2. `agent.Shutdown` (12 s: the 10 s tool grace period plus a margin):
   queued jobs are cancelled, running tools are stopped gracefully, then
   killed, and every job leaves a CANCELLED (`WORKER_SHUTDOWN`) result in the
   outbox.
3. The final flush (5 s): active, `Transport.Drain` reports until every
   event is acknowledged; passive, `Server.Shutdown` keeps answering Pulls
   until the controller has acknowledged them. To make that fast, the
   controller's passive loop pulls again after the busy interval (1 s)
   whenever a Pull returned events, since only the next Pull acknowledges
   them.

The controller turns those results into `interrupted` (`worker_shutdown`).
If the flush does not finish, nothing is lost for good: the next worker
process has a new epoch and the controller's reconcile interrupts the jobs
(`worker_restarted`).

## Consequences

- `cmd/syncr-controller` and `cmd/syncr-worker` are tested in process
  (`go test ./cmd/...`: wiring, wake-up, TLS, shutdown order, both link
  modes, the setup self-check probe against a running worker) and as real
  binaries against Postgres, MinIO and the real tools
  (`internal/e2e/system`, integration tag, Linux): both link modes, a
  controller killed and restarted mid-job, a worker killed and restarted
  mid-job, a graceful worker stop mid-job, and restic init + backup to S3.
- A worker killed with SIGKILL leaves its tool processes running (they run
  in their own process group so that a graceful stop can signal them).
  Under systemd the unit's cgroup is killed with it; elsewhere they finish or
  fail on their own.
- A passive worker that no controller ever pulls from waits the 5 s flush
  budget on stop.
