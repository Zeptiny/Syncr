import type { MessageInitShape } from "@bufbuild/protobuf";

import type { Job, Worker } from "@/gen/syncr/v1/api_pb";
import type { NotificationEvent } from "@/gen/syncr/v1/notify_pb";
import {
  MissedRunAction,
  OverlapPolicy,
  type MissedRunPolicy,
  type MissedRunPolicySchema,
  type Schedule,
  type ScheduleInputSchema,
  type ScheduleTarget,
} from "@/gen/syncr/v1/schedules_pb";
import { fieldViolations } from "@/lib/errors";
import { durationFromSeconds, durationSeconds, formatDuration } from "@/lib/format";
import { browserTimeZone } from "@/lib/timezones";

import {
  definitionError,
  definitionState,
  emptyDefinition,
  forWorkerOS,
  toDefinition,
  validateDefinition,
  type DefinitionErrors,
  type EditableKind,
  type JobDefinitionState,
} from "../jobs/definition";

const HOUR = 3600;

/** MissedState is the missed-run policy as the form edits it. */
export interface MissedState {
  action: "run_late" | "skip";
  /** within is how late a run may start, in seconds; 0 means until the next run. */
  within: number;
}

/** withinPresets are the "start within" choices of a late run, in seconds. */
export const withinPresets: { seconds: number; label: string }[] = [
  { seconds: 15 * 60, label: "15 minutes" },
  { seconds: HOUR, label: "1 hour" },
  { seconds: 6 * HOUR, label: "6 hours" },
  { seconds: 12 * HOUR, label: "12 hours" },
  { seconds: 24 * HOUR, label: "24 hours" },
  { seconds: 0, label: "Until the next run is due" },
];

/** The longest a run may wait for its worker, in seconds. */
export const MAX_WITHIN = 7 * 24 * HOUR;

/** withinLabel names a "start within" value. */
export function withinLabel(seconds: number): string {
  return withinPresets.find((p) => p.seconds === seconds)?.label ?? formatDuration(seconds);
}

/**
 * missedFromProto reads a stored policy. An unset or zero max_delay means
 * "until the next run"; an unspecified action is the default, run late.
 */
export function missedFromProto(p: MissedRunPolicy | undefined): MissedState {
  return {
    action: p?.action === MissedRunAction.SKIP ? "skip" : "run_late",
    within: durationSeconds(p?.maxDelay),
  };
}

/** missedToProto writes the policy; "until the next run" leaves max_delay unset. */
export function missedToProto(m: MissedState): MessageInitShape<typeof MissedRunPolicySchema> {
  if (m.action === "skip") return { action: MissedRunAction.SKIP };
  return {
    action: MissedRunAction.RUN_LATE,
    maxDelay: m.within > 0 ? durationFromSeconds(m.within) : undefined,
  };
}

/** overlapPolicies explain what happens when a run is due while the last one runs. */
export const overlapPolicies: { value: OverlapPolicy; label: string; help: string }[] = [
  {
    value: OverlapPolicy.SKIP,
    label: "Skip it",
    help: "The new run is recorded as missed. Best for syncs, where the next run catches up anyway.",
  },
  {
    value: OverlapPolicy.QUEUE,
    label: "Wait, then run",
    help: "The new run starts when the last one finishes, if that is before its deadline.",
  },
  {
    value: OverlapPolicy.ALLOW,
    label: "Run both",
    help: "The new run starts at once, next to the one still going.",
  },
];

/** overlapLabel names an overlap policy. */
export function overlapLabel(p: OverlapPolicy): string {
  return overlapPolicies.find((x) => x.value === p)?.label ?? overlapPolicies[0].label;
}

/** missedSummary describes a missed-run policy in a few words. */
export function missedSummary(p: MissedRunPolicy | undefined): string {
  const m = missedFromProto(p);
  if (m.action === "skip") return "Skipped if the worker is offline";
  return m.within === 0
    ? "Runs late, until the next run is due"
    : `Runs late, within ${withinLabel(m.within).toLowerCase()}`;
}

