import { Link } from "@tanstack/react-router";
import { Archive, CircleCheck, TriangleAlert } from "lucide-react";

import { KeyValueList } from "@/components/app/key-value-list";
import { Panel } from "@/components/app/panel";
import { ShortId } from "@/components/app/short-id";
import type { Job } from "@/gen/syncr/v1/api_pb";
import type {
  ResticBackupOutput,
  ResticCheckOutput,
  ResticDeleteDataOutput,
  ResticForgetOutput,
  ResticRestoreOutput,
} from "@/gen/syncr/v1/link_pb";
import { formatBytes, formatCount, toNumber } from "@/lib/format";
import { cn } from "@/lib/utils";

import { resticSummary } from "./model";

function Figure({ label, value, sub }: { label: string; value: React.ReactNode; sub?: string }) {
  return (
    <div className="min-w-0">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className="tabular mt-0.5 truncate text-lg leading-7 font-semibold">{value}</div>
      {sub && <div className="tabular truncate text-xs text-muted-foreground">{sub}</div>}
    </div>
  );
}

/** ShareBar splits a total into labelled parts, drawn as one bar with a legend. */
function ShareBar({
  parts,
  label,
}: {
  label: string;
  parts: { label: string; value: number; className: string }[];
}) {
  const total = parts.reduce((n, p) => n + p.value, 0);
  return (
    <div className="flex flex-col gap-2">
      <div
        role="img"
        aria-label={`${label}: ${parts.map((p) => `${formatCount(p.value)} ${p.label.toLowerCase()}`).join(", ")}`}
        className="flex h-2 w-full gap-px overflow-hidden rounded-full bg-muted"
      >
        {total > 0 &&
          parts
            .filter((p) => p.value > 0)
            .map((p) => (
              <span
                key={p.label}
                className={cn("h-full", p.className)}
                style={{ width: `${Math.max(1, (p.value / total) * 100)}%` }}
              />
            ))}
      </div>
      <div className="flex flex-wrap gap-x-5 gap-y-1 text-[13px]">
        {parts.map((p) => (
          <span key={p.label} className="inline-flex items-center gap-1.5">
            <span className={cn("size-2 rounded-full", p.className)} aria-hidden />
            <span className="tabular font-medium">{formatCount(p.value)}</span>
            <span className="text-muted-foreground">{p.label.toLowerCase()}</span>
          </span>
        ))}
      </div>
    </div>
  );
}

/** BackupSummaryCard shows what a backup saved and links to its snapshot. */
export function BackupSummaryCard({ out, job }: { out: ResticBackupOutput; job: Job }) {
  const repo = job.repository;
  return (
    <Panel
      title="Backup summary"
      actions={
        out.snapshotId && repo && !repo.deleted ? (
          <Link
            to="/repositories/$repositoryId/snapshots/$snapshotId"
            params={{ repositoryId: repo.id, snapshotId: out.snapshotId }}
            className="inline-flex items-center gap-1.5 text-[13px] font-medium text-primary hover:underline"
          >
            <Archive className="size-3.5" aria-hidden />
            Browse snapshot {out.snapshotId.slice(0, 8)}
          </Link>
        ) : undefined
      }
    >
      <div className="flex flex-col gap-5">
        <div className="grid grid-cols-2 gap-4 md:grid-cols-4">
          <Figure
            label="Snapshot"
            value={out.snapshotId ? <ShortId id={out.snapshotId} copy /> : "None (dry run)"}
          />
          <Figure
            label="Processed"
            value={formatBytes(out.totalBytesProcessed)}
            sub={`${formatCount(out.totalFilesProcessed)} files`}
          />
          <Figure
            label="Data added"
            value={formatBytes(out.dataAddedBytes)}
            sub="before compression"
          />
          <Figure
            label="Stored"
            value={formatBytes(out.dataAddedPackedBytes)}
            sub="added to the repository"
          />
        </div>
        <ShareBar
          label="Files"
          parts={[
            { label: "New", value: toNumber(out.filesNew), className: "bg-live" },
            { label: "Changed", value: toNumber(out.filesChanged), className: "bg-warning" },
            {
              label: "Unmodified",
              value: toNumber(out.filesUnmodified),
              className: "bg-neutral/50",
            },
          ]}
        />
      </div>
    </Panel>
  );
}

/** RetentionCard shows what a retention job removed and kept. */
export function RetentionCard({ out }: { out: ResticForgetOutput }) {
  const removed = out.removedSnapshotIds;
  return (
    <Panel title="Retention result">
      <div className="flex flex-col gap-4">
        <div className="grid grid-cols-2 gap-4 md:grid-cols-4">
          <Figure label="Removed" value={formatCount(removed.length)} sub="snapshots" />
          <Figure label="Kept" value={formatCount(out.keptCount)} sub="snapshots" />
        </div>
        {removed.length > 0 ? (
          <div className="flex flex-col gap-1.5">
            <span className="text-xs text-muted-foreground">Removed snapshots</span>
            <ul className="flex flex-wrap gap-1.5">
              {removed.slice(0, 60).map((id) => (
                <li key={id} className="rounded bg-muted px-1.5 py-0.5">
                  <ShortId id={id} />
                </li>
              ))}
              {removed.length > 60 && (
                <li className="px-1.5 py-0.5 text-[13px] text-muted-foreground">
                  and {removed.length - 60} more
                </li>
              )}
            </ul>
          </div>
        ) : (
          <p className="text-[13px] text-muted-foreground">
            Every snapshot matched a rule, so nothing was removed.
          </p>
        )}
      </div>
    </Panel>
  );
}

