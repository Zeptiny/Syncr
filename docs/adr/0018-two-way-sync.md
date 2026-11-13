# ADR 0018: Two-way sync

- Status: accepted (v2.1.0)
- Date: 2026-11-12

## Context

People want one folder on two machines kept in step through a bucket: a
desktop and a laptop, each with the folder, and one Cloudflare R2 remote
between them. The only operation Syncr has is `rclone_transfer` (copy,
sync, move), which goes one way. Two copy jobs per machine with `--update`
come close, but deletions never spread, and the older side of a conflict
is overwritten without a trace.

`rclone bisync` does this properly, and the worker already ships a pinned
rclone (1.75.2) that has it. Running it showed what shapes the design:

- bisync has **state**: the listings of both sides after the last run,
  which the next run compares against. Without them it refuses to run and
  asks for `--resync`, which rebuilds them as the union of both sides.
- It writes that state, its lock and its queues into a **work directory**.
  The default (`~/.cache/rclone/bisync`) would land in the per-job scratch
  directory that the rclone runner deletes after every job (ADR 0003).
- It names its state files after **both full paths** (`/a/b` and
  `r1:bucket/x` give `a_b..r1_bucket_x.path1.lst`). Two long paths overflow
  the 255-byte file-name limit, and rclone then reports a misleading
  "prior lock file found". An `alias` remote does not help: rclone
  resolves it to the underlying path.
- It has **safety checks** that stop a run: more deletions than
  `--max-delete` (for bisync, a percentage; default 50), every file
  changed on both sides, an empty side, missing `RCLONE_TEST` files with
  `--check-access`. `--force` runs past the first two. The state is kept.
- Some errors are **critical**, and only a resync recovers from them.
  Changing the filters is one: rclone keeps an MD5 of the filters file
  next to it, and with `--resilient` every later run fails with "(must run
  --resync)" while the listings stay as they were. Others, such as a file
  that still cannot be read after three tries, rename the listings to
  `.lst-err` ("Must run --resync to recover").
- A killed run leaves its `.lck` file behind; the next run with `--recover`
  finishes without a resync once the lock is gone. A kill during a
  transfer can also leave a `.partial` file on the receiving side, which
  the next run copies across and the run after removes.
- On SIGINT, bisync lets the transfers under way finish for up to 30
  seconds before it cancels them. When the signal arrives just as a
  transfer ends, its clean-up can remove the `.partial` file the copy is
  still finishing, and it reports that as a critical error: rarely, a
  cancelled run leaves a pair that needs a resync.
- A dry run keeps its listings apart (`.lst-dry`) and changes no state.
- "New or changed in both paths" is logged for every file new or changed
  on both sides, also when both versions are equal; only the rename or
  winner lines after it mark a real conflict.
- On a backend without modification times, bisync warns and compares by
  size only: an edit that keeps the size goes unnoticed, even with
  checksums when the sides share no hash.

Constraints from Syncr itself: the controller builds a secret-free
definition into a spec at dispatch (ADR 0007), every user may run jobs on
every worker, schedules may target a label (ADR 0009), and jobs that must
not overlap share concurrency keys (ADR 0013).

## Decision

### A new job kind

Two-way sync is a job kind of its own, `rclone_bisync` ("Two-way sync" in
the UI): two locations (`path1`, `path2`; "Folder A" and "Folder B"),
typed options, and two one-shot flags, `resync` and `force`. Each option
maps to one fixed flag after validation, as for transfers; no free-form
flags. The worker reports the feature `rclone.bisync`; a worker without it
fails the job with `WORKER_OUTDATED`.

### State lives on the worker, under a key the controller computes

A pair's state lives in `<data_dir>/bisync/<state_key>/` on the worker
that ran it: `syncr.lock` (Syncr's own lock), `work/` (rclone's
`--workdir`), the filters file and rclone's MD5 of it, and `last-run`.

The controller computes `state_key` and sends it in the spec: the SHA-256
(64 lower-case hex digits) of a canonical encoding of a version tag and,
per side in order, `local` and the cleaned path (also for a remote of the
local backend, with its root resolved), or `remote` with the remote's ID,
backend type, non-secret options sorted by key, its union upstreams the
same way, and the joined path. Secrets and obscured values never enter it,
nor do options named like a secret. So:

- the same pair gets the same key on every worker version;
- a rotated secret keeps the key;
- any other change of a location (endpoint, bucket, host, root, path), or
  swapping the sides, gives a new key: the worker treats it as a new pair,
  and never compares old listings against a different place.

The key takes every non-secret option, because the descriptors do not
say which options name the place and which only tune the access. Some
identify the credential rather than the place: S3's `access_key_id` is an
option, so rotating an R2 or S3 token, which changes it, makes the next
run of the remote's pairs a resync, and so does changing a tuning option.
That costs a resync (a union) and never compares listings against a
different place; a descriptor flag for "identity, not location" options
would avoid it.

The worker computes nothing from the key. It checks the format
(`^[0-9a-f]{64}$`) before the key becomes a directory name.

