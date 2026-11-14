// Job drafts: a job definition prepared elsewhere (the file browser, the
// transfer sheet) and handed to "Run a job" or "New schedule" to finish.
// A draft travels in the router's history state, never in the URL: it can
// be long (filters) and it is nobody's link to share. A missing or invalid
// draft leaves the form empty.
import {
  create,
  fromJson,
  toJson,
  type JsonValue,
  type MessageInitShape,
} from "@bufbuild/protobuf";

import type { SelectedItem } from "@/components/app/file-browser/selection";
import { JobDefinitionSchema, type JobDefinition, type Worker } from "@/gen/syncr/v1/api_pb";
import { FilterAction, ProbeEntryType, ResticRemoteSourceMethod } from "@/gen/syncr/v1/link_pb";
import { parseFilterLine } from "@/lib/rclone-filters";
import { methodProblem } from "@/lib/remote-sources";

import { definitionState, type EditableKind } from "../jobs/definition";
import { formKind } from "../jobs/search";
import type { RunJobState } from "../jobs/run-job-page";
import { emptySchedule, type ScheduleFormState } from "../schedules/model";
import { transferSource, type Place } from "./transfer";

/** JobDraft is a job for a form to fill in: its kind, definition and worker. */
export interface JobDraft {
  kind: EditableKind;
  /** definition is the JobDefinition in protobuf JSON, so the history can store it. */
  definition: JsonValue;
  /** workerId proposes the worker that runs it ("" lets the user choose). */
  workerId: string;
}

declare module "@tanstack/history" {
  interface HistoryState {
    /** jobDraft prefills /jobs/new or /schedules/new (see job-drafts.ts). */
    jobDraft?: JobDraft;
  }
}

const cases: Record<EditableKind, JobDefinition["operation"]["case"]> = {
  rclone_transfer: "rcloneTransfer",
  rclone_bisync: "rcloneBisync",
  restic_backup: "resticBackup",
  restic_forget: "resticForget",
  restic_check: "resticCheck",
  restic_prune: "resticPrune",
};

/** kindOf is the form kind of a definition, or undefined for one no form edits. */
function kindOf(def: JobDefinition): EditableKind | undefined {
  return (Object.keys(cases) as EditableKind[]).find((k) => cases[k] === def.operation.case);
}

/** makeJobDraft builds a draft from a definition (its kind follows the definition). */
export function makeJobDraft(
  definition: MessageInitShape<typeof JobDefinitionSchema>,
  workerId = "",
): JobDraft {
  const def = create(JobDefinitionSchema, definition);
  const kind = kindOf(def);
  if (!kind) throw new Error(`a draft cannot hold a ${def.operation.case ?? "blank"} job`);
  return { kind, definition: toJson(JobDefinitionSchema, def), workerId };
}

/** ParsedDraft is a draft read back: its definition is a message again. */
export interface ParsedDraft {
  kind: EditableKind;
  definition: JobDefinition;
  workerId: string;
}

/**
 * readJobDraft reads a draft from history state, or returns undefined when
 * there is none or it is not a valid draft of a kind a form edits.
 */
export function readJobDraft(state: unknown): ParsedDraft | undefined {
  if (typeof state !== "object" || state === null) return undefined;
  const d = (state as { jobDraft?: unknown }).jobDraft;
  if (typeof d !== "object" || d === null) return undefined;
  const { kind, definition, workerId } = d as Record<string, unknown>;
  const k = formKind(kind);
  if (!k || (workerId !== undefined && typeof workerId !== "string")) return undefined;
  try {
    const def = fromJson(JobDefinitionSchema, definition as JsonValue);
    if (kindOf(def) !== k) return undefined;
    return { kind: k, definition: def, workerId: workerId ?? "" };
  } catch {
    return undefined;
  }
}

/** draftRunJob fills the Run job form from a draft. */
export function draftRunJob(d: ParsedDraft): RunJobState {
  return { workerId: d.workerId, definition: definitionState(d.definition) };
}

/** draftSchedule fills a new schedule from a draft: on the draft's worker, at the default time. */
export function draftSchedule(d: ParsedDraft, timezone?: string): ScheduleFormState {
  return {
    ...emptySchedule(timezone),
    target: { kind: "worker", workerId: d.workerId, label: "" },
    definition: definitionState(d.definition),
  };
}

