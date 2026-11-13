# Restic backups

Syncr runs [restic](https://restic.net/) on your workers: you keep
**repositories** in Syncr, back folders up into them by hand or on a
schedule, apply retention, check them, browse their snapshots and restore
from them. The worker runs the pinned restic (0.19.1) and reaches the
storage through rclone, with the remotes you already store in Syncr.

Design records: [repositories](adr/0011-restic-repositories.md),
[worker probes](adr/0012-worker-probes.md),
[concurrency keys](adr/0013-concurrency-keys.md),
[browser operations](adr/0019-browser-operations.md).

## Repositories

A repository is a path on one of your remotes plus a password. Add one
under **Repositories, Add repository**:

- **Create a new one** runs `restic init` at that path. The password needs
  at least 8 characters.
- **Connect to an existing one** reads the repository that is already
  there. It needs that repository's password.

Either way a worker does it before anything is stored, so a repository in
the list is one Syncr has seen. What the worker finds is shown next to the
field it concerns: a wrong password, no repository at the path, or a
repository that already exists where you asked for a new one.

- **The password is write-only.** It is encrypted at rest with
  `secrets.key`, never shown again, and sent only to the worker that runs a
  job or lists snapshots. **Keep it somewhere safe yourself:** without it
  the backups cannot be read, by Syncr or anyone else.
- **Repositories belong to their owner,** like remotes: other users and
  administrators do not see them.
- **The location does not change.** To move a repository, connect it again
  at its new place. The name, the description and the stored password can
  be changed; a new password must open the repository.
- A repository on a **local folder of a worker** exists on that worker
  only: choose the worker when you create it, and run its jobs there.
- Any remote type can hold a repository except a union.
- A remote that holds a repository cannot be deleted. Delete or forget the
  repository first.
- Repositories on one remote cannot be nested: a new repository's folder
  may not lie inside another repository's folder, or hold one. A
  repository at the remote's root is the only one on that remote.

## Jobs

Under **Jobs, Run a job** (or a repository's buttons), and under
**Schedules**:

| Job | What it does |
|---|---|
| **Backup** | Saves folders of the worker (inside its `local_roots`) as a snapshot. Options: exclude patterns, tags, a host name, skip cache folders, stay on one file system, skip files above a size, dry run. |
| **Retention** | `restic forget` with a policy: keep the last N, or N hourly, daily, weekly, monthly, yearly snapshots, snapshots with certain tags, or everything within a period such as `1y6m`. With **prune**, the data no snapshot needs any more is removed too. |
| **Prune** | Removes unneeded data without changing which snapshots exist. |
| **Check** | Verifies the repository's structure, and reads back a share of the data (0 to 100 %). |
| **Restore** | Writes a snapshot, one of its folders or selected items into a folder of the worker. Started by hand only. |

**Jobs on one repository run one at a time,** oldest first, whichever
worker they are on. A job that waits shows *Waiting for repository* and
links to the job ahead of it. A job stuck on an offline worker holds up the
ones behind it until it times out (15 minutes) or you cancel it; a job that
hangs while running holds the repository until you cancel it.

A backup's job page shows the snapshot it wrote, how many files were new,
changed and unmodified, and how much data was added. Notifications carry
the same details.

### Databases and running applications: hooks

restic reads files as they are. A database that writes while it is backed
up, or an application whose files belong together, needs something to run
first: a dump, or a stop. Every job above, restores included, can run
**hooks** of its worker before and after its operation, for example
`dump-db` before a backup and `start-app` at its end. The worker's
operator defines them in the worker's configuration; the job's form offers
them by name. If a hook before the backup fails, no snapshot is written and
the job fails. See [hooks](hooks.md).

### Windows: volume snapshots (VSS)

**Use a volume snapshot** backs up open and locked files (`restic
--use-fs-snapshot`). It needs the worker to run as a system service: a
worker installed in user mode (a scheduled task) cannot create volume
snapshots, and the job fails with restic's error. The option is refused for
workers that are not on Windows: when you choose the worker, when a
schedule on a label picks one of the label's workers (only Windows workers
qualify), and when the job is sent to its worker.

A Windows snapshot records `C:\Users\ada\Documents` as the tree path
`/C/Users/ada/Documents`; the snapshot browser shows the tree form.

## Snapshots and restore

A repository's page lists its snapshots. Listing needs a worker and takes a
moment, so Syncr keeps the last list: it is marked out of date after a
backup, retention, prune or data deletion job on the repository ran,
including one that failed or was cancelled part-way (it may have changed
the snapshots), and **Refresh** lists again. The newest 5000 snapshots are
kept.

The list can be narrowed:

- **Filter** by ID, host, tag or path, and pick a **Host** or a **Tag**.
- **Group by host and paths** shows one group per backup set, as
  `restic snapshots --group-by host,paths` does.
- **Compare with the previous snapshot** (the icon on each row) compares a
  snapshot with the one before it in the same group ([below](#compare-two-snapshots)).
- **Search** finds files in every snapshot ([below](#search-and-file-versions)).

### Browsing a snapshot

Open a snapshot to browse its folders, with the same table, keyboard,
selection and tray as the [file browser](browsing.md). In addition:

- **The snapshot switcher** in the header steps to the **Older** or
  **Newer** snapshot of the same host and paths, or picks any snapshot
  from its menu, and keeps the open folder. A folder the other snapshot
  does not have says so and offers its nearest parent.
- **Listed by** chooses the worker that reads the repository (a repository
  on a worker's disk needs its own worker).
- **Calculate sizes** measures the open folder of the snapshot
  ([folder sizes](browsing.md#folder-sizes)).
- **Compare with…** compares this snapshot with another, for the open
  folder.
- Each row's menu has **Restore…** (that item), **Restore to original
  location…**, **Show versions** (files and links only) and **Copy path**.

### Restore

From the snapshot browser:

- **Restore whole snapshot** writes everything below the target folder,
  with the original paths (`/srv/photos/a.jpg` restored to `/srv/restore`
  becomes `/srv/restore/srv/photos/a.jpg`).
- **Restore this folder** writes the folder's content directly into the
  target.
- **Restore selected** writes the selected files and folders, with their
  original paths below the target.
- **Restore to original location** (a row's menu, or the tray) writes the
  selected items back where they were.

The target is a folder of the worker, inside its `local_roots`. The sheet
proposes the worker and target you used last for this repository (kept in
your browser), as long as that worker is online.

**Existing files are kept by default.** Choose what happens when a file
already exists:

| Choice | Effect |
|---|---|
| Keep existing files (default) | Never replaces a file. |
| Replace changed files | Replaces a file whose content differs. |
| Replace older files | Replaces a file that is older than the one in the snapshot. |
| Replace all | Always writes the snapshot's file. |

**Restore to original location** proposes the worker whose host name is
the snapshot's host and, as the target, the items' common folder as it is
on that machine: for `/srv/photos/2026` and `/srv/photos/a.jpg` it is
`/srv/photos`, and a Windows snapshot's `/C/Users/ada/Documents` becomes
`C:\Users\ada\Documents`. Existing files are kept unless you choose
otherwise. With no such worker online you choose one; a target outside the
chosen worker's `local_roots` is refused. The job restores that folder of
the snapshot (`restic restore <snapshot>:/srv/photos`) with only the
selected items (`--include`), so they land at their own paths and nothing
else is written. This needs a worker of 2.1 or later (the
`restic.restore_subfolder_paths` feature): the restore sheet proposes only
such a worker and shows older ones as outdated, and a job sent to an older
worker through the API fails with `worker_outdated`.

### Search and file versions

**Search** (on the repository's page or in the snapshot browser) runs
`restic find` over **all snapshots** (the default) or one, through a
worker:

- **Ignore case** is on by default.
- A pattern without `/` matches a file or folder of that name anywhere in
  the path: `*.pdf`, `albums.json`.
- A pattern with `/` matches the end of the path (`2026/albums.json`
  matches `/srv/photos/2026/albums.json`), or from the top when it starts
  with `/` (`/srv/photos/*.jpg`).
- `*` matches any part of a name, `?` one character and `**` any number of
  folders. To match `*`, `?` or `[` themselves, write `[*]`, `[?]` or
  `[[]`.
- A matching folder also brings everything inside it: `docs` lists the
  folder and its whole content.
- A pattern is at most 256 bytes. (The API takes up to ten at once.)

Results are grouped by path, newest first: how many snapshots hold it,
and its size and time in the newest. A path opens into its **versions**:
the distinct copies (by size and modification time), each with the
snapshots that hold it, a link to its folder in one of them, and
**Restore…**.

**Show versions** in the snapshot browser's row menu shows the same for
one file, in a side sheet: every snapshot that has that exact path,
grouped by version. Folders have no versions: compare snapshots instead.

The search is in the page's address, so it can be bookmarked or shared.
Opening such an address, or reloading the page, fills in the form and
waits for **Search**: a search reads every snapshot, so a link never
starts one on a worker by itself. From the app (the form, the snapshot
browser) it runs at once.

A search stops at 1,000 matches (the result says so, and how many
snapshots it searched), and like folder sizes it stops at
`probe.scan_timeout` with what it found so far. Narrow the pattern, or
search one snapshot, to see more.

### Compare two snapshots

**Compare** runs `restic diff` between two snapshots, for the whole tree
or one folder. The page shows the totals first (files added, removed and
changed, bytes added and removed), then the changes, which you can filter
by kind (**Added**, **Removed**, **Changed**, **Type changed**) and by
text. Each path opens its folder in the snapshot that has it.

- Added and removed folders are listed (with a trailing `/`), and so is
  everything in them. A folder whose content changed is not listed
  itself; the files that changed are.
- A file whose content is unchanged is not listed, even when only its
  modification time or permissions changed: Syncr runs `restic diff`
  without `--metadata`, which would mark every folder above a change.
- Type changes are, for example, a link that became a file.
- At most 2,000 changes are listed; the totals still cover all of them.
  A comparison that reaches `probe.scan_timeout` stops with the changes
  found so far and no totals, and says so.
- Like a search, a comparison opened from an address (a shared link, a
  reload) waits for **Compare**; from the snapshot list or **Compare
  with…** in the snapshot browser it runs at once.

## Browsing and testing

- **Browse** opens the folders of a worker (inside its `local_roots`) or
  of a remote as a page, where you can select, copy, move, measure and
  start jobs ([browsing files](browsing.md)). Any signed-in user may
  browse any online worker's allowed folders. In forms, **Browse** opens
  the same folders in a dialog to pick a path.
- **Test remote** on a remote's form lists the remote through a worker and
  reports what it found: the first entries, the space used where the
  backend can tell, or the error. It also tests values you have not saved
  yet.

These are *probes*: short, read-only operations a worker answers within the
request. They are limited to 20 at once per user and then two a second, and
to `probe.timeout` (30 s) for listings and tests, `probe.restic_timeout`
(60 s) for restic's listings, and `probe.scan_timeout` (45 s) for folder
sizes, searches and comparisons, which end early with what they found.
Raise `probe.restic_timeout` (at most 90 s) if listing the snapshots of a
large repository on a slow remote times out. A remote on a worker's local
folder can only be browsed through that worker.

Probes and the restic jobs need a current worker. A worker that is too old
shows as **Outdated** on its page; jobs sent to it fail with
`worker_outdated`. Upgrade it with the command on its page
(`syncr-worker upgrade`, see [upgrading](upgrade.md)).

## Deleting a repository

**Delete** on a repository offers two things.

**Forget only** (the default) removes the repository from Syncr and wipes
its stored password. Nothing in the backend is touched: the backups stay
where they are, and the repository can be connected again with its
password. Its schedules stay: each pauses itself at its next run, saying
that the repository is gone, until you delete or change it.

**Also delete the backup data** removes the repository's files from the
backend. It cannot be undone. You must tick the option and type the
repository's name. Then:

1. The repository is marked *Deleting data*. Its schedules are paused, and
   new jobs, schedule changes and refreshes on it are refused.
2. A **Delete data** job is queued on a worker. It waits for the jobs
   already queued or running on the repository.
3. The job opens the repository with its password and checks that it is
   the repository Syncr knows (restic's repository ID) and that no other
   restic client holds a lock. If any of that fails, nothing is deleted.
4. It deletes only restic's own files: the files named like restic's in
   `locks`, `snapshots`, `index`, `data` and `keys`, and `config` last,
   and removes those folders once they are empty. Other files in the same
   folder, or inside restic's folders, are kept; the job names the ones
   inside restic's folders in a warning. The folder itself is removed only
   if it is then empty and is not the remote's root.
5. When the job succeeds, the repository disappears from Syncr and its
   password is wiped.

You can cancel the job from the repository's page while it waits or runs.

**If the job fails or is cancelled,** the repository becomes active again
and says why; its schedules stay paused. Some files may already be gone,
so the backups may no longer be readable. **Try again** finishes the
deletion (the key and the config are deleted last, so the repository can
still be verified), and **Forget only** removes it from Syncr as it is.

**If the worker finds no repository at the location,** nothing is
deleted and the deletion fails: "no restic repository was found at the
location on this worker". The data is elsewhere: for a folder of a
worker, choose the worker whose disk holds it; for a remote, check the
remote's settings (endpoint, bucket). Or forget the repository without
deleting its data. Finding nothing counts as done only when an earlier
attempt already deleted files on the same worker (then that attempt
stopped after deleting the rest). Data deletion is refused while another
repository lies inside the repository's folder.

Things to know about backends:

- A **versioned bucket** (S3 with versioning, B2 with a lifecycle that
  keeps versions) keeps older versions of the deleted files until its own
  rules remove them. Syncr asks Backblaze B2 for a hard delete and Google
  Drive to skip the trash.
- Some **WebDAV** servers move deleted files to a trash.
- Deleting a large repository can take hours. The job shows its progress
  and can be cancelled.

## Limits and known gaps

- The same physical repository registered by two users, through two
  remotes, is two repositories to Syncr: their jobs are not serialized
  against each other, and restic's own locks decide.
- Listing can fail while a prune runs on the repository ("the repository
  changed during the listing"). Try again.
- On S3 and compatibles a folder that does not exist lists as empty, so a
  mistyped path is reported as "no restic repository at this location"
  rather than as a missing folder.
- With more than one controller, a probe answered to another controller
  than the one that sent it times out for the caller.
- The dashboard's "bytes" total counts what backups read, not what they
  added, so nightly backups dominate it.
