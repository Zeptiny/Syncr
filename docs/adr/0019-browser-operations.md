# ADR 0019: Browser operations

- Status: accepted (v2.1.0)
- Date: 2026-11-13

## Context

Syncr could list a worker's folders, a remote and a snapshot, but only to
pick one path for a form or to restore from a snapshot. People asked for a
file browser: select several files and folders, copy or move them, see
which folder takes the space, find a file in a repository's snapshots and
its older versions, compare two snapshots, and put restored files back
where they were.

What Syncr already had, and what shaped the design:

- **Listings are probes** (ADR 0012): synchronous requests a worker answers
  within the call, bounded by `probe.timeout` (30 s) and
  `probe.restic_timeout` (60 s), never above `link.MaxProbeTimeout`
  (90 s), at most 4 at once per worker, 5,000 entries and 4 MiB per
  result. Workers advertise the operations they can run (`probe_ops`),
  and the controller only picks a worker that lists the operation.
- **Copy and move are jobs** (`rclone_transfer`): one source location, one
  destination, options including filter rules. Every user may run them on
  every worker.
- **Restic restore** takes a target, literal tree paths (includes), and a
  subfolder whose content goes straight into the target, but not both.
- **The proxy.** The documented reverse-proxy read timeout was "above
  40 s"; nginx's default is 60 s. A call that runs for 90 s is cut off by a
  default proxy.

And what the tools do, checked against the pinned rclone 1.75.2 and restic
0.19.1:

- `rclone lsjson -R` streams its listing page by page. It uses the
  backend's recursive listing when it has one, with or without
  `--fast-list` (S3: 21 requests for 20,000 objects); SFTP and local disks
  are walked folder by folder. An unreadable folder is logged and skipped,
  and rclone exits non-zero after listing the rest.
- `restic ls --json --recursive -- <snapshot> <dir>` streams one node per
  line.
- `restic find --json` prints its whole result as **one line**, written
  as it goes, one object per snapshot with matches, newest snapshot first.
  A pattern without `/` matches any name in a path; one with `/` matches
  the path's trailing folders, and a leading `/` anchors it at the root. A
  match on a folder also matches everything below it. Character classes
  (`[*]`, `[?]`, `[[]`) match glob characters literally, also where `\` is
  a path separator.
- `restic diff --json -- <a>:<dir> <b>:<dir>` streams `change` lines and
  ends with a `statistics` line; paths are relative to `<dir>`. With
  `--metadata` it marks every folder above a change as changed, and turns
  a type change into "TU"; without it, a change of metadata alone is not
  reported.
- rclone's filter syntax is escaped with `\` before `\ * ? [ ] { }`. A
  pattern that starts with `/` is anchored, so a name starting with `-`,
  `+`, `#`, `;` or `!` needs nothing more. For `+ /a/b/c.txt` then `- **`,
  rclone descends into the parent folders by itself. `--filter-from` trims
  lines (a trailing space is lost); `--filter` does not.
- `rclone move --delete-empty-src-dirs` with such filters removes the
  folders it moved and folders it emptied, never the source folder or an
  unselected folder. Without `--create-empty-src-dirs`, a selected empty
  folder is deleted from the source and never created at the destination.
- `restic restore --include=<path> -- <snapshot>:<subfolder>` matches the
  includes against paths relative to the subfolder.

## Decision

### Sizes, search and compare are probes that end early

Measuring a folder (`measure_local`, `measure_remote`, `restic_measure`),
searching a repository (`restic_find`) and comparing two snapshots
(`restic_diff`) are five new probe operations. They are read-only and
answer within the request, like listings, through the same broker, rate
limit, owner scoping, worker pick and error mapping. A new operation needs
no link feature: workers that do not list it are not picked.

They walk whole trees, so they get their own limit, **`probe.scan_timeout`**
(45 s by default, 1 s to 90 s). The worker runs the operation under a
**soft deadline** before that limit (the limit minus a tenth, at least
2 s: 40.5 s of 45 s) and, when it is reached, stops the tool and answers
**OK with what it found** and `complete = false`. Every number is then a
lower bound, and the UI says "at least" and "stopped after 41 s". A cancel
from the caller stays CANCELLED, and a worker still busy at the probe's
own limit is TIMEOUT, as for every probe. The 45 s default plus the API's
15 s margin for its own work fits a proxy with nginx's default 60 s, so
the documented proxy read timeout goes from "above 40 s" to "above
60 s", and above `probe.scan_timeout` plus 15 s when it is raised.

