import { useQuery } from "@connectrpc/connect-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { FolderInput } from "lucide-react";

import { ErrorState } from "@/components/app/error-state";
import { FileBrowser } from "@/components/app/file-browser/file-browser";
import { copyPathItem } from "@/components/app/file-browser/helpers";
import { commonParent, uncovered } from "@/components/app/file-browser/selection";
import { useBrowserSelection } from "@/components/app/file-browser/use-selection";
import { PageHeader } from "@/components/app/page-header";
import { StatusBadge } from "@/components/app/status-badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { WorkerAdminService } from "@/gen/syncr/v1/api_pb";
import { useFolderMeasure } from "@/hooks/use-folder-measure";
import { sourceKey, type PathSource } from "@/hooks/use-path-listing";
import { normalizePath } from "@/lib/paths";

import { JobDraftMenu } from "./job-draft-menu";
import type { Place } from "./transfer";
import { TransferActionsHost } from "./transfer-actions-host";
import { useTransferActions } from "./use-transfer-actions";

/**
 * WorkerFilesPage browses a worker's allowed folders: the list of them at
 * the top, then a folder at a time. The open folder is in the URL. A
 * selection can be copied or moved, and jobs started from it.
 */
export function WorkerFilesPage({ workerId, path }: { workerId: string; path?: string }) {
  const navigate = useNavigate();
  const wq = useQuery(WorkerAdminService.method.getWorker, { workerId });
  const source: PathSource = { kind: "worker", workerId };
  const [selection, setSelection] = useBrowserSelection(sourceKey(source));
  const open = path ? normalizePath(path, "worker") : "";
  const place: Place = { kind: "worker", workerId };
  const transfer = useTransferActions(place);
  const measure = useFolderMeasure(source, open);

  if (wq.error) return <ErrorState error={wq.error} onRetry={() => void wq.refetch()} />;
  const w = wq.data?.worker;
  if (!w)
    return (
      <div className="flex flex-col gap-4">
        <Skeleton className="h-8 w-72" />
        <Skeleton className="h-96 w-full" />
      </div>
    );

  const items = uncovered(selection, "worker");
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
        measure={measure}
        rowMenu={(e) => [transfer.rowItem(e), copyPathItem(e)]}
        toolbarActions={
          <JobDraftMenu
            target={{
              place,
              items,
              parent: commonParent(
                items.map((i) => i.path),
                "worker",
              ),
              folder: open,
              workerId,
              worker: w,
            }}
          />
        }
        selectionActions={(ctx) => (
          <Button
            size="sm"
            onClick={() => transfer.openFor(ctx.items, ctx.parent, ctx.size, ctx.clear)}
          >
            <FolderInput aria-hidden />
            Copy or move…
          </Button>
        )}
      />
      <TransferActionsHost actions={transfer} />
    </>
  );
}
