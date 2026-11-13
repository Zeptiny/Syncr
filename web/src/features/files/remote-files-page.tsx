import { useQuery } from "@connectrpc/connect-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { FolderInput, Pencil } from "lucide-react";

import { ErrorState } from "@/components/app/error-state";
import { FileBrowser } from "@/components/app/file-browser/file-browser";
import { copyPathItem } from "@/components/app/file-browser/helpers";
import { commonParent, uncovered } from "@/components/app/file-browser/selection";
import { useBrowserSelection } from "@/components/app/file-browser/use-selection";
import { PageHeader } from "@/components/app/page-header";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { WorkerAdminService } from "@/gen/syncr/v1/api_pb";
import { RemoteService } from "@/gen/syncr/v1/remotes_pb";
import { useBackendMap } from "@/hooks/use-backends";
import { sourceKey, usePathListing, type PathSource } from "@/hooks/use-path-listing";
import { normalizePath } from "@/lib/paths";

import { BackendIcon } from "../remotes/backend-icon";
import { JobDraftMenu } from "./job-draft-menu";
import { RemoteUsage } from "./remote-usage";
import type { RemoteFilesSearch } from "./search";
import type { Place } from "./transfer";
import { TransferActionsHost } from "./transfer-actions-host";
import { useTransferActions } from "./use-transfer-actions";

/**
 * RemoteFilesPage browses one of the user's remotes through a worker: the
 * controller's choice, or the one in the URL (worker=). A remote on a
 * worker's own disk needs that worker chosen first. At the root the
 * toolbar shows the remote's usage when its backend can tell. A selection
 * can be copied or moved, and jobs started from it.
 */
export function RemoteFilesPage({
  remoteId,
  path,
  worker,
}: { remoteId: string } & RemoteFilesSearch) {
  const navigate = useNavigate();
  const rq = useQuery(RemoteService.method.getRemote, { remoteId });
  const workers = useQuery(WorkerAdminService.method.listWorkers, {});
  const backends = useBackendMap();
  const remote = rq.data?.remote;
  const backend = remote ? backends.get(remote.backendType) : undefined;
  const local = !!backend?.capabilities?.local;
  const source: PathSource = { kind: "remote", remoteId, workerId: worker };
  const [selection, setSelection] = useBrowserSelection(sourceKey(source, local));
  const open = path ? normalizePath(path, "remote") : "";
  const waiting = local && !worker;
  // The same query as the browser's: it tells which worker listed the folder.
  const listing = usePathListing(source, open, { enabled: !!remote && !waiting });
  const place: Place = { kind: "remote", remoteId, local, workerId: local ? worker : undefined };
  const transfer = useTransferActions(place);
  const go = (search: RemoteFilesSearch) =>
    void navigate({ to: "/remotes/$remoteId/files", params: { remoteId }, search });

  if (rq.error) return <ErrorState error={rq.error} onRetry={() => void rq.refetch()} />;
  if (!remote)
    return (
      <div className="flex flex-col gap-4">
        <Skeleton className="h-8 w-72" />
        <Skeleton className="h-96 w-full" />
      </div>
    );

  const items = uncovered(selection, "remote");
  return (
    <>
      <PageHeader
        back={{ to: "/remotes", label: "Remotes" }}
        title={
          <span className="flex items-center gap-3">
            <BackendIcon id={remote.backendType} className="size-9" />
            {remote.name}
          </span>
        }
        meta={
          <span className="text-[13px] text-muted-foreground">
            {backend?.label ?? remote.backendType}
            {local ? ", a folder on a worker's disk" : ""}
          </span>
        }
        actions={
          <Button variant="outline" asChild>
            <Link to="/remotes/$remoteId" params={{ remoteId }}>
              <Pencil aria-hidden />
              Edit remote
            </Link>
          </Button>
        }
      />
      <FileBrowser
        source={source}
        path={open}
        onNavigate={(to) => go({ path: to || undefined, worker })}
        selection={selection}
        onSelectionChange={setSelection}
        rootLabel={remote.name}
        rowMenu={(e) => [transfer.rowItem(e), copyPathItem(e)]}
        toolbarActions={
          waiting ? undefined : (
            <JobDraftMenu
              target={{
                place,
                items,
                parent: commonParent(
                  items.map((i) => i.path),
                  "remote",
                ),
                folder: open,
                workerId: worker ?? listing.workerId ?? "",
              }}
            />
          )
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
        workerChoice={{
          workers: workers.data?.workers ?? [],
          op: "list_remote",
          required: local,
          onChange: (id) => go({ path: open || undefined, worker: id || undefined }),
        }}
        waitingForWorker={
          waiting ? "Choose the worker whose disk holds this remote, above." : undefined
        }
        toolbarExtra={
          open === "" ? (
            <RemoteUsage remoteId={remoteId} workerId={worker} local={local} />
          ) : undefined
        }
      />
      <TransferActionsHost actions={transfer} listedBy={listing.workerId} />
    </>
  );
}
