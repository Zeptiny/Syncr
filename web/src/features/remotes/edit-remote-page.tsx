import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { Trash2 } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { ConfirmDialog } from "@/components/app/confirm-dialog";
import { RemoteForm } from "@/components/app/descriptor-form";
import { ErrorState } from "@/components/app/error-state";
import { PageHeader } from "@/components/app/page-header";
import { Panel } from "@/components/app/panel";
import { RelativeTime } from "@/components/app/relative-time";
import { TestRemoteButton } from "@/components/app/test-remote-button";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { WorkerAdminService } from "@/gen/syncr/v1/api_pb";
import { RemoteService } from "@/gen/syncr/v1/remotes_pb";
import { ResticService } from "@/gen/syncr/v1/restic_pb";
import { ScheduleService } from "@/gen/syncr/v1/schedules_pb";
import { useBackendMap } from "@/hooks/use-backends";
import { tsDate } from "@/lib/format";
import { invalidateService } from "@/lib/invalidate";

import { BackendIcon } from "./backend-icon";
import { HOST_KEYS_FIELD, HostKeysField } from "./host-keys";

/** EditRemotePage edits a remote and offers to delete it. */
export function EditRemotePage({ remoteId }: { remoteId: string }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const backends = useBackendMap();
  const q = useQuery(RemoteService.method.getRemote, { remoteId });
  const remotes = useQuery(RemoteService.method.listRemotes, {});
  const usedBy = useQuery(ScheduleService.method.listSchedules, { remoteId });
  const repos = useQuery(ResticService.method.listRepositories, { remoteId });
  const workers = useQuery(WorkerAdminService.method.listWorkers, {});
  const update = useMutation(RemoteService.method.updateRemote);
  const del = useMutation(RemoteService.method.deleteRemote);
  const [deleting, setDeleting] = useState(false);

  if (q.error) return <ErrorState error={q.error} onRetry={() => void q.refetch()} />;
  const remote = q.data?.remote;
  const backend = remote ? backends.get(remote.backendType) : undefined;
  if (!remote || !backend) {
    return (
      <div className="flex flex-col gap-4">
        <Skeleton className="h-8 w-72" />
        <Skeleton className="h-96 w-full max-w-4xl" />
      </div>
    );
  }
  const inUse = remote.usedByUnions.length > 0;
  const schedules = usedBy.data?.schedules ?? [];
  const repositories = repos.data?.repositories ?? [];
  // used_by_unions holds IDs; show the unions' names.
  const unionNames = remote.usedByUnions.map(
    (id) => remotes.data?.remotes.find((r) => r.id === id)?.name ?? id,
  );

  return (
    <>
      <PageHeader
        back={{ to: "/remotes", label: "Remotes" }}
        title={
          <span className="flex items-center gap-3">
            <BackendIcon id={backend.id} className="size-9" />
            {remote.name}
          </span>
        }
        meta={
          <span className="text-[13px] text-muted-foreground">
            {backend.label}, updated <RelativeTime date={tsDate(remote.updatedAt)} />
          </span>
        }
      />
      <Panel className="max-w-4xl">
        <RemoteForm
          key={String(remote.version)}
          backend={backend}
          remote={remote}
          remotes={remotes.data?.remotes ?? []}
          backends={backends}
          onCancel={() => void navigate({ to: "/remotes" })}
          onSubmit={async (input) => {
            await update.mutateAsync({ remoteId: remote.id, version: remote.version, input });
            await invalidateService(queryClient, RemoteService);
            toast.success(`Saved ${input.name ?? remote.name}`);
          }}
          renderField={(f, idPrefix) =>
            f.name === HOST_KEYS_FIELD ? (
              <HostKeysField field={f} idPrefix={idPrefix} workers={workers.data?.workers ?? []} />
            ) : undefined
          }
          extra={(input) => (
            <TestRemoteButton
              remoteId={remote.id}
              input={input}
              workers={workers.data?.workers ?? []}
              needsWorker={!!backend.capabilities?.local}
            />
          )}
          footer={
            <Button
              type="button"
              variant="ghost"
              className="text-danger hover:text-danger"
              onClick={() => setDeleting(true)}
            >
              <Trash2 aria-hidden />
              Delete remote
            </Button>
          }
        />
      </Panel>
      <ConfirmDialog
        open={deleting}
        onOpenChange={setDeleting}
        destructive
        title={`Delete ${remote.name}?`}
        description={
          repositories.length
            ? `${repositories.map((x) => x.name).join(", ")} ${repositories.length === 1 ? "is a repository" : "are repositories"} on this remote. Forget or delete ${repositories.length === 1 ? "it" : "them"} first: a remote that holds repositories cannot be deleted.`
            : inUse
              ? `It is a member of ${unionNames.join(", ")}. Remove it from ${unionNames.length === 1 ? "that union" : "those unions"} first.`
              : schedules.length
                ? `${schedules.map((x) => x.name).join(", ")} ${schedules.length === 1 ? "uses" : "use"} it, and will be paused at ${schedules.length === 1 ? "its" : "their"} next run. Its saved credentials are erased; past jobs keep its name.`
                : "Its saved credentials are erased. Past jobs keep its name; queued jobs that use it will fail."
        }
        confirmLabel="Delete remote"
        // The controller refuses while repositories or unions use the remote.
        confirmDisabled={repos.isPending || repositories.length > 0 || inUse}
        onConfirm={async () => {
          await del.mutateAsync({ remoteId: remote.id });
          await invalidateService(queryClient, RemoteService);
          toast.success(`Deleted ${remote.name}`);
          await navigate({ to: "/remotes" });
        }}
      />
    </>
  );
}
