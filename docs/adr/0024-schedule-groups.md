# ADR 0024: Schedule groups and the schedule list

- Status: accepted (unreleased until v2.4.0)
- Date: 2026-11-21

## Context

The schedule list was one table, by name, with no search, filters or
sorting, and every action worked on one schedule at a time. A home server
with a few workers soon has tens of schedules (a backup, its retention and
check per repository, transfers, two-way syncs), and their names had to
carry everything: what they do, where, and what belongs together. Pausing
everything on a laptop that travels meant opening each schedule.

What shaped the design:

- **Schedules are private to their owner, admins included** (ADR 0009),
  and the scheduler's guarantees (one fire per occurrence, one pending run,
  the two-phase launch, auto-pause with `paused_reason`) must not change.
- **"Labels" and "tags" already mean something:** worker labels pick a
  worker for a schedule, and restic snapshots carry tags
  (`syncr-remote:<id>`, ADR 0020).
- **`ListSchedules` returns every schedule of the caller, unpaged,** with
  its last run, remotes and repository names.

## Decision

### One optional group per schedule, the owner's

- A **group** (`schedule_groups`, migration 00014) has a name (1 to 64
  characters, unique per owner ignoring case) and an owner; a schedule has
  at most one (`schedules.group_id`, nullable). One level, ordered by
  name: no nesting, colours or manual order.
- **Not tags.** With several per schedule, a list in sections shows a
  schedule more than once, and "pause this group" becomes ambiguous. The
  other ways to slice the list (worker, kind, status) come from the
  schedules themselves and need no tagging.
- **Owner-only, like schedules:** another user's group is NotFound. The
  foreign key `(group_id, owner_id) REFERENCES schedule_groups (id,
  owner_id)` makes a schedule in another owner's group impossible in the
  database too; memstore checks the same. A group goes with its owner (ON
  DELETE CASCADE): a user who still owns schedules cannot be deleted, so
  such a group is empty.

### Groups only organize

- **Nothing that runs schedules reads groups:** no group-level enabled
  state, no inherited contacts or policies. A second on/off switch would
  make "why did this not run?" a question with two places to look, and
  resuming a group would have to decide what to do with schedules paused on
  their own or by Syncr. Inherited settings would change many schedules
  without anyone editing them.
- **Bulk actions are the per-schedule calls** (`RunScheduleNow`,
  `SetScheduleEnabled`, `DeleteSchedule`), a few at a time from the
  browser, so each schedule keeps its own checks (resuming re-checks its
  references), outcome and `paused_reason`, and the UI names the ones that
  failed. Only moving is one call.

### Moving

- `MoveSchedules` moves 1 to 500 of the caller's schedules into one of the
  caller's groups (or out of any) in one transaction: all or none. Like
  `SetScheduleEnabled` it takes no version; unlike a fire it changes the
  configuration, so it bumps each schedule's version. An edit read before
  the move then fails with ABORTED instead of putting the schedule back in
  its old group when it is saved.
- `DeleteScheduleGroup` moves the group's schedules out of it (version + 1)
  and deletes the group, in one transaction. `ON DELETE SET NULL
  (group_id)` (which keeps `owner_id`) catches a move into the group that
  commits in between.
- Groups never wake the scheduler.

### The list is computed in the browser

- Search, filters (kind, worker or label, group, status), sorting (name,
  next run, last run) and grouping (by group, worker or label, kind,
  status, or none) run on the full `ListSchedules` answer; their state is
  in the URL, folded sections in `localStorage` (`syncr-schedule-sections`).
  `ListSchedules` gets no group filter.
- **Needs attention** is a schedule Syncr paused, or one that is on and
  whose last run was missed, could not start, or whose job failed or was
  interrupted. Cancelled runs and jobs do not count: someone cancelled
  them.

### Metrics

`syncr_schedule_info` gets a `group` label (the group's name, empty for
none). Other schedule series keep their labels, so existing queries and
alerts are unchanged, and alerts join the info series to route by group.

## Consequences

- Migration 00014 adds a table, a nullable column, its foreign key and a
  partial index; its down script drops them, ungrouping everything.
- Lists of a few hundred schedules are filtered and sorted in the browser
  without trouble; much larger ones would need server-side paging and
  filters, which `ListSchedules` does not have.
- An admin cannot organize other users' schedules, and groups are not
  shared between users.
- Grouping other lists (remotes, repositories) would follow the same
  pattern; it is not done.