/** TargetState is where the schedule's jobs run. */
export interface TargetState {
  kind: "worker" | "label";
  workerId: string;
  label: string;
}

/** ContactLink is one contact the schedule notifies, with its events. */
export interface ContactLink {
  contactId: string;
  events: NotificationEvent[];
}

/** ScheduleFormState is the schedule form. */
export interface ScheduleFormState {
  name: string;
  description: string;
  cron: string;
  timezone: string;
  target: TargetState;
  missed: MissedState;
  overlap: OverlapPolicy;
  enabled: boolean;
  contacts: ContactLink[];
  definition: JobDefinitionState;
}

/** ScheduleErrors are the form's problems, by section. */
export type ScheduleErrors = DefinitionErrors &
  Partial<Record<"name" | "cron" | "timezone" | "target" | "missed", string>> & {
    /** contacts holds errors by link index. */
    contacts?: Record<number, string>;
  };

/**
 * emptySchedule is a new schedule: daily at 02:30 in the browser's zone. A
 * kind and a repository start it as that kind of job on that repository.
 */
export function emptySchedule(
  timezone = browserTimeZone(),
  kind?: EditableKind,
  repositoryId?: string,
): ScheduleFormState {
  return {
    name: "",
    description: "",
    cron: "30 2 * * *",
    timezone,
    target: { kind: "worker", workerId: "", label: "" },
    missed: { action: "run_late", within: 6 * HOUR },
    overlap: OverlapPolicy.SKIP,
    enabled: true,
    contacts: [],
    definition: emptyDefinition(kind, repositoryId),
  };
}

function targetState(t: ScheduleTarget | undefined): TargetState {
  const v = t?.target;
  if (v?.case === "workerLabel") return { kind: "label", workerId: "", label: v.value };
  return { kind: "worker", workerId: v?.case === "workerId" ? v.value : "", label: "" };
}

/** scheduleState fills the form from a stored schedule (edit, or "duplicate"). */
export function scheduleState(s: Schedule): ScheduleFormState {
  return {
    name: s.name,
    description: s.description,
    cron: s.cron,
    timezone: s.timezone,
    target: targetState(s.target),
    missed: missedFromProto(s.missedRun),
    overlap: s.overlap || OverlapPolicy.SKIP,
    enabled: s.enabled,
    contacts: s.contacts.map((c) => ({ contactId: c.contactId, events: [...c.events] })),
    definition: definitionState(s.definition),
  };
}

/** scheduleFromJob starts a schedule that repeats a job ("Schedule this job"). */
export function scheduleFromJob(job: Job, timezone = browserTimeZone()): ScheduleFormState {
  return {
    ...emptySchedule(timezone),
    target: { kind: "worker", workerId: job.workerId, label: "" },
    definition: definitionState(job.definition),
  };
}

/** validateSchedule checks what the browser can check before sending. */
export function validateSchedule(s: ScheduleFormState): ScheduleErrors {
  const e: ScheduleErrors = validateDefinition(s.definition);
  if (!s.name.trim()) e.name = "Give the schedule a name.";
  else if (s.name.trim().length > 64) e.name = "Use at most 64 characters.";
  if (!s.cron.trim()) e.cron = "Enter when it runs, or pick a preset.";
  if (!s.timezone) e.timezone = "Choose a time zone.";
  if (s.target.kind === "worker" && !s.target.workerId)
    e.target = "Choose the worker that runs the jobs.";
  if (s.target.kind === "label" && !s.target.label.trim()) e.target = "Enter a worker label.";
  const single = singleWorkerReason(s.definition);
  if (s.target.kind === "label" && single) e.target = `${single}: choose a worker.`;
  if (s.missed.action === "run_late" && (s.missed.within < 0 || s.missed.within > MAX_WITHIN))
    e.missed = "A late run can wait at most 7 days.";
  const contactErrors: Record<number, string> = {};
  s.contacts.forEach((c, i) => {
    if (!c.events.length) contactErrors[i] = "Choose at least one event, or remove the contact.";
  });
  if (Object.keys(contactErrors).length) e.contacts = contactErrors;
  return e;
}