The outputs are bounded too: a measurement lists at most 5,000 child
folders, largest first; a search at most 1,000 matches (workers cap any
request at 5,000), newest snapshot first; a comparison at most 2,000 changes, but it reads
on, so its statistics still cover everything. Over the 4 MiB result cap
the smallest children, oldest matches or last changes are dropped and the
result says it is truncated. Entries the walk cannot read are counted
(`unreadable`) and skipped, so one folder without permission does not
fail a measurement of the disk. Links and junctions are never followed:
they count as entries of no size.

The worker reads `restic find`'s single line in chunks and tracks the
JSON brackets, so it can stop early and keep the snapshots it finished.
It lists the snapshots first, for their order, for the number searched and
to tell an unknown snapshot from one without matches.

Nothing is stored. Results live in the browser's query cache until the
user refreshes or a job started from the page ends.

The scans accept no GET, so a link cannot start one through the API. The
web's search and compare pages keep the scan in their address, to share
or bookmark, but opening the address alone (a link, a reload) shows the
filled form and its button. The app's own ways in (the page's form, the
snapshot list, the snapshot browser's **Compare with…**) ask for the run
in the router's history state, which a link cannot carry, tagged with the
page load so that a reload does not count.

The worker's measuring walk (`measure_local`) does not enter Linux pseudo
filesystems (proc, sysfs, cgroup, debugfs and the like), recognized by
their filesystem type (`fstatfs`), not by path: they hold kernel state,
and `/proc/kcore` alone reports the address space's size. Other mounted
filesystems are walked.

### One heavy probe per worker

A worker runs at most one of the five scans at a time (`MaxHeavy`,
default 1), in addition to one of its normal probe slots. A second one is
refused at once with BUSY, which the API maps to UNAVAILABLE and the UI
words as "This worker is already measuring or searching. Try again in a
moment." A scan refused because the worker's normal probe slots are all
taken is BUSY too; the protocol has one code for both, so the worker
words them differently (`link.ProbeBusyHeavy`, `link.ProbeBusySlots`)
and the controller and the UI say "busy with other requests" for the
second. Listings stay fast while a scan runs, and a few users clicking
**Calculate sizes** cannot keep a NAS's disks busy with parallel walks.

### Copy and move are transfer jobs with filters

A selection is copied or moved by an ordinary `rclone_transfer` job, with
**file-manager semantics**: a selected folder `photos` copied into `D`
arrives as `D/photos`. rclone copies a folder's contents, so the web turns
the selection into:

- the source: the selected items' deepest common folder (for one item,
  its parent);
- filters, in order: each selected item that is not inside another
  selected folder, relative to the source and escaped, as `+ /<file>` or
  `+ /<folder>/**`; then `- **`;
- the chosen destination folder.

The definition always sets `create_empty_src_dirs` (so a selected empty
folder is not lost by a move) and, for a move, `delete_empty_src_dirs`.
The conflict choice is worded like restore's: **Keep existing**
(`ignore_existing`, the default), Replace files that differ (rclone's
default), Replace older files (`update`). A preview is the same job with
`dry_run`.

The rules for the worker follow from where the data is: a side on a
worker's disk (a worker folder, or a remote of the local backend listed by
it) pins the job to that worker; two different workers' disks are refused
("copy through a remote"); between remotes the worker that listed the
source is proposed. The web code that builds the filters is checked
against rclone with a golden file (`web/src/lib/rclone-filters.golden.json`:
a tree with awkward names and what `rclone lsf -R` matches for each
selection), and the system test copies and moves the same cases through
MinIO with the real binaries.

### Restoring selected items to their original place

"Restore to original location" restores the selected items of a snapshot
with `subfolder` set to their common folder and `paths` set to the items,
into that folder as it is on the snapshot's machine (the worker whose
hostname is the snapshot's host, proposed; existing files kept). The
worker makes each path relative to the subfolder and passes it as an
include, so the items land at their own paths and nothing beside them is
written.

