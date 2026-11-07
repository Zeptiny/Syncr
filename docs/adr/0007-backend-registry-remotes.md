# ADR 0007: Backend registry and remotes

- Status: accepted (unreleased until v2.0.0)
- Date: 2026-11-07

## Context

Milestone 3 lets users configure remotes (storage locations with their
credentials) and run jobs on them by ID. Several constraints shape the
design:

- The worker trusts nothing from the controller. `internal/worker/rcloneenv`
  already validates every `RemoteDefinition` it receives: alias and option
  syntax, a denylist of options that run commands or read local files (any
  `*_file`), no `local` backend (local paths are checked against
  `local_roots` instead), no wrapper backends and no nested unions.
- Use cases (`internal/app/**`) may import only the standard library, the
  domain and, from this milestone, the backend core (`internal/backend`).
- The web UI must render remote forms without knowing backend types, so
  that a new backend type needs no frontend change.
- Secrets must never be returned or logged, and remotes are private to
  their owner, admins included.

## Decision

### Backend types are descriptors in a registry

`internal/backend` defines `Type`: an ID, a label, a description, `Fields`
(name, kind, required, secret, advanced, obscure, default, enum choices and
the rclone option name), `Capabilities` (local, union, union member, restic
repository), `Validate(Config)` and `ToRclone(Config)`. A `Config` is the
user's input (non-secret `Options`, `Secrets`); an `Rclone` is a typed rclone
remote (backend, options, secrets, obscured keys, and a `Root` joined in
front of every location path). `Registry` is an immutable set of types,
checked by `CheckDescriptor` when it is built.

The built-in types live in `internal/backend/builtin/<id>` (`s3`, `sftp`,
`local`, `union`, `b2`, `webdav`), each one file of about 60 lines that
declares its fields and delegates to the helpers `CheckFields`,
`MapFields`, `JoinRemote` and `JoinLocal`. `builtin.Types()` lists them.
`BackendService.ListBackends` serves the descriptors to the UI, which builds
its forms from them. Adding a type is one descriptor package plus one line
in `builtin.go`; a test-only type in the API tests proves it.

Fields that are not rclone options (`NotRclone`) are consumed by the
descriptor itself, typically a bucket or base path that becomes `Root`. A
local remote maps to no rclone backend (`Backend == ""`): its locations are
plain local paths. A union's members are not configuration: the remotes use
case owns them, and the job builder turns them into `union_upstreams`.

### Who validates what

- **The worker is the security boundary** and stays unchanged. Descriptors
  do not copy its denylist.
- **Descriptors are the typing and UX layer**: which types are offered,
  their fields, kinds, enums, required and secret flags, defaults and
  cross-field rules (S3 needs an endpoint unless the provider is AWS; SFTP
  needs a password or a key).
- **One cross-check test connects the two.**
  `internal/worker/rcloneenv/backend_accept_test.go` maps every example of
  every built-in type to a `RemoteDefinition` the way the job builder does
  (and a union over every member kind), and asserts that `rcloneenv`
  accepts it. With `SYNCR_TEST_RCLONE` set it also asserts that every
  emitted option is an option of the real rclone backend, because rclone
  ignores unknown `RCLONE_CONFIG_*` variables silently. If a descriptor emits
  something the worker rejects, the descriptor is fixed, not the worker.
- **A contract suite checks every descriptor's behavior.**
  `internal/backend/backendtest` (`Run`, `RunRegistry`, `WithAccept`) checks
  the ID and fields, that `Validate` accepts the examples (which together
  must set every field) and rejects unknown keys, values in the wrong map and
  missing required fields without echoing values, and that `ToRclone`
  emits only declared rclone names, keeps secret values out of options and
  `Root`, lists exactly the obscured secrets, uses the ID as the backend
  (`""` exactly for a local type) and is deterministic. The suite tests
  itself against deliberately broken types.

### Remotes

