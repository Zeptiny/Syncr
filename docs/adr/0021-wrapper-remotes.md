# ADR 0021: Wrapper remotes (encrypted remotes)

- Status: accepted (v2.3.0)
- Date: 2026-11-20

## Context

People asked for files that are encrypted before they reach a bucket or a
server (issue #42). rclone's `crypt` backend does it: it encrypts contents
and names on the machine that runs rclone and stores them in a folder of
another remote. chunker (and alias) work the same way, so whatever crypt
needs, a "wrapper" type will need again.

What Syncr already had, and what shaped the design:

- **Remotes reach rclone only as `RCLONE_CONFIG_<ALIAS>_*` variables**
  (ADR 0007), and every user may run jobs on every worker. A remote string
  from user input could point anywhere, including a worker's local folder
  outside `local_roots`, so the worker's backend allowlist refused crypt:
  its `remote` option is such a string.
- **Unions already refer to other remotes,** through typed members
  (remote, path, mode) stored in `union_members`, checked for ownership by
  the remotes use case, and turned into typed upstream locations that the
  worker builds into rclone's `upstreams` (ADR 0007).
- **Keys hash locations,** not jobs: the two-way sync state key, its
  `bisync:` concurrency keys (ADR 0018) and the remote-source mirror key
  (ADR 0020) are computed from a remote's non-secret options and a path.
  They must stay byte-identical for existing remotes (2.x is released).
- **Nothing knew which backends keep checksums.** Transfers and two-way
  syncs could ask for checksum comparison on any side.

What rclone 1.75.2 does, checked against the pinned binary:

- crypt has no hashes (`backend features`: `Hashes: []`); `copy
  --checksum` silently falls back to size only.
- With a wrong password and the default options, a listing skips names it
  cannot decrypt ("Skipping undecryptable file name") and exits 0, so a
  `sync` from it **deleted every file of its destination**. With
  `strict_names=true` the listing fails. Obfuscated and unencrypted names
  list, but reading fails ("bad password?") and a sync deletes nothing.
- On S3 (and by the same semantics B2) a folder below the crypt root read
  with the wrong password lists as **empty** even with `strict_names`: the
  folder's name encrypts to a prefix that does not exist, and a bucket
  lists a missing prefix as empty. SFTP reports it missing.
- `pass_bad_blocks` with a wrong password and unencrypted names makes
  `copy` exit 0 and write files of zeros.
- `suffix` takes almost anything (a value without a leading dot gets one,
  with an error in the log; a `/` turns every file into a folder).

## Decision

### A typed wrapped reference, not a string

A wrapper type (`backend.Capabilities.Wrapper`) stores its data in a
folder of exactly one other remote of the owner: `remotes.Remote.Wrapped`
(remote ID plus path), stored in `remotes.wrapped_remote_id` and
`wrapped_path` (migration 00012), sent as `RemoteInput.wrapped` and marked
in the descriptor by one field of kind `wrapped` (`FIELD_KIND_WRAPPED`),
which is not a configuration value (like `members`).

- The remotes use case checks it like a union member: a live remote of the
  same owner (another user's is "not one of your remotes", as a missing
  one), not itself, a known type that is not `Local`, a union or a wrapper,
  and a path that `JoinRemote` accepts. Deleting a wrapped remote is
  refused while a live remote wraps it, in the use case and again in the
  store's transaction; postgres locks the wrapped row while a wrapper is
  written (`ShareLiveRemote`), so a wrapper cannot be created against a
  remote being deleted.
- The job builder defines the wrapped remote under its own alias in the
  same spec and sends `RemoteDefinition.wrapped` as a `Location` (alias,
  path joined with its root). The worker builds crypt's `remote` option
  from it and refuses an alias that is local, undefined, itself, a union or
  a wrapper. So a crypt remote can never name a local path or another
  user's remote, and nothing reaches rclone as a remote string.

### No nesting

A wrapper wraps a non-local, non-union, non-wrapper remote, and is never a
union member (it has no `UnionMember`). Resolution therefore stays one level
deep, the worker's checks stay simple (one alias, one kind), and a wrong
password has one place to be wrong. Unions of encrypted remotes, crypt over
a union and crypt over a worker's local folder are follow-ups.

### `strict_names` always, and a parent-listing guard

The worker always sets `strict_names=true` and refuses the options
`remote`, `strict_names` and `pass_bad_blocks` for crypt, and crypt
passwords that are not obscured secrets. A wrong password then fails a
listing instead of presenting an empty remote.

That is not enough on buckets (above), so the worker guards the sources
that could empty something: before a **transfer** reads a folder below a
crypt root, it looks for the folder in its parent's listing
(`lsjson --max-depth=1`, stopping once found); a **backup of a remote**
does the same when its empty-source pre-check finds nothing. A missing
folder is `NOT_FOUND`, naming the password as a likely cause. Two-way sync
needs no guard: rclone aborts on an empty side listing, even with `--force`.

Destinations are not guarded: a wrong password writes new files under
other names and deletes nothing, and since the password is fixed (below),
only a second remote over the same folder can have a wrong one. Probes
(the browser, folder sizes) are read-only and are not guarded either.

### Fixed fields

`backend.Field.Fixed` (`BackendField.fixed`) marks values set at creation
that never change. For crypt: the password, the salt and every name
setting, because another value makes the stored data unreadable. The
remotes use case compares effective values (the value, or the default) on
`UpdateRemote` and on a test of a stored remote, and refuses a different
option, a clear, and **any** new value of a fixed secret, even one equal to
the stored secret: comparing would make `UpdateRemote` an oracle that
confirms password guesses. The form shows fixed fields read-only when
editing and never sends fixed secrets; on create, a fixed secret gets a
warning, a generator and a required "I have saved…" acknowledgement. All
of it is driven by the descriptor, not by the crypt type.

The name and the wrapped location stay editable: pointing at another
folder is how data that was moved is followed.

### Checksums

`backend.Capabilities.NoChecksum` says rclone keeps no checksums on a
type's remotes. Checksum comparison with such a side is refused at every
layer: hidden in the editors, refused by `RunJob`, schedules and the
scheduler (`jobkind.RemoteChecker`, `Registry.CheckWith` with the remotes'
infos), and again when the dispatcher and the probe runner build the spec,
so a definition stored before fails `invalid_spec`. The message is generic
("a remote that keeps no checksums"), since FTP and SMB will set it too.

### Keys go through the wrapper

Where a crypt remote's data lies is where its jobs collide:

- the two-way sync state key and the mirror key (`stateRemote`) append,
  only for a remote with `Wrapped`, the wrapped remote (recursively) and
  the joined path. Keys of every other remote are byte-identical; a test
  pins them;
- a two-way sync side on a wrapper holds the `bisync:` key of the folder
  its data is stored in (`jobkind.RemoteKeyed`), and sides are compared
  for nesting by that folder, at the API and again on the worker;
- a backup of a crypt folder is compared with its repository by the folder
  its data is stored in, at the API (`restic.Service.CheckRemoteSource`),
  at dispatch and on the worker.

### The link feature

Workers that accept crypt definitions report `rclone.crypt`. The
backend-to-feature table is `jobkind/features.go`: the dispatcher fails a
job with a crypt remote on a worker without it (`worker_outdated`, naming
the type), and the probe pick chooses only workers that have it. The
scheduler's worker pick is unchanged, so a label with an older worker can
hit `worker_outdated`.

## Consequences

- Encrypted remotes work everywhere a remote does: transfers, two-way
  sync, the browser, folder sizes, remote tests and backups of a remote
  (mirror or mount). They cannot hold restic repositories (no
  `ResticRepository`: restic encrypts already).
- A crypt source folder that does not exist on S3 or B2 is `NOT_FOUND`
  where a plain bucket's missing folder is an empty source; each such
  transfer costs one more listing.
- A wrong password can still write files under foreign names into the
  folder (a second remote, destinations unguarded); the right remote's
  root listing then fails until they are removed. The docs say so.
- With obfuscated names and a wrong password, a two-way sync sees every
  file as changed; its delete limit is the backstop. The docs recommend
  standard names.
- Changing a wrapper's folder leaves queued jobs with the concurrency keys
  computed at creation, as for union members.
- Mixed worker versions behind a label can fail with `worker_outdated`.
- Changing a password means a new remote and a copy job; there is no
  re-encryption flow.
- An rclone upgrade must review crypt's options again (the worker's audit
  test fails until it is done).

## Alternatives considered

- **crypt's `remote` as an option string, validated:** every validator
  would have to understand rclone's remote syntax (`alias:path`, on-the-fly
  `:backend,opt=…:` remotes, local paths, Windows drive letters) and keep
  up with it; one gap reaches another user's data or a worker's disk. A
  typed reference reuses the ownership checks unions already have.
- **Nesting (crypt over unions, unions of crypt):** possible with the same
  mechanism, but the resolution, the worker's checks, the keys and the
  wrong-password behaviour multiply. Left until someone needs it.
- **Editable passwords:** an edit would silently make every stored file
  unreadable; re-encrypting is a copy into a new remote, which users can
  already run.
- **Comparing an equal secret on update:** friendlier to forms that send
  everything back, but it lets anyone with a token test passwords.
- **Leaving `strict_names` to the user or `pass_bad_blocks` available:**
  both turn a wrong password into an empty or zeroed source that a sync
  copies over good data.
- **Guarding destinations too:** a listing before every write, for a case
  that only a second remote over the same folder can cause and that
  deletes nothing.
- **Hiding checksum in the editors only:** the API, schedules stored
  before, and scripts would still send it, and rclone would silently fall
  back to size only.