/** CheckCard shows what a check found. */
export function CheckCard({ out }: { out: ResticCheckOutput }) {
  const errors = toNumber(out.numErrors);
  const read =
    out.readDataPercent <= 0
      ? "Structure only"
      : out.readDataPercent >= 100
        ? "All data read"
        : `${out.readDataPercent}% of the data read`;
  return (
    <Panel title="Check result">
      <div className="flex flex-col gap-3">
        <div
          className={cn(
            "flex items-center gap-2 text-[15px] font-semibold",
            errors > 0 ? "text-danger" : "text-success",
          )}
        >
          {errors > 0 ? (
            <TriangleAlert className="size-4" aria-hidden />
          ) : (
            <CircleCheck className="size-4" aria-hidden />
          )}
          {errors > 0
            ? `${formatCount(errors)} ${errors === 1 ? "error" : "errors"} found`
            : "No errors found"}
        </div>
        <p className="text-[13px] text-muted-foreground">{read}.</p>
        {(out.suggestRepairIndex || out.suggestPrune) && (
          <ul className="flex list-disc flex-col gap-1 pl-5 text-[13px]">
            {out.suggestRepairIndex && <li>restic suggests repairing the index.</li>}
            {out.suggestPrune && <li>restic suggests running a prune.</li>}
          </ul>
        )}
        {out.brokenPacks.length > 0 && (
          <div className="flex flex-col gap-1.5">
            <span className="text-xs text-muted-foreground">Damaged pack files</span>
            <ul className="flex flex-wrap gap-1.5">
              {out.brokenPacks.slice(0, 40).map((id) => (
                <li key={id} className="rounded bg-danger-soft px-1.5 py-0.5">
                  <ShortId id={id} />
                </li>
              ))}
            </ul>
          </div>
        )}
      </div>
    </Panel>
  );
}

/** RestoreCard shows what a restore wrote and what it left alone. */
export function RestoreCard({ out }: { out: ResticRestoreOutput }) {
  return (
    <Panel title="Restore result">
      <div className="flex flex-col gap-5">
        <div className="grid grid-cols-2 gap-4 md:grid-cols-4">
          <Figure
            label="Restored"
            value={formatBytes(out.bytesRestored)}
            sub={`${formatCount(out.filesRestored)} files`}
          />
          <Figure
            label="Left as they were"
            value={formatCount(out.filesSkipped)}
            sub={`${formatBytes(out.bytesSkipped)} already there`}
          />
          <Figure
            label="In the selection"
            value={formatBytes(out.totalBytes)}
            sub={`${formatCount(out.totalFiles)} files`}
          />
        </div>
        <ShareBar
          label="Files"
          parts={[
            { label: "Restored", value: toNumber(out.filesRestored), className: "bg-success" },
            { label: "Skipped", value: toNumber(out.filesSkipped), className: "bg-neutral/50" },
          ]}
        />
      </div>
    </Panel>
  );
}

/** DeleteDataCard shows what deleting a repository's data removed. */
export function DeleteDataCard({ out }: { out: ResticDeleteDataOutput }) {
  return (
    <Panel title="Deletion result">
      {out.alreadyGone ? (
        <p className="text-[13px]">
          The repository's files were already gone, so there was nothing left to delete.
        </p>
      ) : (
        <div className="flex flex-col gap-4">
          <div className="grid grid-cols-2 gap-4 md:grid-cols-4">
            <Figure label="Objects deleted" value={formatCount(out.deletedObjects)} />
            <Figure
              label="Parts removed"
              value={formatCount(out.removedParts.length)}
              sub={out.removedParts.join(", ") || undefined}
            />
          </div>
          <p className="text-[13px] text-muted-foreground">
            {out.directoryRemoved
              ? "The repository's folder was removed."
              : "The folder was kept: other files remain in it, or it is the remote's root."}
          </p>
        </div>
      )}
    </Panel>
  );
}

/**
 * JobOutputCards renders what a finished job reported: the card of its
 * kind, then the controller's summary lines for any kind.
 */
export function JobOutputCards({ job }: { job: Job }) {
  const out = job.output?.output;
  return (
    <>
      {out?.case === "resticBackup" && <BackupSummaryCard out={out.value} job={job} />}
      {out?.case === "resticForget" && <RetentionCard out={out.value} />}
      {out?.case === "resticCheck" && <CheckCard out={out.value} />}
      {out?.case === "resticRestore" && <RestoreCard out={out.value} />}
      {out?.case === "resticDeleteData" && <DeleteDataCard out={out.value} />}
      {job.details.length > 0 && (
        <Panel title="Summary">
          <KeyValueList items={job.details.map((d) => ({ label: d.label, value: d.value }))} />
        </Panel>
      )}
    </>
  );
}

/** ResticBand says what a restic job does, where a transfer shows its two ends. */
export function ResticBand({ job, worker }: { job: Job; worker?: string }) {
  const summary = resticSummary(job.definition, job.repository);
  const repo = job.repository;
  if (!summary) return null;
  return (
    <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg border bg-card px-4 py-3.5">
      <div className="min-w-0">
        <div className="text-xs text-muted-foreground">
          {worker ? `On ${worker}` : "On a removed worker"}
        </div>
        <div className="mt-1 font-semibold break-words">{job.title || summary.title}</div>
        {summary.detail && (
          <div className="mt-0.5 text-[13px] text-muted-foreground">{summary.detail}</div>
        )}
      </div>
      {repo &&
        (repo.deleted ? (
          <span className="text-[13px] text-muted-foreground">
            {repo.name} <span className="text-xs">(repository removed)</span>
          </span>
        ) : (
          <Link
            to="/repositories/$repositoryId"
            params={{ repositoryId: repo.id }}
            className="inline-flex items-center gap-1.5 text-[13px] font-medium text-primary hover:underline"
          >
            <Archive className="size-3.5" aria-hidden />
            {repo.name}
          </Link>
        ))}
    </div>
  );
}
