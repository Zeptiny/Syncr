// URL state of the schedule pages, kept apart from them for code splitting.

import { formKind, type FormKind } from "../jobs/search";

/** StatusFilter is a status the list can be narrowed to. */
export type StatusFilter = "attention" | "on" | "paused";

/** GroupBy is how the list is split into sections. */
export type GroupBy = "group" | "target" | "kind" | "status" | "none";

/** ScheduleSort orders the list; a leading "-" reverses it. */
export type ScheduleSort = "name" | "-name" | "next" | "-next" | "last" | "-last";

/** SchedulesSearch is the schedule list's URL state. */
export interface SchedulesSearch {
  /** q is searched in names, descriptions, groups, paths, remotes and more. */
  q?: string;
  kind?: string;
  /** target is "w:<worker id>" or "l:<label>". */
  target?: string;
  status?: StatusFilter;
  /** group is a group ID, or "none" for the schedules in no group. */
  group?: string;
  /** by is the grouping; unset means by group. */
  by?: GroupBy;
  /** sort is the order; unset means by name. */
  sort?: ScheduleSort;
}

const groupBys: GroupBy[] = ["group", "target", "kind", "status", "none"];
const sorts: ScheduleSort[] = ["name", "-name", "next", "-next", "last", "-last"];
const statuses: StatusFilter[] = ["attention", "on", "paused"];

/** validateSchedulesSearch parses /schedules's search params. */
export function validateSchedulesSearch(s: Record<string, unknown>): SchedulesSearch {
  const str = (v: unknown) => (typeof v === "string" && v ? v : undefined);
  const one = <T extends string>(v: unknown, list: T[]) => list.find((x) => x === v);
  const by = one(s.by, groupBys);
  const sort = one(s.sort, sorts);
  return {
    q: str(s.q),
    kind: str(s.kind),
    target: str(s.target),
    status: one(s.status, statuses),
    group: str(s.group),
    by: by === "group" ? undefined : by,
    sort: sort === "name" ? undefined : sort,
  };
}

/** NewScheduleSearch says what a new schedule starts from. */
export interface NewScheduleSearch {
  /** from is a schedule to duplicate. */
  from?: string;
  /** job is a job to repeat ("Schedule this job"). */
  job?: string;
  /** kind starts the form on a job kind; a restore cannot be scheduled. */
  kind?: FormKind;
  /** repository preselects a repository for the restic kinds. */
  repository?: string;
  /** group puts the new schedule in this group. */
  group?: string;
}

/** validateNewScheduleSearch parses /schedules/new's search params. */
export function validateNewScheduleSearch(s: Record<string, unknown>): NewScheduleSearch {
  const str = (v: unknown) => (typeof v === "string" && v ? v : undefined);
  return {
    from: str(s.from),
    job: str(s.job),
    kind: formKind(s.kind),
    repository: str(s.repository),
    group: str(s.group),
  };
}
