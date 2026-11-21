// The schedule list's model: what each schedule needs to be found, sorted
// and placed in a section. Everything here is computed in the browser from
// ListSchedules, which returns every schedule of the user.

import { JobStatus, type Worker } from "@/gen/syncr/v1/api_pb";
import {
  ScheduleRunStatus,
  type Schedule,
  type ScheduleGroup,
  type ScheduleRun,
} from "@/gen/syncr/v1/schedules_pb";
import { cronText } from "@/lib/cron";
import { tsDate } from "@/lib/format";
import { kindLabel } from "@/lib/status";

import { jobRoute, resticSummary } from "../jobs/model";
import { asJob, kindOf } from "./model";
import { runState } from "./run-state";
import type { GroupBy, ScheduleSort, SchedulesSearch, StatusFilter } from "./search";

/** kindOrder is the order of kinds in lists, as the forms offer them. */
const kindOrder = [
  "rclone_transfer",
  "rclone_bisync",
  "restic_backup",
  "restic_forget",
  "restic_check",
  "restic_prune",
];

/**
 * lastRunProblem reports whether a run went wrong in a way to look at: it
 * was missed or could not start, or its job failed or was interrupted. A
 * cancelled run or job is not one: someone (or a pause) cancelled it.
 */
export function lastRunProblem(run: ScheduleRun | undefined): boolean {
  if (!run || !runState(run)?.problem) return false;
  if (run.status === ScheduleRunStatus.CANCELLED) return false;
  return !(run.status === ScheduleRunStatus.LAUNCHED && run.jobStatus === JobStatus.CANCELLED);
}

/**
 * needsAttention reports whether a schedule needs a look: Syncr paused it
 * (its worker or a remote went away), or it is on and its last run went
 * wrong. A schedule paused by its owner does not.
 */
export function needsAttention(s: Schedule): boolean {
  if (!s.enabled) return !!s.pausedReason;
  return lastRunProblem(s.lastRun);
}

/** statusOf is the one status a schedule is listed under. */
export function statusOf(s: Schedule): StatusFilter {
  if (needsAttention(s)) return "attention";
  return s.enabled ? "on" : "paused";
}

/** statusTitles name the statuses, in the order of the status sections. */
export const statusTitles: Record<StatusFilter, string> = {
  attention: "Needs attention",
  on: "On",
  paused: "Paused",
};

/** targetKey identifies where a schedule runs: "w:<worker id>" or "l:<label>". */
export function targetKey(s: Schedule): string {
  const t = s.target?.target;
  if (t?.case === "workerLabel") return `l:${t.value}`;
  return `w:${t?.case === "workerId" ? t.value : ""}`;
}

/** targetTitle names a target key: the worker, or the label's workers. */
export function targetTitle(key: string, workers: Map<string, Worker>): string {
  if (key.startsWith("l:")) return `Workers labelled ${key.slice(2)}`;
  return workers.get(key.slice(2))?.name ?? "Removed worker";
}

/** ListContext is what the list needs besides the schedules. */
export interface ListContext {
  workers: Map<string, Worker>;
  groups: ScheduleGroup[];
}

/**
 * searchText is everything a schedule can be found by, lower-cased: its
 * name, description and group, when it runs, what it runs (kind, remotes,
 * paths, repository) and where.
 */
export function searchText(s: Schedule, ctx: ListContext): string {
  const parts: string[] = [s.name, s.description, cronText(s.cron), s.cron, s.timezone];
  parts.push(kindLabel(kindOf(s)));
  const group = ctx.groups.find((g) => g.id === s.groupId);
  if (group) parts.push(group.name);
  parts.push(targetTitle(targetKey(s), ctx.workers));
  const t = s.target?.target;
  if (t?.case === "workerLabel") parts.push(t.value);
  for (const r of s.remotes) parts.push(r.name);
  if (s.repository?.name) parts.push(s.repository.name);
  const route = jobRoute(asJob(s));
  if (route) parts.push(route.a.path, route.b.path, route.mode);
  const restic = resticSummary(s.definition, s.repository, s.remotes);
  if (restic) parts.push(restic.title, restic.detail);
  return parts.filter(Boolean).join("\n").toLowerCase();
}

/** filtered reports whether any filter (not the grouping or sort) is set. */
export function filtered(search: SchedulesSearch): boolean {
  return !!(search.q || search.kind || search.target || search.status || search.group);
}

/** matches reports whether a schedule passes the search's filters. */
export function matches(s: Schedule, search: SchedulesSearch, ctx: ListContext): boolean {
  if (search.kind && kindOf(s) !== search.kind) return false;
  if (search.target && targetKey(s) !== search.target) return false;
  if (search.group) {
    if (search.group === "none" ? !!s.groupId : s.groupId !== search.group) return false;
  }
  switch (search.status) {
    case "attention":
      if (!needsAttention(s)) return false;
      break;
    case "on":
      if (!s.enabled) return false;
      break;
    case "paused":
      if (s.enabled) return false;
      break;
  }
  if (search.q) {
    const text = searchText(s, ctx);
    if (
      !search.q
        .toLowerCase()
        .split(/\s+/)
        .filter(Boolean)
        .every((w) => text.includes(w))
    )
      return false;
  }
  return true;
}

