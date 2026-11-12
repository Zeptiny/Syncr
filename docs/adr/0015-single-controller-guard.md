# ADR 0015: One controller per database

- Status: accepted (unreleased until v2.0.0)
- Date: 2026-11-11

## Context

Syncr runs one controller per database. Running several was ruled out in
Milestone 5. Nothing enforced that, and a second controller on the same
database did not fail; it misbehaved quietly:

- Every store statement is safe under concurrency (ADR 0004), but the
  controller also keeps state in its own process: the hub that wakes worker
  long polls, the probe broker, the rate limiters, the live job broker.
- A worker polls one controller and a probe asked of the other one times
  out. A job queued through one controller wakes nobody on the other, so
  it starts late. Each controller has its own rate limits, so the limits
  double.

## Decision

### A session-level advisory lock, held for the controller's lifetime

The Postgres store takes `pg_advisory_lock` on a key of its own
(`0x53594e435243`, not the migration lock's) on a **dedicated connection**,
outside the pool, with `statement_timeout` and `lock_timeout` switched off
for that session and **server-side TCP keepalives** switched on
(`tcp_keepalives_idle` 30 s, `tcp_keepalives_interval` 10 s,
`tcp_keepalives_count` 3, and `tcp_user_timeout` 60 s). The controller holds it from start to exit
(`postgres.Store.AcquireControllerLock`, `ControllerLock.Release`). Advisory
locks belong to a database, so two Syncr databases on one server do not
interfere.

The controller takes the lock **before it migrates and before it listens**.
A second controller, possibly a newer version, never changes the schema
under a running one. It does not even open its port: it serves nothing
until it holds the lock.

### A second controller waits and says so

It waits in `pg_advisory_lock`, so Postgres wakes it as soon as the lock
is free. It logs "another controller is running against this database" at
once and every 30 seconds, with the holder's backend PID, client address
and session start, and a hint: if no controller runs there any more,
`SELECT pg_terminate_backend(<pid>)` ends that session at once. When the
first controller stops, the second takes over without a restart.

### A crashed holder loses the lock within about a minute

Postgres releases a session lock when the session ends. That is at once
when the controller exits or closes its connection, and also when its
process is killed, because the operating system closes the connection.
When the controller's *host* crashes or drops off the network, nothing
closes the connection: the server keeps the session, and the lock, until
it notices the peer is gone. With the operating system's default
keepalives that takes about two hours, and a controller restarted on
another host (or the same host after a reboot) would wait that long. The
lock session's keepalives make the server probe it after 30 idle seconds
and end it after three unanswered probes 10 seconds apart, so the waiting
controller gets the lock about a minute after the crash. Keepalives are
not sent while the server waits for an acknowledgement (a crash right
after it answered one of the lock's checks); `tcp_user_timeout` bounds
that case to the same minute. A rebooted host
answers the first probe with a reset, which ends the session at once. A
session over a Unix socket ignores the settings; its peer runs on the
database's own host.

### A lost connection is taken again, or the controller stops

Every 10 seconds the lock's connection is checked. When the check fails,
the connection is closed and opened again. While the database cannot be
reached, the controller retries every 5 seconds and keeps running: the
rest of it cannot use the database either, and a database restart alone
must not stop it. Each connection that works gets one
`pg_try_advisory_lock`:

- **It gets the lock:** the controller carries on.
- **This controller's previous session still holds it:** the server has not
  noticed the broken connection yet. That session is ended
  (`pg_terminate_backend`) and the next attempt takes the lock. A session
  is identified by its PID **and** its start time, so a recycled PID is
  never ended.
- **Another session holds it:** another controller took over meanwhile.
  `Lost()` is closed, and the controller shuts down in the usual order
  (ADR 0005) and exits non-zero. Its service manager restarts it, and it
  waits as the second controller.

### Shutdown

The lock is released last, after the store is closed. A waiting controller
only starts once this one has stopped writing. Release unlocks first and
then closes the connection; closing alone would release it too.

### The in-memory store needs no guard

It lives in one process.

## Consequences

- Two controllers on one database are now an explicit, logged state instead
  of silent misbehaviour. The README states the rule.
- A deployment that starts the new controller before stopping the old one
  waits for the old one to stop. Upgrades cost one stop and one start, not
  more.
- After the lock's connection broke, another controller may hold the lock
  and serve until this one's next check and reconnection, about 10 seconds
  plus the reconnection. In that window the store's statements stay
  race-safe, and the old controller stops at the end of it.
- A waiting controller holds one database connection.
- After a crash of the holder's host, a standby controller waits about a
  minute, or less if an operator ends the old session with the
  `pg_terminate_backend` call the log suggests.
- Session-level advisory locks need a real session. A pooler in
  transaction mode (PgBouncer's `pool_mode = transaction`) would hand the
  lock's statements to different server sessions. The controller must
  reach Postgres directly or through a session-mode pool, which the
  migration lock already needed.

## Alternatives considered

- **A lease row with heartbeats.** It needs expiry times, so it compares
  clocks, and it needs a takeover protocol. Postgres releases a session
  lock itself when the holder's session ends: when its process exits, or,
  after a crash of its host, once the server's keepalives find the
  connection dead (about a minute, see above).
- **Refusing to start while another controller runs.** Waiting makes a
  standby controller and a restart-based failover work without scripts.
- **Only a sentence in the README.** The misbehaviour is quiet enough that
  people would not connect it to the cause.