That combination used to be refused by both the controller and the
worker. It is now valid when every path lies strictly inside the
subfolder, and it needs a new link feature,
`restic.restore_subfolder_paths`. A kind may now require features for
some definitions only (`jobkind.DefinitionRequirer`), and the dispatcher
checks them: a worker without the feature fails such a job with
`WORKER_OUTDATED` instead of a misleading invalid-spec failure. Plain
subfolder or item restores still go to older workers.

### Downloads are not here

Downloading a file through the controller (issue #36) needs a streaming
path from the worker through the link to the browser, with limits,
resumption and authorization of its own; probes carry at most 4 MiB and
jobs carry no data back. It is a separate design. The same goes for new
folder, rename and delete, which write and therefore need job kinds of
their own, and for forgetting single snapshots and editing tags.

## Consequences

- Folder sizes, searches and comparisons work on every worker of 2.1 or
  later, with nothing stored and no migration; older workers are not
  picked for them.
- A scan larger than the limit returns a partial answer instead of an
  error, and the UI says what is missing. There is no progress and no way
  to get the full answer of a very large tree, beyond raising
  `probe.scan_timeout` to 90 s.
- Each worker runs one scan at a time; a second user waits a moment.
- Deployments behind a proxy with a read timeout between 40 s and 60 s
  must raise it, or long scans end in a gateway error.
- Copies and moves are visible, cancellable jobs with their filters on the
  job page, and run under the same rules as any transfer, including
  concurrency, notifications and time limits. A filtered move removes
  only what it moved, and folders that became empty. Like a file
  manager's "skip", a move with Keep existing leaves the files that were
  already at the destination in the source; the confirmation, the end
  message and the docs say so. (Under Replace older files rclone deletes
  an older source file when the destination has a newer copy.)
- The filters depend on rclone's syntax; the golden file and the system
  test catch a change on an rclone upgrade.
- Comparisons do not show changes of metadata alone.
- A restore of selected items to their original place needs a 2.1 worker;
  the restore sheet proposes only workers that report the feature and
  shows the others as outdated.
- A missing snapshot and a missing folder both come back from the worker
  as NOT_FOUND, and the controller tells them apart by restic's message
  ("no snapshot", "no matching ID"), so a folder named like that is
  reported on the snapshot. A structured code needs a protocol change;
  this is a known limitation.
- A stopped search keeps the matches of the snapshot restic was printing
  only when one snapshot can be it: restic names a snapshot after its
  matches and orders snapshots of equal times its own way.
- Windows paths (`C:` in snapshot trees, `\` as a separator for restic's
  patterns, junctions in walks) are covered by unit tests and recorded
  output here, and only proven by Windows CI.

## Alternatives considered

- **Scans as jobs.** Jobs are durable, cancellable and have no time
  limit, but a folder size would then be a job in the history, queued
  behind transfers and serialized by repository keys, with its result
  stored. A user who clicks **Calculate sizes** expects an answer in
  seconds, not a job.
- **Asynchronous probes** (start, poll, fetch): full answers for any size,
  but a new protocol in the link, the broker and the API, and result
  storage. Partial answers cover the common case; this stays a follow-up.
- **One probe per subfolder** for sizes: one walk of the open folder,
  summed per child, does the same work once.
- **`--fast-list` for buckets:** rclone 1.75.2 already uses the
  backend's recursive listing without it (the same 21 requests for 20,000
  objects on MinIO), so it would change nothing there; the measurement
  passes neither it nor anything that delays the stream.
- **Copy and move as new job kinds** taking a list of items: rclone has no
  single command for a list of items into a folder with these semantics;
  one job per item would lose the single job page, preview and cancel.
  Filters on one transfer do it with the existing kind and runner.
- **`--files-from`** instead of filters: it does not take folders
  recursively, and a list file would need a new spec field.
- **Restore to original location with negated excludes** (a `subfolder`
  restore whose excludes drop everything but the items): it would have
  run on older workers, but it turns a selection into exclude-pattern
  logic and the job would read as a restore of the whole folder.
  Includes relative to the subfolder are what restic supports for this,
  at the cost of a link feature.
- **`restic diff --metadata`:** it marks every folder above a changed
  file, which buries the real changes; metadata-only changes are rarely
  what a user compares snapshots for.