/**
 * toScheduleInput builds the API input from the form. os is the target
 * worker's OS when the target is one worker: what it cannot do (VSS off
 * Windows) is left out.
 */
export function toScheduleInput(
  s: ScheduleFormState,
  os?: string,
): MessageInitShape<typeof ScheduleInputSchema> {
  return {
    name: s.name.trim(),
    description: s.description.trim(),
    cron: s.cron.trim().replace(/\s+/g, " "),
    timezone: s.timezone,
    target: {
      target:
        s.target.kind === "label"
          ? { case: "workerLabel", value: s.target.label.trim().toLowerCase() }
          : { case: "workerId", value: s.target.workerId },
    },
    missedRun: missedToProto(s.missed),
    overlap: s.overlap,
    enabled: s.enabled,
    definition: toDefinition(forWorkerOS(s.definition, os)),
    contacts: s.contacts.map((c) => ({ contactId: c.contactId, events: c.events })),
  };
}

/** scheduleServerErrors places API violations on the form's sections. */
export function scheduleServerErrors(err: unknown): { errors: ScheduleErrors; rest: string[] } {
  const errors: ScheduleErrors = {};
  const rest: string[] = [];
  for (const v of fieldViolations(err)) {
    const f = v.field;
    const contact = /^contacts\[(\d+)\]/.exec(f);
    if (f === "name" || f === "cron" || f === "timezone") errors[f] = v.message;
    else if (f.startsWith("target")) errors.target = v.message;
    else if (f.startsWith("missed_run")) errors.missed = v.message;
    else if (contact) errors.contacts = { ...errors.contacts, [Number(contact[1])]: v.message };
    else if (f.startsWith("definition") && definitionError(v, errors)) continue;
    else rest.push(`${f}: ${v.message}`);
  }
  return { errors, rest };
}

/**
 * SINGLE_WORKER_REASON is why a two-way sync's schedule cannot target a
 * label (the controller's SingleWorker reason).
 */
export const SINGLE_WORKER_REASON = "A two-way sync keeps its state on one worker";

/**
 * REMOTE_SOURCE_SINGLE_WORKER_REASON is why the schedule of a backup of a
 * remote cannot target a label (the controller's SingleWorkerFor reason,
 * RemoteSourceSingleWorkerReason).
 */
export const REMOTE_SOURCE_SINGLE_WORKER_REASON =
  "A backup of a remote keeps its mirror or mount folder on one worker";

/**
 * singleWorkerReason says why a definition's schedule must target one
 * worker, not a label, or returns undefined when a label is fine: a
 * two-way sync keeps its state on the worker, and a backup of a remote
 * its mirror or mount folder (restic's parent snapshot follows the folder).
 */
export function singleWorkerReason(def: JobDefinitionState): string | undefined {
  if (def.kind === "rclone_bisync") return SINGLE_WORKER_REASON;
  if (def.kind === "restic_backup" && def.backup.source === "remote")
    return REMOTE_SOURCE_SINGLE_WORKER_REASON;
  return undefined;
}

/** labelTargetAllowed reports whether a schedule of the definition may run on any worker with a label. */
export function labelTargetAllowed(def: JobDefinitionState): boolean {
  return singleWorkerReason(def) === undefined;
}

/** TargetLabel describes a schedule's target for lists. */
export interface TargetLabel {
  kind: "worker" | "label";
  /** text is the worker's name or the label. */
  text: string;
  /** worker is the target worker, when the target is one that still exists. */
  worker?: Worker;
}

/** targetLabel describes where a schedule runs. */
export function targetLabel(
  t: ScheduleTarget | undefined,
  workers: Map<string, Worker>,
): TargetLabel {
  const v = t?.target;
  if (v?.case === "workerLabel") return { kind: "label", text: v.value };
  const w = v?.case === "workerId" ? workers.get(v.value) : undefined;
  return { kind: "worker", text: w?.name ?? "Removed worker", worker: w };
}
