# Browsing files

Syncr can show you the folders of a worker, of a remote and of a restic
snapshot, and act on what you select there: copy or move it, back it up,
start a job or a schedule from it, restore it, and see how big each folder
is. Snapshots have search, file versions and comparisons of their own,
described in [restic backups](restic.md#browsing-a-snapshot).

Nothing here writes outside a job. Listing and measuring are *probes*
([ADR 0012](adr/0012-worker-probes.md)), short read-only operations a
worker answers while you wait; copying, moving and restoring are ordinary
jobs that show in the job list. Design record:
[browser operations](adr/0019-browser-operations.md).

## The pages

| What | Where you open it | Address |
|---|---|---|
| A worker's folders | the worker's page, **Browse files** | `/workers/<id>/files?path=` |
| A remote | the Remotes list's **Browse**, or the remote's page | `/remotes/<id>/files?path=&worker=` |
| A snapshot | a repository's snapshot list | `/repositories/<id>/snapshots/<snapshot>?path=` |

The open folder is in the address, so Back, reload and shared links work.

- **A worker's folders** are those inside its `local_roots`. Any signed-in
  user may browse any online worker's allowed folders, as before.
- **A remote** is listed through a worker. **Listed by** shows which one
  and lets you choose another online worker; your choice goes into the
  address (`worker=`). A remote on a worker's local disk can only be listed
  by that worker, so its page asks you to choose it first. Remotes stay
  private to their owner.
- At the root of a remote the page shows its **usage** ("412 GiB used of
  1 TiB") when the backend can tell, as `rclone about` reports it. S3 and
  most compatibles cannot, and a remote on a worker's disk never shows
  one; then nothing is shown. The figure is kept for five minutes.
- **Refresh** lists the folder again. "Listed 2 min ago" says how old the
  listing is.
- A listing holds at most 5,000 entries; a larger folder says that only
  the first ones are shown.
- A folder that is not there any more (moved, deleted, or not in the
  snapshot you switched to) says so and offers **Go to the nearest
  parent**.

The path pickers of the job, repository, schedule and restore forms are
still dialogs, built from the same parts.

### Sorting

Click **Name**, **Size** or **Modified** to sort; folders stay first
whatever the sort, and names sort numbers by value (`photo2` before
`photo10`). The choice is remembered in your browser. Folders sort by size
once their size is measured ([below](#folder-sizes)).

## Selecting

Tick the box of a row to select it. The selection **stays while you move
between folders**, so you can collect items from several places, and is
cleared when you switch to another worker, remote, snapshot or listing
worker.

| Key | What it does |
|---|---|
| Up, Down, Home, End | move between rows |
| Enter | open a folder |
| Space | select or unselect the row |
| Shift+Space, Shift+click | select a range, from the last row you ticked |
| Ctrl+A (Cmd+A on a Mac) | select every row that matches the filter |
| Backspace, Alt+Up | go up one folder |
| Escape | clear the filter, then the selection |

Ranges follow the order you see, sorted and filtered. **Select all** (the
box in the header, or Ctrl+A) selects every row that matches the filter in
the open folder, not only the 200 rows on the page, and says so.

### The tray

The selected items are listed beside the table (below it on narrow
screens, and in a bar at the bottom of a phone screen), grouped by folder,
each with a button to remove it. The tray shows:

- **The total size** of what is selected: files' sizes plus the measured
  folders. While a selected folder is not measured, it says "at least".
- **Covered items**: when you select a folder and also something inside
  it, the inner item is marked "in <folder>" and the actions ignore it;
  the folder brings it anyway.
- **Copy paths** copies the selected paths, one per line (covered items
  left out), for a script or a ticket.
- The actions: **Copy or move…** on workers and remotes, restores in a
  snapshot.

Each row also has a menu (⋯) with the actions for that one item and **Copy
path**.

## Copy and move

Select files and folders on a worker or a remote, then **Copy or move…**
(in the tray, or a row's menu for one item). Choose where they go, what
happens to files already there, and copy or move.

**It works like a file manager:** a selected folder `photos` copied into
`backup` arrives as `backup/photos`; a selected file arrives as
`backup/<name>`. Items selected in different folders keep the folders
between them and their nearest common folder: `docs/a.txt` and
`photos/2026` copied into `backup` arrive as `backup/docs/a.txt` and
`backup/photos/2026`. To copy only a folder's contents, open it and select
its contents.

**What runs is a transfer job** (`rclone copy` or `rclone move`) from the
items' common folder into the destination, with filters that let through
exactly the selected items (`+ /photos/**`, `+ /notes.md`, then `- **`).
The job page shows the filters, like any transfer's. Names with pattern
characters (`*`, `?`, `[`, `{`, `\`) are escaped, so only the item you
selected matches.

**The destination** is a folder on one of your remotes or a folder of a
worker, typed or picked with **Browse**. Choosing the folder the items are
already in, or a folder inside a selected folder ("A folder can't be copied
into itself"), is refused.

**Which worker runs it:**

- If either side is a worker's folder (or a remote on a worker's disk),
  that worker runs the job: only it can read or write that disk.
- Between two remotes, any online worker can; the one that listed the
  source is proposed, and you can choose another.
- Between two different workers' folders it is refused: a job runs on one
  worker. Copy through a remote instead (one copy into a bucket, one out).

**If a file is already there:**

| Choice | Effect |
|---|---|
| Keep existing (default) | Files already at the destination are left alone; only missing files are copied (`--ignore-existing`). |
| Replace files that differ | A file is replaced when its size or modification time differs (rclone's default). |
| Replace older files | A file is replaced when the one being sent is newer (`--update`). |

**Preview** runs the same job as a dry run: nothing is changed, and its
page lists what would be copied, moved or skipped.

**Move** asks first ("2 items will be deleted from /srv/docs on nas-01
once copied"). A move deletes each file from the source once it is safely
at the destination, then removes the folders it moved and any folder it
emptied. The source folder itself, and folders you did not select that
still hold something or were empty before, stay. Folders, including empty
selected ones, are created at the destination where the backend has
folders (a bucket has none: an empty folder does not arrive there).

After you start it, a message links to the job. The page follows the job
and, when it ends, lists the source (for a move) and the destination again
and forgets their measured sizes. **Open in the job editor** opens the same
job in **Run a job** instead of starting it, to change an option or turn
it into something else.

Copying and moving from a worker's folders is open to every signed-in
user, like any transfer job on any worker; remotes stay their owner's.

## Starting a job from a selection

**New job ▾** in the toolbar opens an editor with the fields filled in,
from the selection, or from the open folder when nothing is selected.
Nothing runs or is saved until you press the editor's button.

| From | Choice | What is filled in |
|---|---|---|
| A worker's folders | **Back up with restic…** | the selected paths and the worker (choose the repository) |
| A worker or a remote | **Copy or sync…** | the open or selected folder as the source; with a selection, its filters |
| A worker or a remote | **Two-way sync…** | the selected folder (one folder) as Folder A |
| A worker or a remote | **Schedule: …** | the same, in a new schedule |
| A remote | **New restic repository here** | the remote and the open folder |

The draft travels with the page you open, not in its address: it survives
Back and reload in that tab, but a copied link opens an empty form.

## Folder sizes

**Calculate sizes** in the toolbar measures the open folder in one pass:
every file below it, summed per subfolder. The folder's total shows in
the toolbar ("6.94 GiB in 136 files"), and each subfolder's **Size** cell
shows its total, its number of files and a bar for its share. The tray's
total then counts the measured folders too.

It works the same in a worker's folder (the worker walks its disk), on a
remote (`rclone lsjson -R` through a worker) and in a snapshot
(`restic ls --recursive`).

- **Results are kept in your browser** until you press **Refresh** (which
  forgets the open folder's sizes but does not measure again) or a copy or
  move started from the page ends. Hovering a size shows how old it is.
  Nothing is stored on the controller.
- **Links are never followed** (symbolic links, Windows junctions): they
  count as entries of no size, so a link to a big disk does not make a
  folder look big, and a loop cannot trap the walk.
- **Unreadable entries** (a folder the worker may not open, a file deleted
  while it walks) are counted and skipped: the result says "1 entry could
  not be read", and the totals are then lower bounds.
- **A long walk stops with what it found.** The worker stops shortly
  before `probe.scan_timeout` (45 s by default; it stops at 40.5 s) and
  answers with what it has counted: the totals say "at least", the note
  says "Stopped after 41 s", and subfolders it did not reach show "–".
  Measure a subfolder to see more of it. An administrator can raise the
  limit to at most 90 s ([configuration](configuration.md)); a reverse
  proxy in front of the controller must then wait longer too
  ([deployment](deployment.md)).
- **One scan per worker at a time.** Measuring, searching a repository and
  comparing snapshots are heavy, so each worker runs one of them at a time
  (next to its normal listings). A second one is refused at once: "This
  worker is already measuring or searching. Try again in a moment."
- Like listings, scans count against your probe rate limit (20 at once,
  then two a second).
- A bucket is listed page by page (S3 returns 1,000 objects per request,
  a few tens of thousands of objects a second against a nearby server), so
  millions of objects may not fit in the limit; the walk then stops early,
  as above. SFTP and local disks are walked folder by folder.
- The list of a worker's allowed folders (the top of its page) has no
  **Calculate sizes**: measure one of them.
- Workers older than 2.1 cannot measure, search or compare: the request
  fails and says to upgrade the worker.

## Limits and known gaps

- There is no new folder, rename or delete, and no download (planned in
  [issue #36](https://github.com/Zeptiny/Syncr/issues/36)). A move is the
  only action that removes files, and only after copying them.
- A copy between two workers' folders needs a remote in between.
- Scans have no progress and cannot run longer than `probe.scan_timeout`;
  what does not fit is reported as "at least".
- Copy and move send at most 999 selected items in one job.
- Names that are not valid on Windows (`*`, `?`, a trailing space) cannot
  be copied to a Windows worker's disk, whatever was selected.
