import { Cloud } from "lucide-react";

import type { ResticSnapshot } from "@/gen/syncr/v1/link_pb";
import { cn } from "@/lib/utils";

import { remoteOrigin, type RemoteNames } from "./snapshot-origin";

/**
 * RemoteBadge marks a snapshot of a remote with the remote it is of ("From
 * Nextcloud", or "From a remote" when the remote is not the user's), in
 * place of children (its paths, which are a worker's internal folder). For
 * a snapshot of folders it renders children.
 */
export function RemoteBadge({
  snapshot,
  names,
  className,
  children,
}: {
  snapshot: Pick<ResticSnapshot, "tags" | "paths">;
  names: RemoteNames;
  className?: string;
  children?: React.ReactNode;
}) {
  const from = remoteOrigin(snapshot, names);
  if (!from) return <>{children}</>;
  return (
    <span
      className={cn(
        "inline-flex max-w-full items-center gap-1 rounded-full bg-live-soft px-2 py-0.5 text-xs font-medium text-live",
        className,
      )}
      title={`Backed up from a folder of a remote. restic records the worker's folder ${snapshot.paths.join(", ")}.`}
    >
      <Cloud className="size-3 shrink-0" aria-hidden />
      <span className="truncate">{from}</span>
    </span>
  );
}
