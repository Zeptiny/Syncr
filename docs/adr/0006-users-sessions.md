# ADR 0006: Users, sessions, API tokens and first-run setup

- Status: accepted (unreleased until v2.0.0)
- Date: 2026-11-07

## Context

Until Milestone 2 the controller had one provisional credential: a shared
admin token (`api.admin_token`) that acted as an admin without a user
account. Milestone 3 adds a web UI, remotes owned by users and jobs that
refer to them, so the controller needs real accounts: a way to create the
first one, browser sessions, credentials for scripts, roles, and rules that
keep one user's objects away from another.

Syncr is self-hosted for a small group of trusted people. There is no
public sign-up, no e-mail, and no external identity provider (yet). The
controller is usually one process, sometimes reachable over plain HTTP on a
LAN.

## Decision

### Accounts and roles

- A user has a lower-case username (`^[a-z0-9][a-z0-9._-]{0,63}$`,
  enforced by the use case and a database `CHECK`), a display name, a role
  (`admin` or `user`), an argon2id password hash and an optional
  `disabled_at`. Updates are version-checked (optimistic concurrency).
- **Admins** manage users and workers and may view and cancel every user's
  jobs. **Users** manage their own remotes and jobs. Remotes are strictly
  owner-only, admins included: another user's remote is "not found".
- Roles gate procedures in one policy table in the API adapter
  (`internal/adapters/api/policy.go`; a procedure without an entry is
  denied). The use cases check the admin role again and scope every object
  by the calling `domain.Actor`.
- **Last active admin.** Demoting, disabling or deleting the last active
  admin is refused (`FailedPrecondition`). The service serializes these
  changes with a lock, so the check and the write cannot interleave within
  one controller; after a demotion or disabling it also recounts and undoes
  the change if another controller removed the other admins at the same
  time. A user who owns remotes (deleted ones included) or jobs cannot be
  deleted (the foreign keys refuse it); admins disable such accounts.
- **Disabled users** cannot log in; their sessions are revoked at once and
  any left over are refused on use and purged; their API tokens stop
  authenticating (and work again if the account is re-enabled).

### Workers are shared (trusted-users model)

Workers have no owner. Every authenticated user can list them and run jobs
on any of them; only admins enroll, rename or remove workers. **Any user can
therefore reach every worker's `local_roots`**: a job's local paths are
checked against the worker's roots, not against the user. This is the
trusted-users model; per-user worker grants are future work.

### Passwords

- argon2id, RFC 9106's second recommended option: m = 64 MiB, t = 3,
  p = 4, a 16-byte random salt and a 32-byte key, stored as a PHC string
  `$argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>`
  (`internal/adapters/password`, `golang.org/x/crypto/argon2`). Keys are
  compared with `crypto/subtle`.
- When the parameters change, a successful login stores a new hash.
  Verification accepts the stored parameters within bounds (at most 1 GiB,
  64 passes), so a corrupted row cannot exhaust memory.
- At most four hashes run at once (a semaphore that honors the request's
  context), which caps hashing memory at 256 MiB.
- Passwords are 12 to 256 characters, with no composition rules (NIST
  SP 800-63B). Longer input never matches and costs the same as any other
  attempt.

### First-run setup

- While no user exists, the controller arms a **setup code** at start and
  logs it at WARN (`"first-run setup: open the web UI and enter this setup
  code"`, attribute `setup_code`): 100 random bits as 20 base32 characters,
  shown `XXXXX-XXXXX-XXXXX-XXXXX`, new on every start until setup is done.
  `auth.setup_code` / `SYNCR_SETUP_CODE` (at least 16 characters) presets
  it for automation and tests.
- The service keeps only the SHA-256 of the normalized code (dashes,
  spaces and case ignored) and compares digests in constant time. A wrong
  code is a field error on `setup_code`, so the wizard can show it.
- Attempts are limited globally (burst 5, then one a minute) and per client
  IP. `CompleteSetup` creates the admin with `CreateFirstUser`, which is
  atomic: Postgres takes a transaction-level advisory lock, counts the users
  and inserts in one transaction, so of concurrent attempts exactly one
  wins. The code is discarded after use.

### Browser sessions

- A session token is 32 random bytes (base64url). The database stores only
  its SHA-256, as the primary key; the raw token lives only in the cookie.
- Sessions end after **7 days without use** or **30 days after the login**,
  whichever is first (`auth.session_idle_timeout`, `auth.session_max_age`).
  Use moves the idle expiry forward, never past the absolute one; the store
  is written at most once a minute per session.
