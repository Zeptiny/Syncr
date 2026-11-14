# ADR 0020: Backing up a remote

- Status: accepted (v2.2.0)
- Date: 2026-11-14

## Context

A restic backup read only folders of a worker. People keep much of what
they want backed up on remotes Syncr already knows: photos on Nextcloud,
documents in a bucket, a share on an SFTP server. Copying such a folder to
a worker with a copy job, then backing that copy up with a second job, works
but leaves the user to keep two jobs, a folder and its disk in step.

What Syncr already had, and what shaped the design:

- **restic reads local files.** Its `-r rclone:` backend reaches a
  repository through `rclone serve restic --stdio` (ADR 0011): that is the
  storage side. Nothing in rclone gives restic a folder of a remote to
  *read*, except a local copy or a mount.
- **Remotes reach rclone through environment variables** (ADR 0007):
  `RCLONE_CONFIG_<ALIAS>_*`, aliases assigned by the job kind's Build. A
  spec can carry the repository's remote and a source remote side by side.
- **Per-worker state has a precedent** (ADR 0018): two-way sync keeps
  listings in `<data_dir>/bisync/<key>/`, under a key the controller
  computes from the remotes' non-secret options, pruned when unused, and
  its schedules must target a worker.
- **Worker abilities are per build** (`Hello.features`), but FUSE depends
  on the machine: the kernel, the package, the service's sandbox, the
  container.
- **Every signed-in user may browse every worker's `local_roots`** and run
  jobs on any worker (ADR 0012). A copy of one user's remote on a worker's
  disk must not become readable to the others.

What the pinned tools do, checked against rclone 1.75.2 and restic 0.19.1:

- `cd <dir> && restic backup .` makes a snapshot whose tree starts at the
  folder's contents, but whose `paths` is the absolute `<dir>`. restic
  finds a parent snapshot by host and paths; with `--ignore-inode` (which
  also ignores ctime) and the same modification times, a re-downloaded
  copy reads as unmodified.
- restic refuses to save an empty snapshot ("snapshot is empty").
- `rclone sync` and `rclone mount` both keep the remote's modification
  times. `rclone sync --delete-excluded` removes what new filters leave out;
  after any error rclone deletes nothing. Filters apply to a mount as well.
- `rclone lsf --max-depth 1` of a missing folder exits 3 ("directory not
  found"); on S3 only a missing bucket does, a missing prefix lists empty.
- `rclone mount` without `/dev/fuse` fails at once with "fuse device not
  found"; on Windows a mount point must not exist yet and needs WinFsp.
  The process stops and unmounts on SIGINT or SIGTERM.

## Decision

### A source on the Backup kind, read through a mirror or a mount

`ResticBackupDefinition.remote_source` holds one folder (`remote_id`,
`path`) of one of the owner's remotes, instead of `paths` (exactly one of
the two), and a method:

- **Mirror** (the default): `rclone sync` into
  `<data_dir>/sources/mirror/<key>/data`, then back that copy up. The copy
  is kept between runs (only changes are downloaded), pruned after
  `remote_sources.mirror_retention` (30 days) unused, or deleted after each
  run if the definition asks (`discard_mirror`). A failed sync makes no
  snapshot and keeps the partial mirror. Works on every worker; costs disk.
- **Mount**: `rclone mount --read-only --vfs-cache-mode off` at
  `<data_dir>/sources/mount/<key>` for the length of the job, then back the
  mounted folder up. Long attribute and directory cache times let restic's
  scanner and archiver share one listing (safe: read-only, one job).
  Costs no disk; needs FUSE 3 or WinFsp.

