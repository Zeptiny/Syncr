import { ArchiveRestore, FolderOpen, TriangleAlert } from "lucide-react";

import { Button } from "@/components/ui/button";
import { ProbeEntryType, type ResticSnapshot } from "@/gen/syncr/v1/link_pb";
import { formatBytes, formatDateTime, tsDate } from "@/lib/format";
import { cn } from "@/lib/utils";

import { shortId } from "./model";
import { folderOf, type FoundPath } from "./scans";

/** VersionListProps configures VersionList. */
export interface VersionListProps {
  found: FoundPath;
  /** snapshots are the repository's, for the snapshots' times. */
  snapshots: readonly ResticSnapshot[];
  /** onOpen opens a folder in a snapshot. */
  onOpen: (snapshotId: string, folder: string) => void;
  /** onRestore restores the path as one snapshot holds it. */
  onRestore: (snapshotId: string, path: string) => void;
  /** locked disables restores (the repository is being deleted). */
  locked?: boolean;
  className?: string;
}

const count = (n: number) => (n === 1 ? "1 snapshot" : `${n.toLocaleString("en-US")} snapshots`);

/**
 * VersionList lists the versions of one path a search found: each
 * distinct size and modification time, newest first, with the snapshots
 * holding it. A version opens its folder in, or restores from, the newest
 * snapshot holding it; the list of snapshots opens any of them.
 */
export function VersionList({
  found,
  snapshots,
  onOpen,
  onRestore,
  locked,
  className,
}: VersionListProps) {
  const byId = new Map(snapshots.map((s) => [s.id, s]));
  const at = (id: string) => byId.get(id);
  const folder = folderOf(found.path);
  const dir = found.entry.type === ProbeEntryType.DIR;
  const label = (i: number) =>
    dir ? "Folder" : i === 0 ? "Newest version" : `Version ${found.versions.length - i}`;

  return (
    <ol
      className={cn("@container flex flex-col divide-y rounded-md border", className)}
      aria-label="Versions"
    >
      {found.versions.map((v, i) => {
        const newest = v.snapshotIds[0];
        const oldest = v.snapshotIds.at(-1)!;
        const range = [at(newest)?.time, at(oldest)?.time].map((t) => formatDateTime(tsDate(t)));
        return (
          <li key={v.key} className="flex flex-col gap-2 px-3 py-2.5 @xl:flex-row @xl:items-start">
            <div className="flex min-w-0 flex-1 flex-col gap-0.5">
              <span className="text-[13px] font-medium">
                {label(i)}
                {!dir && (
                  <span className="tabular ml-2 font-normal text-muted-foreground">
                    {formatBytes(v.entry.size)}
                    {v.entry.modTime && <>, modified {formatDateTime(tsDate(v.entry.modTime))}</>}
                  </span>
                )}
              </span>
              <details className="group text-xs text-muted-foreground">
                <summary className="w-fit cursor-pointer rounded hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none">
                  In {count(v.snapshotIds.length)}
                  {at(newest) &&
                    (v.snapshotIds.length === 1 ? `, ${range[0]}` : `, ${range[1]} to ${range[0]}`)}
                </summary>
                <ul className="mt-1 flex flex-wrap gap-1">
                  {v.snapshotIds.map((id) => (
                    <li key={id}>
                      <button
                        type="button"
                        className="rounded border px-1.5 py-0.5 font-mono text-[11.5px] hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
                        title={`Open ${folder} in snapshot ${shortId(id)}`}
                        onClick={() => onOpen(id, folder)}
                      >
                        {shortId(id)}
                        {at(id) && (
                          <span className="ml-1 font-sans text-muted-foreground">
                            {formatDateTime(tsDate(at(id)?.time))}
                          </span>
                        )}
                      </button>
                    </li>
                  ))}
                </ul>
              </details>
            </div>
            <div className="flex shrink-0 flex-wrap gap-2">
              <Button
                variant="outline"
                size="sm"
                onClick={() => onOpen(newest, folder)}
                aria-label={`Open folder in snapshot ${shortId(newest)}`}
                title={`Open ${folder} in snapshot ${shortId(newest)}`}
              >
                <FolderOpen aria-hidden />
                Open folder
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={locked}
                onClick={() => onRestore(newest, found.path)}
                aria-label={`Restore… from snapshot ${shortId(newest)}`}
              >
                <ArchiveRestore aria-hidden />
                Restore…
              </Button>
            </div>
          </li>
        );
      })}
    </ol>
  );
}

/** FindNotesProps configures FindNotes. */
export interface FindNotesProps {
  /** found says what the search found: "12 matches", "3 versions in 28 snapshots". */
  found: string;
  truncated: boolean;
  complete: boolean;
  searched: number;
  /** total is how many snapshots the search was asked to cover. */
  total?: number;
  className?: string;
}

/**
 * FindNotes says what a search covered: how many snapshots it searched,
 * and whether it was cut at its most matches or stopped at the time limit
 * (older snapshots are searched last, so those are what is missing).
 */
export function FindNotes({
  found,
  truncated,
  complete,
  searched,
  total,
  className,
}: FindNotesProps) {
  const of = total && total >= searched ? ` of ${total.toLocaleString("en-US")}` : "";
  const notes: string[] = [];
  if (truncated)
    notes.push(
      "The search stopped at its most matches, so older snapshots may hold more. Narrow the pattern or search one snapshot.",
    );
  else if (!complete)
    notes.push(
      "The search stopped at the time limit, so older snapshots were not all searched. Narrow the pattern or search one snapshot.",
    );
  return (
    <div className={cn("flex flex-col gap-1 text-[13px]", className)} role="status">
      <span className="text-muted-foreground">
        {found}. {searched.toLocaleString("en-US")}
        {of} {searched === 1 && !of ? "snapshot" : "snapshots"} searched.
      </span>
      {notes.map((n) => (
        <span key={n} className="flex items-start gap-1.5 text-warning">
          <TriangleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden />
          {n}
        </span>
      ))}
    </div>
  );
}
