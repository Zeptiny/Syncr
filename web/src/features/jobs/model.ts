import type {
  Job,
  JobDefinition,
  JobLocation,
  JobRepositoryRef,
  RcloneTransferDefinition,
} from "@/gen/syncr/v1/api_pb";
import { ResticRestoreOverwrite } from "@/gen/syncr/v1/link_pb";
import { durationSeconds, toNumber, tsDate } from "@/lib/format";
import { describeRetention } from "@/lib/retention";
import { isTerminal, transferModeLabel } from "@/lib/status";

import { bisyncOf } from "./bisync-definition";
import { retentionState } from "./restic-definition";

/** transferOf returns the job's transfer definition, if it is one. */
export function transferOf(job: Job): RcloneTransferDefinition | undefined {
  const op = job.definition?.operation;
  return op?.case === "rcloneTransfer" ? op.value : undefined;
}

/** EndpointLabel is a job location ready for display. */
export interface EndpointLabel {
  /** name is the remote's name; undefined for the worker filesystem. */
  name: string | undefined;
  backendType: string | undefined;
  path: string;
  deleted: boolean;
  local: boolean;
}

/** endpoint labels a job location using the job's remote refs. */
export function endpoint(job: Job, loc: JobLocation | undefined): EndpointLabel {
  if (!loc || !loc.remoteId) {
    return {
      name: undefined,
      backendType: undefined,
      path: loc?.path ?? "",
      deleted: false,
      local: true,
    };
  }
  const ref = job.remotes.find((r) => r.id === loc.remoteId);
  return {
    name: ref?.name ?? "Unknown remote",
    backendType: ref?.backendType,
    path: loc.path,
    deleted: ref?.deleted ?? false,
    local: false,
  };
}

/**
 * JobRoute is the two locations of a transfer or a two-way sync, ready for
 * display: from and to of a transfer, Folder A and Folder B of a two-way
 * sync.
 */
export interface JobRoute {
  a: EndpointLabel;
  b: EndpointLabel;
  aLabel: string;
  bLabel: string;
  /** twoWay marks a two-way sync, whose changes go both ways. */
  twoWay: boolean;
  /** mode names the operation: "Copy", "Sync", "Move" or "Two-way sync". */
  mode: string;
}

/**
 * jobRoute returns the locations of a transfer or a two-way sync, or
 * undefined for other kinds. It takes a job, or a schedule shaped as one
 * (its definition and remotes).
 */
export function jobRoute(job: Job): JobRoute | undefined {
  const t = transferOf(job);
  if (t)
    return {
      a: endpoint(job, t.source),
      b: endpoint(job, t.destination),
      aLabel: "From",
      bLabel: "To",
      twoWay: false,
      mode: transferModeLabel(t.mode),
    };
  const b = bisyncOf(job.definition);
  if (b)
    return {
      a: endpoint(job, b.path1),
      b: endpoint(job, b.path2),
      aLabel: "Folder A",
      bLabel: "Folder B",
      twoWay: true,
      mode: "Two-way sync",
    };
  return undefined;
}

/** ResticSummary describes a restic definition in two short lines. */
export interface ResticSummary {
  /** title is the one-line name, e.g. "Back up /srv/photos → Photos vault". */
  title: string;
  /** detail lists the options worth knowing, e.g. "keeps 7 daily; prunes". */
  detail: string;
}

/** overwriteLabel says what a restore does with files that are in the way. */
export function overwriteLabel(o: ResticRestoreOverwrite): string {
  switch (o) {
    case ResticRestoreOverwrite.ALWAYS:
      return "replaces all files";
    case ResticRestoreOverwrite.IF_CHANGED:
      return "replaces files that differ";
    case ResticRestoreOverwrite.IF_NEWER:
      return "replaces older files";
    default:
      return "keeps existing files";
  }
}

/**
 * resticSummary describes a restic definition (of a job or a schedule), or
 * returns undefined for a transfer. repo names its repository.
 */