The worker runs the steps in order: a pre-check that lists the folder
(missing: `NOT_FOUND`; empty: see below), the mirror or mount, restic,
clean-up. Progress carries a phase (`mirror`, then `backup`), and the
counters restart at the change. Skipping uses rclone filter rules in both
methods, so a mirror never downloads what is skipped; restic's excludes,
`one_file_system` and `use_fs_snapshot` are refused there. Dry run brings
the mirror up to date for real (it is the worker's cache) and runs restic
with `--dry-run`.

### Snapshot layout and tag

The worker backs up `.` from the mirror or mount folder, so the snapshot
holds the remote folder's contents at its root, and always passes
`--ignore-inode` (mounts have no stable inode numbers; a discarded mirror
gets new ones). The controller adds the tag `syncr-remote:<remote id>`
(counted toward the 32-tag limit, so 31 are the user's). The tag names the
remote in the UI ("From *Nextcloud*"), lets retention select these
snapshots with forget's tag filter, and keeps "Restore to original
location" away from them: their paths are a folder of the worker's data
directory, not the remote. Restoring goes to a worker folder; a Copy job
puts it back on a remote.

### One worker

restic's parent and retention group by host and paths, and the path is the
worker's mirror or mount folder. A source therefore stays on one worker:
`ResticBackup` implements `DefinitionSingleWorker` for definitions with a
remote source, and schedules of it must target a worker. A run on another
worker (by hand) is allowed; it reads everything once and starts a
retention group of its own. The key under `sources/` is a hash of the
source remote's non-secret options and the path, computed by the
controller like the bisync state key: a changed secret keeps the mirror, a
changed bucket or endpoint starts a new one. A concurrency key
`remote-source:` over the remote and path keeps two runs of one source
apart, next to the repository's key.

### Empty folders fail

A source whose folder is empty fails with the new code `SOURCE_EMPTY`
(32) before the sync, so an emptied or mis-pointed folder neither wipes
the mirror nor makes empty snapshots that push real ones out of retention.
`allow_empty` lets it pass; since restic makes no empty snapshot, such a
run succeeds **without a snapshot**, with a warning, and the controller's
summary and the UI say so. A folder whose files the filters all leave out
fails with `EXECUTION_FAILED`.

### Detection, reported in Hello

Mirror is available unless `remote_sources.mirror: false`. For the mount,
the worker checks at startup: on Linux, `/dev/fuse` opens read-write,
`fusermount3` is on the PATH and, when not root, is setuid root in a
process with `NoNewPrivs: 0`; on Windows, WinFsp's registry entry and DLL.
With `remote_sources.mount: auto` it then mounts an empty folder
read-only, which catches containers without `SYS_ADMIN` and AppArmor's
default profile. The result goes in `Hello.remote_sources` with one-line
reasons ("/dev/fuse is missing"); the feature `restic.remote_source` says
the build can run the kind at all. The editor, `RunJob` and the dispatcher
use both: a worker that cannot use the method is refused upfront, or the
job fails with `TOOL_UNAVAILABLE` and the reason; an older worker fails
with `WORKER_OUTDATED`.

### Mounts never outlive their job

The mount is stopped on every way a job ends (SIGINT through the
supervisor, a kill after 10 s, then `fusermount3 -u -z` if the table still
lists it). At startup, before anything else under the data directory is
touched, the worker unmounts whatever a killed process left under
`sources/mount`, reading only `/proc/self/mountinfo`: `stat` on a dead FUSE
mount can hang, so no walk ever enters `sources/`.

### `<data_dir>/sources` is refused everywhere

Mirrors hold one user's remote data, and every user may browse worker
folders and run jobs anywhere. So every job path, probe path and browse
path that is or lies inside `<data_dir>/sources` fails with
`PATH_NOT_ALLOWED`, even when `local_roots` covers the data directory.
Backups and copies of a folder that holds it (such as `/`) get
`--exclude-if-present=.syncr-sources`, with that marker kept in the
folder, so they never read another user's mirror or walk into a live
mount; folder sizes count it as empty.

## Consequences

- Backing up a remote folder is one job, scheduled like any backup, with
  snapshots named after their remote.
- Mirrors cost disk on the worker, as large as the folder after filters;
  the docs point to the mount, a filter or `discard_mirror`. The first run
  of either method reads everything (egress); later runs read what changed.
- A remote without stable modification times makes every file look
  changed through a mount; the mirror keeps its own times.
- The mount needs setup outside Syncr: `fuse3` (shipped in the worker
  image), a `NoNewPrivileges=no` drop-in for the systemd user service,
  `/dev/fuse`, `SYS_ADMIN` and an unconfined AppArmor profile in Docker,
  `/dev/fuse` in LXC, WinFsp on Windows. The trial mount turns a missing
  piece into a reason on the Workers page instead of failed jobs.
- restic's CLI shows the internal mirror or mount folder as a snapshot's
  path; the UI hides it.
- Moving a source to another worker or method costs one full read and a new
  retention group.
- Overlaps between the source and the repository are refused when both are
  on the same remote; overlaps hidden behind unions, aliases or two remotes
  of the same storage are not detected.
- CI installs `fuse3` and WinFsp and requires the real-mount tests to run
  (`SYNCR_REQUIRE_MOUNT=1`).
- Out for now: restoring straight back to a remote, mount schedules over
  labels (they need a retention `group_by`), several sources in one backup,
  and a per-worker list of mirrors.

## Alternatives considered

- **`rclone serve restic`:** it serves a *repository* over restic's REST
  protocol; Syncr already uses it (`--stdio`) as the storage side. It
  cannot present a folder for restic to read.
- **Streaming into `restic backup --stdin`:** one file per snapshot, no
  tree to browse, no per-file deduplication against the previous run, and
  everything is read every time.
- **`rclone serve nfs` plus a kernel NFS mount:** it needs root to mount,
  NFS client tools and a port on the worker, and is no simpler than FUSE
  where FUSE is missing (containers). The mirror covers those machines.
- **Any worker (label schedules):** with restic's default grouping each
  worker's snapshots would be a group of their own, retention would keep N
  per worker, and every switch would read everything again (and download
  it again with a mirror). Allowing labels needs a retention `group_by`
  such as `host,tags` or `tags`, left as a follow-up.
- **A copy job plus a backup of the copy:** what users did before; two
  jobs to keep in step, a folder any user could browse, and no tie between
  the snapshot and the remote.
- **Excludes instead of filters:** restic excludes would still download
  the excluded files into the mirror; rclone filters skip them in both
  methods.
- **Empty snapshots with `allow_empty`:** restic 0.19.1 refuses them; a
  successful run without a snapshot says the same honestly.
