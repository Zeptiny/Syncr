import { useQuery } from "@connectrpc/connect-query";

import { UsageBar } from "@/components/app/usage-bar";
import { ProbeService } from "@/gen/syncr/v1/probe_pb";
import { formatBytes, toNumber } from "@/lib/format";

/** USAGE_STALE_MS is how long a remote's usage is reused: five minutes. */
const USAGE_STALE_MS = 5 * 60_000;

/**
 * RemoteUsage shows how much of a remote is used ("120 GB of 1 TB"), from
 * GetRemoteUsage, kept for five minutes. It shows nothing when the backend
 * cannot tell (unsupported), when asking failed, or for a remote on a
 * worker's own disk until that worker is chosen (local).
 */
export function RemoteUsage({
  remoteId,
  workerId,
  local,
  className,
}: {
  remoteId: string;
  workerId?: string;
  local?: boolean;
  className?: string;
}) {
  const q = useQuery(
    ProbeService.method.getRemoteUsage,
    { remoteId, workerId: workerId ?? "" },
    { enabled: !local || !!workerId, staleTime: USAGE_STALE_MS, retry: false },
  );
  const u = q.data?.usage;
  if (!u || u.unsupported) return null;
  if (u.used !== undefined && u.total !== undefined && u.total > 0n)
    return (
      <UsageBar
        used={toNumber(u.used)}
        total={toNumber(u.total)}
        label="Used"
        className={className ?? "w-56"}
      />
    );
  const text =
    u.used !== undefined
      ? `${formatBytes(u.used)} used`
      : u.free !== undefined
        ? `${formatBytes(u.free)} free`
        : undefined;
  return text ? <span className="tabular text-xs text-muted-foreground">{text}</span> : null;
}
