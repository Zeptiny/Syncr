import { useQuery } from "@connectrpc/connect-query";
import { Layers, RefreshCw } from "lucide-react";

import { EmptyState } from "@/components/app/empty-state";
import { EntryIcon } from "@/components/app/entry-icon";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import type { ProbeEntry, ResticSnapshot } from "@/gen/syncr/v1/link_pb";
import { ResticService } from "@/gen/syncr/v1/restic_pb";
import { scanErrorText } from "@/lib/scans";

import { exactPathPattern, groupMatches, onlyPath } from "./scans";
import { FindNotes, VersionList } from "./version-list";

/** FileVersionsSheetProps configures FileVersionsSheet. */
export interface FileVersionsSheetProps {
  /** entry is the file or link whose versions are shown (never a folder); undefined closes the sheet. */
  entry: ProbeEntry | undefined;
  onClose: () => void;
  repositoryId: string;
  /** snapshots are the repository's, for the snapshots' times. */
  snapshots: readonly ResticSnapshot[];
  /** workerId pins the worker that searches (the browser's listing worker). */
  workerId?: string;
  onOpen: (snapshotId: string, folder: string) => void;
  onRestore: (snapshotId: string, path: string) => void;
  locked?: boolean;
}

/**
 * FileVersionsSheet shows every version of one file or link across the
 * repository's snapshots ("Show versions"): it searches all snapshots for
 * the exact path (an anchored, escaped pattern) and keeps only that path's
 * matches (a folder of the same path in another snapshot would bring
 * everything inside it). Folders are not offered: their versions mean
 * little, and their subtrees would use up the search's matches.
 */
export function FileVersionsSheet(props: FileVersionsSheetProps) {
  const { entry, onClose } = props;
  return (
    <Sheet open={!!entry} onOpenChange={(o) => !o && onClose()}>
      <SheetContent side="right" className="w-full gap-0 overflow-y-auto sm:max-w-[560px]">
        {entry && <Body {...props} entry={entry} />}
      </SheetContent>
    </Sheet>
  );
}

function Body({
  entry,
  repositoryId,
  snapshots,
  workerId,
  onOpen,
  onRestore,
  locked,
}: FileVersionsSheetProps & { entry: ProbeEntry }) {
  const q = useQuery(
    ResticService.method.findInRepository,
    {
      repositoryId,
      patterns: [exactPathPattern(entry.path)],
      ignoreCase: false,
      snapshotIds: [],
      workerId: workerId ?? "",
    },
    { retry: false, staleTime: Infinity },
  );
  const data = q.data;
  const found = data ? groupMatches(onlyPath(data.matches, entry.path), snapshots)[0] : undefined;
  const versions = found?.versions.length ?? 0;

  return (
    <>
      <SheetHeader className="border-b px-5 py-4">
        <SheetTitle className="flex items-center gap-2">
          <span aria-hidden>
            <EntryIcon type={entry.type} />
          </span>
          Versions of {entry.name}
        </SheetTitle>
        <SheetDescription className="font-mono text-xs break-all">{entry.path}</SheetDescription>
      </SheetHeader>
      <div className="flex flex-col gap-4 px-5 py-4">
        {q.isFetching && (
          <div className="flex flex-col gap-3" role="status">
            <span className="text-[13px] text-muted-foreground">
              Searching every snapshot. This can take up to a minute.
            </span>
            <Skeleton className="h-16 w-full" />
            <Skeleton className="h-16 w-full" />
          </div>
        )}
        {!q.isFetching && q.error && (
          <div className="flex flex-col items-start gap-3">
            <p role="alert" className="text-[13px] text-danger">
              {scanErrorText(q.error)}
            </p>
            <Button variant="outline" size="sm" onClick={() => void q.refetch()}>
              <RefreshCw aria-hidden />
              Try again
            </Button>
          </div>
        )}
        {!q.isFetching && data && (
          <>
            <FindNotes
              found={
                found
                  ? `${versions === 1 ? "1 version" : `${versions} versions`} in ${found.snapshotIds.length === 1 ? "1 snapshot" : `${found.snapshotIds.length} snapshots`}`
                  : "No versions"
              }
              truncated={data.truncated}
              complete={data.complete}
              searched={data.snapshotsSearched}
              total={snapshots.length || undefined}
            />
            {found ? (
              <VersionList
                found={found}
                snapshots={snapshots}
                onOpen={onOpen}
                onRestore={onRestore}
                locked={locked}
              />
            ) : (
              <EmptyState
                icon={Layers}
                title="No snapshot holds this path"
                description="The snapshots searched don't have it."
                className="py-8"
              />
            )}
          </>
        )}
      </div>
    </>
  );
}