export function resticSummary(
  def: JobDefinition | undefined,
  repo?: JobRepositoryRef,
): ResticSummary | undefined {
  const op = def?.operation;
  const name = repo?.name || "a repository";
  const parts = (...p: (string | false | undefined)[]) => p.filter(Boolean).join(", ");
  switch (op?.case) {
    case "resticBackup": {
      const d = op.value;
      const paths =
        d.paths.length > 2 ? `${d.paths[0]} and ${d.paths.length - 1} more` : d.paths.join(", ");
      return {
        title: `Back up ${paths || "files"} → ${name}`,
        detail: parts(
          d.tags.length > 0 && `tagged ${d.tags.join(", ")}`,
          d.excludes.length > 0 &&
            `${d.excludes.length} ${d.excludes.length === 1 ? "exclude" : "excludes"}`,
          d.useFsSnapshot && "volume snapshot (VSS)",
          d.excludeCaches && "skips caches",
          d.oneFileSystem && "one file system",
          d.host && `as host ${d.host}`,
          d.dryRun && "dry run",
        ),
      };
    }
    case "resticForget": {
      const d = op.value;
      return {
        title: `Apply retention to ${name}`,
        detail: parts(
          describeRetention(retentionState(d).policy).replace(/\.$/, ""),
          d.prune ? "prunes afterwards" : "does not prune",
          d.host && `host ${d.host}`,
          d.tags.length > 0 && `tagged ${d.tags.join(", ")}`,
          d.dryRun && "dry run",
        ),
      };
    }
    case "resticPrune":
      return { title: `Prune ${name}`, detail: op.value.dryRun ? "dry run" : "" };
    case "resticCheck": {
      const pct = op.value.readDataPercent;
      return {
        title: `Check ${name}`,
        detail:
          pct <= 0 ? "structure only" : pct >= 100 ? "reads all data" : `reads ${pct}% of the data`,
      };
    }
    case "resticRestore": {
      const d = op.value;
      // As the controller's titles: items of a subfolder are restored as
      // they were in it (back to the original location).
      const what =
        d.paths.length === 1
          ? d.paths[0]
          : d.paths.length > 1
            ? `${d.paths.length} items${d.subfolder ? ` of ${d.subfolder}` : ""}`
            : d.subfolder || `snapshot ${d.snapshotId.slice(0, 8)}`;
      return {
        title: `Restore ${what} from ${name} → ${d.target}`,
        detail: parts(
          overwriteLabel(d.overwrite),
          d.verify && "verifies the restored files",
          d.excludes.length > 0 &&
            `${d.excludes.length} ${d.excludes.length === 1 ? "exclude" : "excludes"}`,
        ),
      };
    }
    case "resticDeleteData":
      return { title: `Delete the data of ${name}`, detail: "" };
    default:
      return undefined;
  }
}

/**
 * jobTitle is a one-line description. A transfer is "Photos → B2 offsite";
 * every other kind uses the title the controller gives the job.
 */
export function jobTitle(job: Job): string {
  const t = transferOf(job);
  if (!t) return job.title || resticSummary(job.definition, job.repository)?.title || job.kind;
  const name = (e: EndpointLabel) => e.name ?? (e.path || "Worker filesystem");
  return `${name(endpoint(job, t.source))} → ${name(endpoint(job, t.destination))}`;
}

/** jobElapsed is how long the job has run (or ran), in seconds. */
export function jobElapsed(job: Job, now = Date.now()): number | undefined {
  const started = tsDate(job.startedAt);
  if (!started) return undefined;
  const finished = tsDate(job.finishedAt);
  if (finished) return (finished.getTime() - started.getTime()) / 1000;
  if (isTerminal(job.status)) return durationSeconds(job.stats?.elapsed) || undefined;
  return Math.max(0, (now - started.getTime()) / 1000);
}

/** jobProgress returns bytes done and total (0 if unknown). */
export function jobProgress(job: Job): {
  bytes: number;
  total: number;
  files: number;
  totalFiles: number;
} {
  return {
    bytes: toNumber(job.stats?.bytes),
    total: toNumber(job.stats?.totalBytes),
    files: toNumber(job.stats?.files),
    totalFiles: toNumber(job.stats?.totalFiles),
  };
}
