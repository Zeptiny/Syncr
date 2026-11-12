# ADR 0011: Restic repositories

- Status: accepted (unreleased until v2.0.0)
- Date: 2026-11-10

## Context

The workers have run restic since Milestone 1, but only from a complete
spec with inline credentials. Milestone 5 makes restic a product feature:
people keep **repositories** in Syncr, back up into them on a schedule,
apply retention, check them, browse their snapshots and restore from them.

A repository is a place (a path on one of the user's remotes) plus a
secret (its password). The password is as sensitive as a remote's
credentials: with it and the remote, anyone can read every backup.

Constraints:

- Remotes are private to their owner, admins included (ADR 0007). A
  repository sits on a remote, so it cannot be less private.
- A job definition is stored and shown; it must stay free of secrets. The
  worker's spec is built at dispatch and never stored (ADR 0007).
- Only a worker can reach a remote. The controller cannot tell by itself
  whether a location holds a repository, or whether a password opens it.
- restic has no "delete repository" command, a repository may live at the
  root of a remote, and other files may live next to it.
- Use cases cannot import each other or the job kinds (ADR 0001).

## Decision

### A repository is a stored object with a write-only password

`internal/app/restic` owns repositories: owner, name, description, remote,
path, the encrypted password, and restic's own repository ID and format
version.

- **Owner-only, admins included.** Every use case checks the owner in the
  service and answers `ErrNotFound` for anyone else, as remotes do.
- **The password is write-only.** It is encrypted with the secrets cipher,
  bound to the repository by associated data (`restic-password:<id>`), and
  leaves the service only inside `Resolved` and probe `Target` values, for
  a job or probe spec. No API message has a field for it and nothing logs
  it.
- **The location never changes.** A new location is a new repository. The
  name, the description and the stored password can change.
- **Three things are unique among a user's live repositories:** the name
  (case-insensitive), the location (remote and normalized path), and
  restic's repository ID, so the same repository cannot be registered
  twice through two remotes. The store enforces them with partial unique
  indexes; the service checks the name and location first, so that a
  doomed request never initializes a repository.
- **Repositories on one remote are not nested.** A new location may not
  lie inside the folder of another live repository on the same remote, or
  hold one inside its own (folders compare whole: `data2` is not inside
  `data`; the remote's root holds everything). Otherwise deleting the
  outer repository's data would reach into the inner one's folder.

### Nothing is stored before a worker has seen the repository

Creating a repository is `init` (create one; fail if one exists) or
`connect` (use an existing one). Both run on a worker as a probe (ADR
0012) **before** the row is stored, and the stored restic ID and format
version come from the worker. A new password is verified the same way and
must open the repository with the stored restic ID.

What the worker reports is mapped to the form field it concerns: a wrong
password to `password`, no repository to `path`, an existing repository to
`mode`, a location outside the worker's folders to `worker_id`. Anything
else is a failed precondition with the worker's scrubbed message.

A repository on a remote that is a worker's local filesystem exists on one
worker only, so the caller must name the worker.

### Restic work is jobs; looking is probes

Backup, retention (`forget`), prune, check and restore are job kinds
(`internal/jobkind`). A definition carries the repository's ID and the
operation's options; the dispatcher resolves the repository and its remote
for the job's owner and the kind builds the spec.

- Every restic kind holds the repository's concurrency key (ADR 0013), so
  the jobs of one repository run one at a time, oldest first, and requires
  the worker feature `restic.v2`: an older worker would ignore the new
  spec fields.
- **A restore never replaces files unless asked to.** An unspecified
  overwrite behavior is sent to the worker as "never"; restic's own
  default is "always".
- Selected items of a restore are literal snapshot tree paths. The worker
  turns them into include patterns for its operating system, so the
  controller never builds glob patterns from file names.
- Restores are started by hand and cannot be scheduled.
- A kind describes its definition in one line (the job's title), labels
  itself ("Backup") and summarizes its output as display details. The
  jobs API and the notifications both show these, so a job reads the same
  in the UI and in a message.

Listing snapshots and browsing a snapshot's tree are probes: short,
read-only (`--no-lock`), answered within the request.

### The snapshot list is cached

Listing snapshots needs a worker and takes seconds. The list is cached per
repository (`restic_snapshots`, replaced as a whole) with the time it was
taken and the repository's **change mark** read before the worker was
asked. The mark counts the jobs on the repository that may have changed
its snapshots: backup, retention, prune and data deletion jobs that ended
after a worker had accepted them, **however they ended** (a failed forget
may have removed snapshots, a backup may have saved its snapshot before it
failed, a failed data deletion has usually removed `snapshots/`). Jobs that
failed or were cancelled while queued never ran and do not count.

The list is **stale** when it was never taken, or when the repository's
mark differs from the one stored with it; the UI refreshes a stale list
once. No clocks are compared: a job's finish time comes from its worker's
clock, which may be minutes off the controller's. A job that ends while
the worker lists leaves the list stale, because the mark was read before.
Jobs are never deleted, so the count only grows; the mark also holds the
newest finish time of those jobs, so that a future purge of old jobs
together with as many new ones still looks like a change. When a refresh
fails, the cached list is served with the reason.

### Deleting has two forms, and the destructive one is opt-in

**Forget only** is the default. It soft-deletes the row, wipes the password
and drops the snapshot cache. The data in the backend is not touched. Job
history keeps its reference to the row.

**Forget and delete the data** needs the opt-in and the repository's name
typed as confirmation; the service enforces both, not only the UI. It is a
job, not a probe: it can take long, changes the backend, and needs
progress, cancellation, a history record, and the repository's concurrency
key so that it runs after every older job.

```
active --delete with data--> deleting --job succeeded--> deleted
                                |
                                +-- job failed, cancelled, interrupted,
                                    or never created --> active (with the error)
```

- It is refused while another live repository lies inside the
  repository's folder (one stored before the nesting rule, for example):
  delete that one first, or forget this one.
- The request picks the worker, reserves the job's ID on the repository
  (`MarkDeleting`, which pauses the repository's schedules and cancels
  their open runs in the same transaction), then queues the job. A crash
  between the two leaves a deleting repository without a job; repeating
  the request queues it, and otherwise the deletion is given up after 15
  minutes.
- While deleting, the repository accepts nothing new: only the job with
  the reserved ID may use it. Jobs that were queued before still run
  first.
- **The row is deleted, and the password wiped, only when the job
  succeeded.** A background pass (`FinishDeletions`, every 30 seconds and
  when a deletion job ends) reads the outcome from the jobs table and ends
  the deletion with a compare-and-swap on the reserved job ID, so restarts
  and two controllers are harmless. After a failure the repository is
  active again, shows the error and keeps its password, so the user can
  try again or forget it. Its schedules stay paused.
- The job kind is system-only: `RunJob` and schedules reject it. Its
  definition holds the repository ID and whether it resumes an earlier
  attempt. The worker deletes restic's own files, never "the folder",
  after verifying that the location still holds the repository with the
  stored restic ID: only files named like restic's (64 hex digits in
  `keys/`, `locks/`, `snapshots/`, `index/`, and in `data/`'s sub-folders
  named by 2 hex digits), then `config`, and only folders it emptied.
  Anything else in those folders stays and is named in a warning.
- **Nothing found is not success on a first attempt.** A repository does
  not record its worker, and a remote's settings can change, so a
  deletion can look at a location that does not hold the data (the wrong
  worker's disk). The job counts a location without restic's files as
  already deleted only when an earlier deletion job of the repository
  **ran on the same worker and deleted files there**: its final stats
  count deletions (failed and cancelled runs report them too), or, for a
  job lost without a result, its last stored progress sample does. Then
  that attempt reached this location and may have been cancelled or lost
  after deleting the config. The decision is made when the job is created
  (the definition's `resume`). Anything else fails with "no restic
  repository was found at the location on this worker", deletes nothing,
  and the repository stays with its password: a first attempt, a retry
  after attempts that deleted nothing (one that found nothing, a wrong
  password, a mismatch, a lock, a cancel before any deletion), and a retry
  on another worker. A lost attempt whose progress was never stored does
  not count either; the repository can still be forgotten.

### A remote that holds a repository cannot be deleted

`DeleteRemote` refuses, naming the repository, in the same transaction that
deletes the remote. A repository is only inserted while its remote is live
and share-locked, so the two cannot race.

## Consequences

- A user who loses the password cannot be helped: Syncr can only replace
  the stored password with one that already opens the repository.
- Rotating the secrets key must re-encrypt repository passwords along with
  remote secrets. A password that cannot be decrypted fails the job at
  once (failed precondition) instead of retrying.
- Creating a repository needs an online worker that can reach the remote.
- The snapshot cache can be out of date when something outside Syncr
  changes the repository; the "Refresh" action lists again.
- A deleting repository whose job never finishes (a hung worker) stays
  deleting until someone cancels the job.
- Admins cannot see or repair another user's repositories. They can see
  and cancel its jobs, which carry the repository's name.

## Alternatives considered

- **Passwords in job definitions.** Rejected: definitions are stored,
  listed and sent in notifications.
- **Storing the repository first and verifying later.** Rejected: a typo
  in the path or the password would surface as a failed backup hours
  later, and `init` could create repositories nobody registered.
- **Deleting data with `rclone purge` of the path, or `rclone delete
  --rmdirs` of restic's folders.** Rejected: the path may be the root of a
  bucket or hold unrelated files, and restic's folders may too (another
  repository stored inside `data/`).
- **Deleting the row at once and letting a job clean up.** Rejected: after
  a failed clean-up nothing would remember the password needed to try
  again, and nothing would show the user that data remains.
- **Listing snapshots on every page view.** Rejected: it needs a worker
  and seconds each time, and fails whenever the worker is offline.