`internal/app/remotes.Service` implements Create, Get, List, Update, Delete,
UnionsUsing (for the API), Usable (the jobs service checks a new job's
remotes), Resolve (the dispatcher builds a job's spec) and Names (job
listings).

- **Owner only.** Every use case scopes by the actor: another user's remote
  is `ErrNotFound`, admins included, and so is a deleted one. `Usable` and
  `Resolve` fail with `ErrNotFound` if any remote is missing, deleted or
  foreign, which the dispatcher reports as `remote_unavailable`. `Names` is
  the one exception: display references of any owner, deleted ones
  included, for job history.
- **Secrets are write-only.** They are stored as one JSON object encrypted
  with the keyring cipher, with associated data `remote-secrets:<id>`, so a
  ciphertext cannot be moved to another remote. Their key names are stored
  in clear (`secret_keys`) and returned as `secrets_set`. On update a
  non-empty secret replaces the stored one, an absent or empty one keeps it,
  and `clear_secrets` removes optional ones; options are replaced as a whole,
  and the type validates the merged configuration. The update is refused
  with `ErrConflict` unless the caller's version is the stored one, so
  secrets are never merged onto a version the caller did not read. The
  service logs IDs only, never an input; validation messages never contain
  values.
- **Names** are 1 to 64 characters (trimmed, no control characters), unique
  per owner among live remotes, case-insensitively. **Types never change**,
  so a member can never later become a union.
- **Unions** have 2 to 16 members. Each member is a live remote of the same
  owner whose type is a union member and not a union, never the union
  itself. A member's path is checked by joining it to the member's `Root`
  with `JoinRemote` or `JoinLocal`, as the job builder will, and a
  read-write path must not end in a mode suffix (`:ro`, `:nc`,
  `:writeback`). Unions cannot nest (as in the worker), so cycles are
  impossible; `Resolve` still keeps a visited set and a depth limit of one.
  Messages do not tell a missing remote from another user's.
- **Delete is soft**: `deleted_at` is set and the secrets are wiped, so job
  history keeps a valid reference and the name. Deleting a member of a live
  union is `ErrFailedPrecondition` naming the union.

### Storage

`remotes` plus `union_members` (migration 00002). Creating and updating a
remote and its member rows is one transaction; unique violations map to
`ErrAlreadyExists`. A member row is inserted only while the member is live,
under a `FOR SHARE` lock, and a delete locks the remote `FOR UPDATE` before
it checks for unions using it. Either the union commits first and the
delete sees it, or the delete commits first and the member insert finds a
deleted remote (`ErrNotFound`): a live union never has a deleted member.
The `storetest` suite (memory and Postgres) includes that race.

## Consequences

- A backend type needs no proto, API, store or UI change. Its descriptor
  must pass `backendtest` and the cross-check against the worker.
- Secrets are decrypted once per queued job per dispatcher poll. They exist
  in plaintext only in memory and on the authenticated worker link.
- Changing the field set of a type is a data migration concern: stored
  options and secrets are keyed by field name, and a renamed or removed
  field makes existing remotes fail validation on their next update.
- A remote whose type is removed from a build keeps its row, but `Resolve`
  reports it as unavailable and `Update` refuses it.
- SFTP host keys cannot be pinned yet: rclone reads them from a
  `known_hosts` file, which the worker refuses. Testing a remote from the UI
  needs a worker command and is deferred.

## Alternatives considered

- **Raw rclone config strings or free-form option maps.** Rejected: the UI
  could not render forms, the controller could not validate, and every
  check would fall to the worker at run time.
- **Copying the worker's denylist into the descriptors.** Rejected: two
  lists drift apart. The cross-check test keeps the descriptors honest
  instead.
- **Storing each secret in its own column or row.** Rejected: one encrypted
  object per remote is simpler, binds all secrets to the remote with one
  associated data, and keeps the key names queryable in clear.
- **Hard delete with cascading job history.** Rejected: job history must
  keep naming the remote it ran against.
