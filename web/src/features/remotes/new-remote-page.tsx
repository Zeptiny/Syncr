import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { toast } from "sonner";

import { RemoteForm } from "@/components/app/descriptor-form";
import { ErrorState } from "@/components/app/error-state";
import { PageHeader } from "@/components/app/page-header";
import { Panel } from "@/components/app/panel";
import { TestRemoteButton } from "@/components/app/test-remote-button";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { WorkerAdminService } from "@/gen/syncr/v1/api_pb";
import { RemoteService } from "@/gen/syncr/v1/remotes_pb";
import { useBackendMap, useBackends } from "@/hooks/use-backends";
import { invalidateService } from "@/lib/invalidate";
import { cn } from "@/lib/utils";

import { BackendIcon } from "./backend-icon";

/** NewRemotePage picks a backend type, then shows its form. */
export function NewRemotePage({ type }: { type?: string }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const backendsQ = useBackends();
  const backends = useBackendMap();
  const remotes = useQuery(RemoteService.method.listRemotes, {});
  const create = useMutation(RemoteService.method.createRemote);
  const workers = useQuery(WorkerAdminService.method.listWorkers, {});
  const chosen = type ? backends.get(type) : undefined;

  if (backendsQ.error)
    return <ErrorState error={backendsQ.error} onRetry={() => void backendsQ.refetch()} />;

  if (!chosen) {
    return (
      <>
        <PageHeader
          back={{ to: "/remotes", label: "Remotes" }}
          title="Add a remote"
          description="Choose the kind of storage. You can add as many remotes of each kind as you like."
        />
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {backendsQ.isPending &&
            Array.from({ length: 6 }, (_, i) => <Skeleton key={i} className="h-28 rounded-lg" />)}
          {backendsQ.data?.backends.map((b) => (
            <Link
              key={b.id}
              to="/remotes/new"
              search={{ type: b.id }}
              className={cn(
                "group flex gap-3.5 rounded-lg border bg-card p-4 transition-colors hover:border-primary/60 focus-visible:border-primary",
              )}
            >
              <BackendIcon
                id={b.id}
                className="size-9 transition-colors group-hover:bg-primary group-hover:text-primary-foreground"
              />
              <div className="min-w-0">
                <div className="font-semibold">{b.label}</div>
                <p className="mt-0.5 text-[13px] leading-5 text-muted-foreground">
                  {b.description}
                </p>
              </div>
            </Link>
          ))}
        </div>
      </>
    );
  }

  return (
    <>
      <PageHeader
        back={{ to: "/remotes", label: "Remotes" }}
        title={`New ${chosen.label} remote`}
        description={chosen.description}
        actions={
          <Button variant="outline" asChild>
            <Link to="/remotes/new">Choose another kind</Link>
          </Button>
        }
      />
      <Panel className="max-w-4xl">
        <RemoteForm
          backend={chosen}
          remotes={remotes.data?.remotes ?? []}
          backends={backends}
          onCancel={() => void navigate({ to: "/remotes" })}
          extra={(input) => (
            <TestRemoteButton
              input={input}
              workers={workers.data?.workers ?? []}
              needsWorker={!!chosen.capabilities?.local}
            />
          )}
          onSubmit={async (input) => {
            const res = await create.mutateAsync({ input });
            await invalidateService(queryClient, RemoteService);
            toast.success(`Created ${res.remote?.name ?? "remote"}`);
            await navigate({ to: "/remotes" });
          }}
        />
      </Panel>
    </>
  );
}