const byName = (a: Schedule, b: Schedule) =>
  a.name.localeCompare(b.name, undefined, { sensitivity: "base" }) || a.id.localeCompare(b.id);

/**
 * sortSchedules orders a list: by name, by next run (paused schedules
 * last) or by last run. Schedules without the time sorted by come last in
 * either direction; ties go by name.
 */
export function sortSchedules(list: Schedule[], sort: ScheduleSort = "name"): Schedule[] {
  const desc = sort.startsWith("-");
  const key = desc ? sort.slice(1) : sort;
  const time = (s: Schedule) =>
    key === "next" ? tsDate(s.nextRunAt)?.getTime() : tsDate(s.lastRun?.scheduledFor)?.getTime();
  return [...list].sort((a, b) => {
    if (key === "name") return desc ? byName(b, a) : byName(a, b);
    const ta = time(a);
    const tb = time(b);
    if (ta === undefined || tb === undefined) {
      if (ta !== tb) return ta === undefined ? 1 : -1;
      return byName(a, b);
    }
    return (desc ? tb - ta : ta - tb) || byName(a, b);
  });
}

/** sortDefaults is the first direction of each sort key (last run: newest first). */
const sortDefaults: Record<string, ScheduleSort> = { name: "name", next: "next", last: "-last" };

/** nextSort is the sort after clicking a column: its default, then the reverse. */
export function nextSort(
  current: ScheduleSort | undefined,
  key: "name" | "next" | "last",
): ScheduleSort {
  const cur = current ?? "name";
  const curKey = cur.replace(/^-/, "");
  if (curKey !== key) return sortDefaults[key];
  return cur.startsWith("-") ? key : `-${key}`;
}

/** Section is one titled part of the list. */
export interface Section {
  /** key is unique among the sections of one grouping (it keys the collapsed state). */
  key: string;
  title: string;
  /** group is the schedule group of a section of the "group" grouping ("" for "No group"). */
  group?: ScheduleGroup;
  schedules: Schedule[];
}

/**
 * sections splits a sorted list into the sections of a grouping, in
 * order: groups by name, then "No group"; workers by name, then labels;
 * kinds as the forms list them; statuses as statusTitles does. "none"
 * gives one section without a title, and so does "group" while the user
 * has no groups (none at all for an empty list, so the table shows its
 * empty state). showEmpty also lists groups without schedules (when no
 * filter is set, so they can be renamed or deleted).
 */
export function sections(
  list: Schedule[],
  by: GroupBy,
  ctx: ListContext,
  showEmpty = false,
): Section[] {
  if (by === "none" || (by === "group" && ctx.groups.length === 0)) {
    return list.length ? [{ key: "all", title: "", schedules: list }] : [];
  }
  const buckets = new Map<string, Schedule[]>();
  const keyOf = (s: Schedule): string => {
    switch (by) {
      case "group":
        return ctx.groups.some((g) => g.id === s.groupId) ? s.groupId : "";
      case "target":
        return targetKey(s);
      case "kind":
        return kindOf(s);
      case "status":
        return statusOf(s);
    }
  };
  for (const s of list) {
    const k = keyOf(s);
    buckets.set(k, [...(buckets.get(k) ?? []), s]);
  }
  const take = (key: string, title: string, group?: ScheduleGroup): Section => ({
    key,
    title,
    group,
    schedules: buckets.get(key) ?? [],
  });
  let out: Section[];
  switch (by) {
    case "group":
      out = [...ctx.groups.map((g) => take(g.id, g.name, g)), take("", "No group")];
      return out.filter((sec) => sec.schedules.length > 0 || (showEmpty && sec.group));
    case "target": {
      const keys = [...buckets.keys()];
      const title = (k: string) => targetTitle(k, ctx.workers);
      keys.sort(
        (a, b) =>
          Number(a.startsWith("l:")) - Number(b.startsWith("l:")) ||
          title(a).localeCompare(title(b), undefined, { sensitivity: "base" }),
      );
      return keys.map((k) => take(k, title(k)));
    }
    case "kind": {
      const rank = (k: string) => (kindOrder.includes(k) ? kindOrder.indexOf(k) : kindOrder.length);
      return [...buckets.keys()]
        .sort((a, b) => rank(a) - rank(b) || a.localeCompare(b))
        .map((k) => take(k, k ? kindLabel(k) : "Unreadable"));
    }
    case "status":
      out = (Object.keys(statusTitles) as StatusFilter[]).map((k) => take(k, statusTitles[k]));
      return out.filter((sec) => sec.schedules.length > 0);
  }
}

/** Counts sums a list up for the summary above it. */
export interface Counts {
  total: number;
  attention: number;
  on: number;
  paused: number;
}

/** counts counts a list by status (a schedule needing attention is also on or paused). */
export function counts(list: Schedule[]): Counts {
  const out = { total: list.length, attention: 0, on: 0, paused: 0 };
  for (const s of list) {
    if (needsAttention(s)) out.attention++;
    if (s.enabled) out.on++;
    else out.paused++;
  }
  return out;
}