/**
 * DraftTarget is what the file browser's "New job" menu acts on: the
 * selection's uncovered items or, with none, the open folder.
 */
export interface DraftTarget {
  place: Place;
  items: SelectedItem[];
  /** parent is the items' common parent (SelectionContext.parent). */
  parent: string | undefined;
  /** folder is the open folder ("" for a worker's list of folders or a remote's root). */
  folder: string;
  /** workerId proposes the worker of the job: the place's worker, or the listing one. */
  workerId: string;
  /** worker is the place's worker (for its allowed folders). */
  worker?: Worker;
}

/**
 * draftFolder is the one folder a target names: the single selected
 * folder, or the open one when nothing is selected. A worker's list of
 * folders is not a folder; several items or a file name none.
 */
export function draftFolder(t: DraftTarget): string | undefined {
  if (t.items.length === 0)
    return t.place.kind === "worker" && t.folder === "" ? undefined : t.folder;
  const [only] = t.items;
  return t.items.length === 1 && only.entry.type === ProbeEntryType.DIR ? only.path : undefined;
}

const loc = (p: Place, path: string) => ({
  remoteId: p.kind === "remote" ? p.remoteId : "",
  path,
});

/** backupDraft backs up the selected paths (or the open folder) of a worker. */
export function backupDraft(t: DraftTarget): JobDraft | undefined {
  if (t.place.kind !== "worker") return undefined;
  const paths = t.items.length ? t.items.map((i) => i.path) : t.folder ? [t.folder] : [];
  if (!paths.length) return undefined;
  return makeJobDraft(
    { operation: { case: "resticBackup", value: { repositoryId: "", paths } } },
    t.workerId,
  );
}

/**
 * remoteBackupDraft backs up the target's folder of a remote (the open
 * folder, or the one folder selected) with restic, through a mirror, on
 * the worker that listed it when workers (if given) says it can mirror. A
 * Local directory remote is a folder on a worker: back that up instead.
 */
export function remoteBackupDraft(t: DraftTarget, workers?: Worker[]): JobDraft | undefined {
  if (t.place.kind !== "remote" || t.place.local) return undefined;
  const folder = draftFolder(t);
  if (folder === undefined) return undefined;
  // The listing worker, unless it cannot keep a mirror (outdated, or
  // mirrors off): then the editor asks for one.
  const lister = workers?.find((w) => w.id === t.workerId);
  const workerId =
    lister && methodProblem(lister, ResticRemoteSourceMethod.MIRROR) ? "" : t.workerId;
  return makeJobDraft(
    {
      operation: {
        case: "resticBackup",
        value: {
          repositoryId: "",
          remoteSource: {
            remoteId: t.place.remoteId,
            path: folder.replace(/^\/+|\/+$/g, ""),
            method: ResticRemoteSourceMethod.MIRROR,
          },
        },
      },
    },
    workerId,
  );
}

/**
 * transferDraft copies the selection (its common parent with filters that
 * keep only the items) or the open folder somewhere still to choose.
 */
export function transferDraft(t: DraftTarget): JobDraft | undefined {
  let source: { path: string; filters: string[] };
  if (t.items.length === 0) {
    const folder = draftFolder(t);
    if (folder === undefined) return undefined;
    source = { path: folder, filters: [] };
  } else {
    const r = transferSource({ place: t.place, items: t.items, parent: t.parent }, t.worker);
    if ("error" in r) return undefined;
    source = r;
  }
  return makeJobDraft(
    {
      operation: {
        case: "rcloneTransfer",
        value: {
          source: loc(t.place, source.path),
          destination: { remoteId: "", path: "" },
          options: {
            // A selected empty folder must arrive too.
            createEmptySrcDirs: source.filters.length > 0,
            filters: source.filters.map((l) => {
              const f = parseFilterLine(l);
              return {
                action: f.include ? FilterAction.INCLUDE : FilterAction.EXCLUDE,
                pattern: f.pattern,
              };
            }),
          },
        },
      },
    },
    t.workerId,
  );
}

/** bisyncDraft keeps the target's folder in step with another one (Folder A). */
export function bisyncDraft(t: DraftTarget): JobDraft | undefined {
  const folder = draftFolder(t);
  if (folder === undefined) return undefined;
  return makeJobDraft(
    { operation: { case: "rcloneBisync", value: { path1: loc(t.place, folder) } } },
    t.workerId,
  );
}
