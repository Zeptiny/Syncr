import { useQuery } from "@connectrpc/connect-query";
import { Link, useNavigate } from "@tanstack/react-router";

import { ErrorState } from "@/components/app/error-state";
import { FileBrowser } from "@/components/app/file-browser/file-browser";
import { copyPathItem } from "@/components/app/file-browser/helpers";
import { useBrowserSelection } from "@/components/app/file-browser/use-selection";
import { PageHeader } from "@/components/app/page-header";
import { StatusBadge } from "@/components/app/status-badge";
import { Skeleton } from "@/components/ui/skeleton";
import { WorkerAdminService } from "@/gen/syncr/v1/api_pb";
import { sourceKey, type PathSource } from "@/hooks/use-path-listing";
import { normalizePath } from "@/lib/paths";

/**
 * WorkerFilesPage browses a worker's allowed folders: the list of them at
 * the top, then a folder at a time. The open folder is in the URL.
 */
export function WorkerFilesPage({ workerId, path }: { workerId: string; path?: string }) {
  const navigate = useNavigate();
  const wq = useQuery(WorkerAdminService.method.getWorker, { workerId });
  const source: PathSource = { kind: "worker", workerId };
  const [selection, setSelection] = useBrowserSelection(sourceKey(source));
  const open = path ? normalizePath(path, "worker") : "";

  if (wq.error) return <ErrorState error={wq.error} onRetry={() => void wq.refetch()} />;
  const w = wq.data?.worker;
  if (!w)
    return (
      <div className="flex flex-col gap-4">
        <Skeleton className="h-8 w-72" />
        <Skeleton className="h-96 w-full" />
      </div>
    );

  return (
    <>
      <PageHeader
        back={{ to: "/workers", label: "Workers" }}
        title={`Files on ${w.name}`}
        meta={
          <>
            <StatusBadge worker={w.status} />
            <Link
              to="/workers/$workerId"
              params={{ workerId }}
              className="text-[13px] font-medium text-primary hover:underline"
            >
              Worker details
            </Link>
          </>
        }
        description="Only the worker's allowed folders can be opened."
      />
      <FileBrowser
        source={source}
        path={open}
        onNavigate={(to) =>
          void navigate({
            to: "/workers/$workerId/files",
            params: { workerId },
            search: { path: to || undefined },
          })
        }
        selection={selection}
        onSelectionChange={setSelection}
        rootLabel="Allowed folders"
        rowMenu={(e) => [copyPathItem(e)]}
      />
    </>
  );
}