### One worker per pair

The state is on one worker, so a schedule of a two-way sync must target a
worker, not a label (`jobkind.SingleWorker`, asked through
`Registry.LabelTargetAllowed`). RunJob always names a worker. Running a
pair on another worker is allowed: there it is a new pair, and its first
run resyncs.

### Resync: automatic only for a pair without a baseline

A pair has no baseline on a worker when its work directory holds no
listing from a completed run with entries: its first run there, a new
state key, state removed for age, a first resync that was cancelled or
killed part-way (it leaves only `-new` listings, and rclone says to run it
again), or a resync of two empty sides (rclone refuses to sync against
empty listings). Dry-run listings never count; listings renamed after a
critical error (`-err`) always do, and so does a listing of a kind the
worker does not know. Without a baseline the worker runs with `--resync`
on its own and reports the reason `FIRST_RUN`. A resync is a union: what one side lacks is copied
from the other and nothing is deleted. A file that differs keeps the newer
version by default (`resync_mode`; rclone's own default is Path1).

After a critical error the worker never resyncs on its own: the job fails
with `RESYNC_REQUIRED`, and a person starts a job with `resync` set
("Resync now"). A critical error means something unexpected happened, and a
resync can overwrite a version. Changing the filters is the common case:
the worker always passes the filters as a file, empty without rules, so
that rclone notices every change, including removing all rules.

### One-shot fields

`resync` and `force` are for a single run. A schedule would repeat them
every time, so schedules refuse a definition that sets them
(`jobkind.OneShot`, asked through `Registry.CheckSchedulable` next to
`Schedulable`). RunJob accepts them, and the job keeps them in its stored
definition, so its page says "Resync" or "Delete limit ignored". The UI
offers them as actions on a failed job: "Resync now" after
`RESYNC_REQUIRED`, "Run once anyway" after `SAFETY_ABORT`.

### Two codes for the safety checks

rclone's safety checks fall into two groups, and `--force` overrides only
one of them, so each group has its own error code:

- `SAFETY_ABORT`: more deletions than `max_delete_percent` (default 50),
  or every file changed on both sides. The changes may be intended (a
  folder cleaned out on purpose, a clock change), and a one-shot `force`
  run applies them.
- `SIDE_UNAVAILABLE`: a side's current listing is empty, or the
  `check_access` files (`RCLONE_TEST`) are missing. `force` does not
  override these; the fix is the location (mount the drive, create the
  check files), then a normal run.

Neither needs a resync: the state is kept. A side that does not exist at
all is `NOT_FOUND`. With one code, the UI could only tell from the
message whether "Run once anyway" would help.

A resync checks access too, and when it fails rclone adds "Must run
--resync to recover", which a resync cannot fix. The worker therefore
classifies a failed access test as `SIDE_UNAVAILABLE` before it looks for
"must run --resync"; a pair without a baseline stays without one, so its
next run resyncs again once the files are there.

### Safety

| Risk | What stops it |
|---|---|
| An unmounted drive or a wrong path makes everything look deleted | rclone's empty-side check and optional `check_access`: `SIDE_UNAVAILABLE`, fixed at the location; `max_delete_percent` (default 50) and the "every file changed" check: `SAFETY_ABORT`, overridden only by a deliberate `force` run |
| Old state compared against a different place | a new state key, so a resync (a union) |
| Filters changed, so newly excluded files look deleted | rclone's MD5 of the filters file: `RESYNC_REQUIRED` and a deliberate resync |
| Two runs on one location at once | concurrency keys per location |
| A run killed half-way | Syncr clears the stale rclone lock under its own lock; `--recover` |
| A file changed on both sides | both versions kept by default (`conflict_resolve` none, `conflict_loser` num); rclone's "delete the loser" is not offered |
| A resync overwriting a version | `resync_mode` newer by default; the resync dialog says so. Version history is restic's job; the docs recommend a backup next to a two-way sync |

### Concurrency keys per location

A two-way sync job has one key per side: `bisync:` and the first 40 hex
digits of a SHA-256 over (`remote`, remote ID, path) or (`local`, path,
lower-cased when it looks like a Windows path), 47 characters within
ADR 0013's pattern. The desktop's and the laptop's pairs share the key of
their R2 location, so their runs never overlap and run oldest first; a
scheduled run and a manual resync of one pair wait for each other too.
A local key ignores the worker, which the kind does not know: the same
path on two machines waits in line, which is harmless. The kind does not
know a remote's type either, so a remote of the local backend and a direct
worker path to the same folder get different keys (their state key is the
same); like nested locations, they are not serialized.

### Syncr owns the work directory

Only Syncr uses a pair's work directory, and only the holder of
`syncr.lock` runs rclone in it. A run takes the lock without waiting
(`filelock.TryLock`); if it is held, the job fails with `TEMPORARY`
(concurrency keys make that unreachable in normal operation). Holding it,
the worker deletes `work/*.lck` left by a killed run instead of waiting
for rclone's `--max-lock` to expire. It always runs rclone with
`--recover --resilient --color=NEVER`, JSON logs and stats, and the
locations after `--`, through the same location checks as transfers
(`local_roots`, aliases). It also always passes `--conflict-resolve`,
`--conflict-loser` and `--max-delete` (none, num and 50 when unset), so no
safety default is rclone's, and refuses equal or nested sides itself.

### A limit on path length

The two rclone paths (`r1:<root/path>`, or the cleaned local path) may be
at most 200 bytes together, which leaves room for the `..` joiner and
suffixes such as `.path1.lst-new` under 255 bytes. The kind checks the
definition's paths when a job or schedule is created, the full paths with
the remotes' roots when it builds the spec, and the worker checks the
final strings again (`INVALID_SPEC`), so that an older controller cannot
reach rclone's misleading error.

### Outcome

The worker classifies a failed run by rclone's messages, then by its exit
code: a failed access check is `SIDE_UNAVAILABLE`; "must run --resync"
anywhere, missing prior listings and empty prior listings are
`RESYNC_REQUIRED`; an empty side is `SIDE_UNAVAILABLE`; a missing side
while listing is `NOT_FOUND`; the delete limit and "all files were
changed" are `SAFETY_ABORT`, with rclone's counts in the message; anything
else is mapped as for transfers. The messages come from the pinned rclone
and are tested against its recorded output, so a tool upgrade that changes
them fails the tests instead of mislabelling jobs. A successful run
reports `RcloneBisyncOutput`: whether it was a resync and why, and the
conflicts (a count and the first 20 paths), also as a warning. A conflict
counts only when rclone renamed a version or picked a winner.

### Retention

At startup and then daily, the worker removes the state of pairs whose
`last-run` is older than 180 days and whose lock it can take. The age is a
constant, not a setting. A pruned pair resyncs on its next run.

## Consequences

- One folder on several machines stays in step through a bucket: new
  files, changes and deletions spread both ways, and a file edited on both
  sides keeps both versions.
- A pair belongs to a worker. Schedules cannot use labels for it, and
  moving a pair to another worker costs a resync there, which keeps the
  newer version of each differing file.
- A resync can overwrite the older version of a file that differs. It runs
  on its own only for a fresh pair, where there is nothing to compare
  against anyway.
- Changing a two-way sync's filters stops it until someone resyncs, and so
  does one file that cannot be read: a critical error stops the whole pair.
- Rotating an S3 or R2 token (a new access key ID), or changing any other
  non-secret option of a remote, makes the next run of its pairs a resync.
- Small folders can hit "every file changed": a folder of one file edited
  on both sides between runs stops with `SAFETY_ABORT` and needs a
  `force` run.
- State takes disk space on the worker: about 100 to 150 bytes per file
  per listing, with up to six listing files per pair, until 180 days after
  its last run.
- Pairs with long paths are refused with a message that says why.
- Nested locations on one remote (`R2:shared` and `R2:shared/sub`) and
  transfers into a two-way location are not serialized. The docs say not
  to do either.
- Change detection and `newer` rely on modification times and clocks.
  On backends without modification times bisync falls back to sizes, so
  an edit that keeps the size goes unnoticed (checksums help only where
  both sides share a hash); case-insensitive file systems against
  case-sensitive buckets can collide. Both are documented, not solved.
- bisync is an "advanced" command upstream. The pinned version and the
  recorded-output tests are what make a tool upgrade safe.
- A cancel, a time limit or a worker stop gives rclone the supervisor's
  10 seconds; a transfer that needs longer is killed, and the next run
  finishes it with `--recover` (one more run may be needed to remove a
  `.partial` file). On Windows every cancel is a kill. Rarely, a cancel
  just as a transfer ends leaves a pair that needs a resync (an rclone
  race).
- Interrupt delivery and long work-directory paths on Windows can only be
  verified in CI.

## Alternatives considered

- **A fourth transfer mode.** bisync has state, a first-run resync,
  conflicts, its own safety checks and its own errors, and half of the
  transfer options do not apply to it. A mode would put bisync checks all
  through the transfer kind, its runner, the editor and the descriptions.
- **State on the controller,** sent with each spec and returned with each
  result, so that any worker of a label could run a pair. Listings are
  rewritten every run and run to tens of megabytes for big trees, which
  the link and the job store are not built for. Left as a follow-up.
- **Labels with a warning** that each worker keeps its own state. A
  schedule whose runs move between workers would resync again and again,
  and two workers could run one pair from different states.
- **Resync automatically after critical errors, or when the filters
  change.** It would hide a real problem, such as a wrong path or newly
  excluded files, behind a resync that can overwrite versions.
- **rclone's default work directory.** It is inside the per-job scratch
  directory, which is deleted after every job.
- **`--max-lock` instead of clearing stale locks.** It waits for a lock
  that Syncr knows to be stale, and it is not needed when Syncr's own lock
  says who runs.
- **An alias remote to shorten the state file names.** rclone resolves the
  alias and names the files after the underlying path.
- **Offering rclone's "delete the conflict loser".** It turns every
  conflict into silent data loss on one side.
