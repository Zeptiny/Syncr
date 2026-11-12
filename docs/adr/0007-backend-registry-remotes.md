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

- **The worker is the security boundary**. Descriptors do not copy its
  rules. It accepts only an allowlist of rclone backend types
  (`rcloneenv.SupportedBackends`: the backends of the built-in types plus
  `memory` for tests), not a denylist: rclone has more backends that read
  the worker's files through free-form remote strings (`archive`, `alias`,
  `crypt`...) or sign in as the worker host (Azure CLI and managed
  identities, cloud metadata servers) than a denylist can keep up with. A
  backend is added to the allowlist only after its options in the pinned
  rclone have been reviewed against the option denylist, which also
  rejects options that use the worker host's identity (`env_auth`,
  `key_use_agent`, `use_az`, `use_msi`, `use_kerberos`); an SFTP remote
  must carry its own password or key, or rclone would use the host's
  ssh-agent. With `SYNCR_TEST_RCLONE` set,
  `TestSupportedBackendOptionsReviewed` fails on any option of a
  supported backend that is neither denied nor listed as reviewed, so an
  rclone upgrade cannot add one silently.
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
  The same file checks that the worker's backend allowlist holds exactly
  the backends of the built-in types (plus `memory`).
- **A contract suite checks every descriptor's behavior.**
  `internal/backend/backendtest` (`Run`, `RunRegistry`, `WithAccept`) checks
  the ID and fields, that `Validate` accepts the examples (which together
  must set every field) and rejects unknown keys, values in the wrong map and
  missing required fields without echoing values, and that `ToRclone`
  emits only declared rclone names, keeps secret values out of options and
  `Root`, lists exactly the obscured secrets, uses the ID as the backend
  (`""` exactly for a local type) and is deterministic. A type may also
  list configurations its `Validate` must reject (`InvalidExampler`,
  Milestone 6); the suite checks that each is rejected with an error on
  the named field that echoes no value. The suite tests itself against
  deliberately broken types.

### SFTP host keys (Milestone 6)

Without a pinned key rclone accepts any SFTP server key ("No host key
validation"). rclone has three ways to pin one, and only one fits the
worker: `known_hosts_file` reads a local file (denied, like every
`*_file` option), and `pin_host_key` writes the key it saw into the
per-job config file, which starts empty and is removed after the job. So
the `sftp` type has a `host_keys` field ("Server host keys", multi-line
text, not a secret), mapped to rclone's `host_keys` option, which
`rcloneenv` already accepted after its option review.

- **Format.** rclone takes full public keys, not fingerprints: a
  comma-separated list of `<algorithm> <base64 key>` entries, the second
  and third fields of a `known_hosts` line, at most 16. The field accepts
  one entry per line or comma-separated (what a user pastes, or what the
  UI's "Fetch from server" fills in), and `ToRclone` normalizes it to
  rclone's form: entries joined by commas, one space inside each. The
  stored value is what the user entered.
- **Validation** uses the standard library only: at most 16 entries; each
  is exactly two fields; the key is padded standard base64; and the key is
  an SSH wire-format public key whose first length-prefixed string (the
  key type) equals the entry's algorithm, with key data after it. Whether
  the key's numbers form a valid point or modulus is left to rclone, which
  parses the key when it connects. Messages name the entry by position
  and never echo it.
- **Optional.** Saving a remote without host keys stays allowed (the user's
  decision); the UI warns that the server is not verified and offers to
  fetch the keys through a worker (`ProbeService.GetSftpHostKeys`, ADR
  0012). Fetching is trust on first use: the UI shows the SHA-256
  fingerprints and how to compare them on the server.
- **Key types.** If only some of the server's key types are pinned, rclone
  may negotiate another type and refuse the server. The fetch collects
  every type the server offers (ed25519, the ECDSA curves, RSA), and the
  worker's SFTP integration test pins all of them.

### Bucket storage

`Capabilities.ObjectStore` marks types whose remotes keep objects in
buckets (`s3`, `b2`). There a folder is only a prefix of object names, so
a mistyped folder lists as empty instead of failing; messages about a
location that holds nothing (restic's "no restic repository at this
location") say so. A local or union type never sets it.

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
  must pass `backendtest` and the cross-check against the worker, and a
  new rclone backend must be reviewed and added to the worker's allowlist
  (one line in `internal/worker/rcloneenv`). A worker older than the
  controller rejects remotes of a backend it does not support yet, and the
  job fails with an error naming the backend type.
- Secrets are decrypted once per queued job per dispatcher poll. They exist
  in plaintext only in memory and on the authenticated worker link.
- Changing the field set of a type is a data migration concern: stored
  options and secrets are keyed by field name, and a renamed or removed
  field makes existing remotes fail validation on their next update.
- A remote whose type is removed from a build keeps its row, but `Resolve`
  reports it as unavailable and `Update` refuses it.
- SFTP host keys are pinned per remote (`host_keys`, above); a remote
  saved without them connects to any server that answers at its address.
  When a server's keys change, every job on the remote fails until the
  keys are fetched or entered again.

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
