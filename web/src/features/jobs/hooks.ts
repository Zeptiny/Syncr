import { create } from "@bufbuild/protobuf";

import type { FieldViolation, Job, JobDefinition, Worker } from "@/gen/syncr/v1/api_pb";
import {
  HookPhase,
  HookStatus,
  JobHooksSchema,
  type HookInfo,
  type HookRun,
  type JobHooks,
} from "@/gen/syncr/v1/link_pb";
import { durationSeconds } from "@/lib/format";
import type { Tone } from "@/lib/status";
import { FEATURE_HOOKS, hasFeature } from "@/lib/workers";

/**
 * A hook is a command a worker's operator defined in the worker's
 * configuration under a name. A job names hooks to have them run around
 * its operation; the controller never knows more of a hook than its name, a
 * description and a timeout.
 */

/** MAX_HOOKS_PER_PHASE is how many hooks a job may name for one phase. */
export const MAX_HOOKS_PER_PHASE = 8;

/** HookPhaseKey is a phase as the form keeps it: a JobHooks field. */
export type HookPhaseKey = "before" | "onSuccess" | "onFailure" | "after";

/** HooksState is the editable form of JobDefinition.hooks: names, in the order they run. */
export type HooksState = Record<HookPhaseKey, string[]>;

/** HookPhaseMeta describes a phase to people. */
export interface HookPhaseMeta {
  key: HookPhaseKey;
  phase: HookPhase;
  /** label heads the phase in a form or a list. */
  label: string;
  /** help says when its hooks run and what a failing one does. */
  help: string;
  /** field is the phase's name in the API's field paths ("definition.hooks.on_success[0]"). */
  field: string;
}

/** hookPhases are the phases in the order they run. */
export const hookPhases: HookPhaseMeta[] = [
  {
    key: "before",
    phase: HookPhase.BEFORE,
    label: "Before the job",
    help: "Run first, in order. If one fails, the job does not run and fails.",
    field: "before",
  },
  {
    key: "onSuccess",
    phase: HookPhase.ON_SUCCESS,
    label: "After success",
    help: "Run when the job succeeded. If one fails, the job fails; what it did is kept.",
    field: "on_success",
  },
  {
    key: "onFailure",
    phase: HookPhase.ON_FAILURE,
    label: "After failure",
    help: "Run when the job failed, whatever failed. Not when it was cancelled.",
    field: "on_failure",
  },
  {
    key: "after",
    phase: HookPhase.AFTER,
    label: "Always, at the end",
    help: "Run last whatever happened, also after a cancel or the time limit: the place to undo what a hook before the job did.",
    field: "after",
  },
];

/** phaseMeta returns the description of a wire phase; undefined for one this build does not know. */
export function phaseMeta(phase: HookPhase): HookPhaseMeta | undefined {
  return hookPhases.find((p) => p.phase === phase);
}

/** emptyHooks names no hooks. */
export function emptyHooks(): HooksState {
  return { before: [], onSuccess: [], onFailure: [], after: [] };
}

/** hooksState fills the form from a stored definition (a job or a schedule). */
export function hooksState(def: JobDefinition | undefined): HooksState {
  const h = def?.hooks;
  return {
    before: [...(h?.before ?? [])],
    onSuccess: [...(h?.onSuccess ?? [])],
    onFailure: [...(h?.onFailure ?? [])],
    after: [...(h?.after ?? [])],
  };
}

/** hasHooks reports whether the form names at least one hook. */
export function hasHooks(s: HooksState): boolean {
  return hookPhases.some((p) => s[p.key].length > 0);
}

/** hookNames lists every hook the form names, once each, in the order they run. */
export function hookNames(s: HooksState): string[] {
  return [...new Set(hookPhases.flatMap((p) => s[p.key]))];
}

/** toHooks builds JobDefinition.hooks; undefined when no hook is named. */
export function toHooks(s: HooksState): JobHooks | undefined {
  if (!hasHooks(s)) return undefined;
  return create(JobHooksSchema, {
    before: [...s.before],
    onSuccess: [...s.onSuccess],
    onFailure: [...s.onFailure],
    after: [...s.after],
  });
}

/** HookChoice is a hook the form offers. */
export interface HookChoice {
  name: string;
  description: string;
  /** timeoutSeconds is how long one run may take; 0 when unknown. */
  timeoutSeconds: number;
  /** workers counts the workers, among those the job may run on, that define it. */
  workers: number;
}

/**
 * offeredHooks lists the hooks of the workers a job may run on, by name: one
 * worker's own, or for a label every hook one of its workers defines, with
 * how many do. The description and timeout are the first worker's that has
 * them.
 */
export function offeredHooks(workers: Worker[]): HookChoice[] {
  const byName = new Map<string, HookChoice>();
  for (const w of workers) {
    for (const h of w.info?.hooks ?? []) {
      const seen = byName.get(h.name);
      if (seen) {
        seen.workers++;
        seen.description ||= h.description;
        seen.timeoutSeconds ||= durationSeconds(h.timeout);
      } else {
        byName.set(h.name, {
          name: h.name,
          description: h.description,
          timeoutSeconds: durationSeconds(h.timeout),
          workers: 1,
        });
      }
    }
  }
  return [...byName.values()].sort((a, b) => a.name.localeCompare(b.name));
}

/** definesHooks reports whether a worker defines every one of names. */
export function definesHooks(w: Worker, names: string[]): boolean {
  const defined = new Set((w.info?.hooks ?? []).map((h: HookInfo) => h.name));
  return names.every((n) => defined.has(n));
}