- **Revocation.** A password change ends the user's other sessions; an
  admin's password reset, disabling the account or changing its role ends
  all of them. Every request re-reads the user, so a role change or
  disabling takes effect at once anyway. A stream (`WatchJob`, open for up
  to `api.watch_max_duration`) authenticates when it starts and then
  again every 30 s with the same credentials and policy check: once the
  session or token is gone or the user is disabled it ends with
  `Unauthenticated`, and once the refreshed caller no longer sees the job
  (a demoted admin watching another user's job) with `PermissionDenied`.
  A check that errors also ends it (fail closed); clients watch again. An
  open stream counts as session activity, like any request.
- **Cookie.** `__Host-syncr_session` when Secure, otherwise
  `syncr_session`; `HttpOnly; SameSite=Strict; Path=/;
  Max-Age=<remaining lifetime>`. `auth.cookie_secure` is `auto` (Secure
  when the request came over TLS or `http.public_url` is https), `always`
  or `never`. The `__Host-` prefix keeps sibling subdomains and plain-HTTP
  responses from setting or shadowing the cookie. The authenticator reads
  only the name of the request's mode and refuses a request carrying the
  cookie twice. Login and setup always issue a new token (no fixation);
  logout deletes the session and the cookie.

### CSRF

Three cheap layers instead of a double-submit token (the UI is same-origin
and has no HTML forms):

1. `SameSite=Strict` on the cookie.
2. Go's `http.CrossOriginProtection` around the whole API: non-safe
   cross-origin requests are refused by `Sec-Fetch-Site`, or Origin versus
   Host. Clients without these headers (workers, curl) pass.
3. A custom header: every call authenticated by the cookie, plus `Login`
   and `CompleteSetup`, must carry `X-Syncr-CSRF: 1`. CORS is never
   enabled, so other origins cannot send it. A cookie without the header is
   not a credential at all (the call is unauthenticated); `Login` and
   `CompleteSetup` without it are refused before any check, so a forged
   login cannot plant an attacker's session. Bearer calls need no header:
   they carry no ambient credential.

### Personal API tokens

- `syncr_pat_` followed by 32 random bytes (base64url); stored as SHA-256
  (unique). The first 14 characters (`syncr_pat_` plus four) are kept as a
  display prefix. Expiry is optional (one minute to ten years); a user may
  hold 100 tokens. The last use is stored at most once a minute.
- A token acts with its owner's **current** role. It authenticates as
  `Authorization: Bearer syncr_pat_...`; a wrong one fails the call even if
  a valid cookie comes with it. Other `Authorization` headers (a reverse
  proxy's basic auth, other bearer formats) are ignored, so the cookie
  still works behind such a proxy.
- A token caller **cannot change the password, mint or delete tokens**
  (policy level `userSession`), so a leaked token cannot entrench itself.
  For the same reason an admin's token cannot create users or reset
  passwords (`adminSession`): either yields a password that signs in,
  even for the token's own owner.
- Lookups are by hash; with 256 random bits, timing differences of the
  index lookup reveal nothing usable.
- The provisional admin token goes away; the README's curl examples and the
  system tests use API tokens (setup code, `CompleteSetup`, then
  `CreateAPIToken`).

### Login and abuse limits

- `Login` is limited per client IP (burst 10, then one every 30 s) and per
  username, unknown usernames included (burst 5, then one a minute). Every
  attempt counts. `ChangePassword` shares the per-username budget, so
  guessing a password costs the same through either. Exceeding a limit is
  `ResourceExhausted` with a generic message.
- The client IP comes from `internal/platform/clientip` (forwarding headers
  only from `http.trusted_proxies`).
- The limiter (`internal/platform/ratelimit`) is a keyed token bucket with
  at most 10 000 keys. Buckets that have refilled are forgotten when room is
  needed; if none has, new keys share one overflow bucket, so a flood of
  distinct keys cannot grow memory. Usernames that cannot exist share a
  single key, so the keys stay short.
- **No enumeration.** Wrong password, unknown username and disabled account
  are one `Unauthenticated` error, and each costs exactly one argon2id
  verification: unknown usernames are verified against a dummy hash made at
  start. Unknown usernames are not logged (they are often mistyped
  passwords).

### Secrets in logs and errors

Passwords, session tokens, API tokens and the setup code are never logged
or put into errors; the API logs authentication failures with a reason
("session expired", "unknown token") but never the credential, and
returns only generic messages. `Caller.SessionToken` is a `redact.Secret`.

## Consequences

- Every protected call reads the session (or token) and the user: two
  primary-key lookups, plus a write at most once a minute.
- The last-admin rule is exact within one controller. Two controllers
  deleting (not demoting) the last two admins at the same moment could both
  succeed; Syncr runs one controller per database today.
- API tokens of a disabled account come back when it is re-enabled; delete
  them to revoke them for good. Expired tokens stay listed until deleted.
- In `auto` mode over plain HTTP without `http.public_url`, the cookie is
  not Secure; the controller warns at start. Deployments behind a TLS proxy
  should set `http.public_url`.
- Any user can run jobs on every worker and reach its `local_roots`.