/**
 * HooksTarget is what the form knows about where the job runs, as far as
 * hooks go.
 */
export interface HooksTarget {
  /** workers are the ones the job may run on that have reported in. */
  workers: Worker[];
  /** label is set when the job runs on any worker with a label. */
  label?: string;
}

/**
 * unavailableHooks lists the hooks the form names that the job's target
 * cannot run: for one worker the ones it does not define, for a label the
 * ones none of its workers defines. Empty while nothing is known about the
 * target (no worker chosen, or none has reported in): the controller
 * decides then.
 */
export function unavailableHooks(s: HooksState, target: HooksTarget): string[] {
  if (target.workers.length === 0) return [];
  const offered = new Set(offeredHooks(target.workers).map((h) => h.name));
  return hookNames(s).filter((n) => !offered.has(n));
}

/**
 * hooksNote says what the form should tell the user about the target
 * before they choose hooks; undefined when there is nothing to say.
 */
export function hooksNote(s: HooksState, target: HooksTarget): string | undefined {
  const { workers, label } = target;
  if (workers.length === 0) return undefined;
  const able = workers.filter((w) => hasFeature(w, FEATURE_HOOKS));
  if (able.length === 0) {
    return label
      ? `The workers labelled ${label} must be upgraded before they can run hooks.`
      : `${workers[0].name} must be upgraded before it can run hooks.`;
  }
  if (!label) return undefined;
  const names = hookNames(s);
  if (names.length === 0) {
    return "A run goes to a worker with the label that defines every hook chosen here.";
  }
  const n = able.filter((w) => definesHooks(w, names)).length;
  if (n === 0) {
    return `No worker labelled ${label} defines all of these hooks, so a run would find no worker.`;
  }
  return `${n} of ${workers.length} ${workers.length === 1 ? "worker" : "workers"} labelled ${label} ${n === 1 ? "defines" : "define"} all of these hooks; a run goes to one of them.`;
}

/**
 * hooksError places one API violation on the hooks, returning its message
 * for people, or undefined when the field is not part of the hooks. Paths
 * look like definition.hooks.before[1] or definition.hooks.
 */
export function hooksError(v: FieldViolation): string | undefined {
  const m = /(?:^|\.)hooks(?:\.([a-z_]+))?(?:\[(\d+)\])?$/.exec(v.field);
  if (!m) return undefined;
  const phase = hookPhases.find((p) => p.field === m[1]);
  // A message about a worker stands by itself; the others need their place.
  if (!phase || v.message.startsWith("worker ")) return v.message;
  const where = m[2] === undefined ? phase.label : `${phase.label}, hook ${Number(m[2]) + 1}`;
  return `${where}: ${v.message}`;
}

/** HookStatusMeta describes how a hook run's status is shown. */
export interface HookStatusMeta {
  label: string;
  tone: Tone;
}

const hookStatuses: Partial<Record<HookStatus, HookStatusMeta>> = {
  [HookStatus.SUCCEEDED]: { label: "Succeeded", tone: "success" },
  [HookStatus.FAILED]: { label: "Failed", tone: "danger" },
  [HookStatus.TIMED_OUT]: { label: "Timed out", tone: "danger" },
  [HookStatus.CANCELLED]: { label: "Stopped", tone: "warning" },
};

/** hookStatusMeta returns the label and tone of a hook run's status. */
export function hookStatusMeta(status: HookStatus): HookStatusMeta {
  return hookStatuses[status] ?? { label: "Unknown", tone: "neutral" };
}

/** HookRow is one line of a job's hooks: a run, or a hook that did not run. */
export interface HookRow {
  name: string;
  /** run is how it went; undefined for a hook that has not run. */
  run?: HookRun;
}

/** HookGroup is the hooks of one phase of a job. */
export interface HookGroup {
  phase: HookPhaseMeta;
  rows: HookRow[];
}

/**
 * jobHookGroups lays out a job's hooks by phase, in the order the phases
 * run: every run its worker reported, then the hooks the definition names
 * for the phase that did not run (skipped, or not reached yet). Phases
 * without hooks are left out. A run of a phase this build does not know is
 * listed under the last phase, so nothing a worker reported is hidden.
 */
export function jobHookGroups(job: Job): HookGroup[] {
  const named = hooksState(job.definition);
  const groups: HookGroup[] = [];
  for (const phase of hookPhases) {
    const last = phase === hookPhases[hookPhases.length - 1];
    const runs = job.hookRuns.filter(
      (r) => r.phase === phase.phase || (last && !phaseMeta(r.phase)),
    );
    const rows: HookRow[] = runs.map((run) => ({ name: run.name, run }));
    const ran = new Set(runs.map((r) => r.name));
    for (const name of named[phase.key]) {
      if (!ran.has(name)) rows.push({ name });
    }
    if (rows.length) groups.push({ phase, rows });
  }
  return groups;
}

/** hookSummary names a definition's hooks in one line: "Before: a, b · Always: c". */
export function hookSummary(s: HooksState): string {
  const short: Record<HookPhaseKey, string> = {
    before: "Before",
    onSuccess: "After success",
    onFailure: "After failure",
    after: "Always",
  };
  return hookPhases
    .filter((p) => s[p.key].length > 0)
    .map((p) => `${short[p.key]}: ${s[p.key].join(", ")}`)
    .join(" · ");
}
