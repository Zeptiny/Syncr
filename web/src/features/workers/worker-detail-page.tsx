import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { Pencil, Trash2 } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { ConfirmDialog } from "@/components/app/confirm-dialog";
import { ErrorState } from "@/components/app/error-state";
import { KeyValueList } from "@/components/app/key-value-list";
import { PageHeader } from "@/components/app/page-header";
import { Panel } from "@/components/app/panel";
import { RelativeTime } from "@/components/app/relative-time";
import { StatusBadge } from "@/components/app/status-badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { JobService, WorkerAdminService, type Worker } from "@/gen/syncr/v1/api_pb";
import { WorkerMode } from "@/gen/syncr/v1/link_pb";
import { useSession } from "@/hooks/use-session";
import { errorMessage } from "@/lib/errors";
import { formatDateTime, tsDate } from "@/lib/format";
import { invalidateService } from "@/lib/invalidate";
import { workerModeLabel } from "@/lib/status";
import { JobsTable } from "@/features/jobs/jobs-table";

import { platformLabel } from "./workers-page";

function RenameDialog({
  worker,
  open,
  onOpenChange,
}: {
  worker: Worker;
  open: boolean;
  onOpenChange: (o: boolean) => void;
}) {
  const [name, setName] = useState(worker.name);
  const rename = useMutation(WorkerAdminService.method.renameWorker);
  const queryClient = useQueryClient();
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    await rename.mutateAsync({ workerId: worker.id, name: name.trim() });
    await invalidateService(queryClient, WorkerAdminService);
    toast.success("Worker renamed");
    onOpenChange(false);
  };
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <form onSubmit={(e) => void submit(e).catch(() => {})} className="flex flex-col gap-4">
          <DialogHeader>
            <DialogTitle>Rename worker</DialogTitle>
            <DialogDescription>The name is shown in job lists and pickers.</DialogDescription>
          </DialogHeader>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="worker-name">Name</Label>
            <Input
              id="worker-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              maxLength={64}
              autoFocus
            />
            {rename.error && <p className="text-xs text-danger">{errorMessage(rename.error)}</p>}
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={!name.trim() || rename.isPending}>
              Rename
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/** WorkerDetailPage shows one worker, its local folders and recent jobs. */
export function WorkerDetailPage({ workerId }: { workerId: string }) {
  const { isAdmin } = useSession();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const q = useQuery(
    WorkerAdminService.method.getWorker,
    { workerId },
    { refetchInterval: 10_000 },
  );
  const jobs = useQuery(JobService.method.listJobs, { workerId, pageSize: 10 });
  const remove = useMutation(WorkerAdminService.method.removeWorker);
  const [renaming, setRenaming] = useState(false);
  const [removing, setRemoving] = useState(false);

  if (q.error) return <ErrorState error={q.error} onRetry={() => void q.refetch()} />;
  const w = q.data?.worker;
  if (!w) {
    return (
      <div className="flex flex-col gap-4">
        <Skeleton className="h-8 w-60" />
        <Skeleton className="h-48 w-full" />
      </div>
    );
  }
  const info = w.info;
  return (
    <>
      <PageHeader
        back={{ to: "/workers", label: "Workers" }}
        title={w.name}
        meta={
          <>
            <StatusBadge worker={w.status} />
            <span className="text-[13px] text-muted-foreground">
              {workerModeLabel(w.mode)} link, last seen <RelativeTime date={tsDate(w.lastSeen)} />
            </span>
          </>
        }
        actions={
          isAdmin && (
            <>
              <Button variant="outline" onClick={() => setRenaming(true)}>
                <Pencil aria-hidden />
                Rename
              </Button>
              <Button
                variant="outline"
                className="text-danger hover:text-danger"
                onClick={() => setRemoving(true)}
              >
                <Trash2 aria-hidden />
                Remove
              </Button>
            </>
          )
        }
      />
      <div className="grid gap-6 lg:grid-cols-[minmax(0,3fr)_minmax(0,2fr)]">
        <Panel title="About this worker">
          <KeyValueList
            items={[
              { label: "Hostname", value: info?.hostname || "–" },
              { label: "Platform", value: platformLabel(w), mono: true },
              { label: "Worker version", value: info?.workerVersion || "–" },
              {
                label: "Tools",
                value: info?.tools.length
                  ? info.tools.map((t) => `${t.name} ${t.version}`).join(", ")
                  : "–",
              },
              { label: "Job kinds", value: info?.jobKinds.join(", ") || "–", mono: true },
              { label: "Parallel jobs", value: info?.maxConcurrentJobs || "–" },
              ...(w.mode === WorkerMode.PASSIVE
                ? [
                    { label: "URL", value: w.url, mono: true },
                    { label: "Fingerprint", value: w.certFingerprint, mono: true },
                  ]
                : []),
              { label: "Added", value: formatDateTime(tsDate(w.createdAt)) },
              { label: "Worker ID", value: w.id, mono: true },
            ]}
          />
        </Panel>
        <Panel
          title="Allowed folders"
          description="Jobs may read and write the worker's own disks only inside these folders."
        >
          {info?.localRoots.length ? (
            <ul className="flex flex-col gap-1.5">
              {info.localRoots.map((r) => (
                <li key={r} className="rounded-md bg-muted px-3 py-2 font-mono text-[13px]">
                  {r}
                </li>
              ))}
            </ul>
          ) : (
            <p className="text-[13px] text-muted-foreground">
              None. Jobs on this worker can only move files between remotes.
            </p>
          )}
        </Panel>
      </div>
      <h2 className="mt-8 mb-3 text-[15px] font-semibold">Recent jobs</h2>
      <JobsTable
        jobs={jobs.data?.jobs}
        isLoading={jobs.isPending}
        error={jobs.error ?? undefined}
        onRetry={() => void jobs.refetch()}
        workers={new Map([[w.id, w]])}
        hideWorker
        empty={
          <p className="p-8 text-center text-[13px] text-muted-foreground">
            No jobs have run on this worker yet.
          </p>
        }
      />
      {isAdmin && (
        <>
          <RenameDialog key={w.name} worker={w} open={renaming} onOpenChange={setRenaming} />
          <ConfirmDialog
            open={removing}
            onOpenChange={setRemoving}
            destructive
            title={`Remove ${w.name}?`}
            description="Its credentials stop working at once. Queued jobs fail and running jobs are interrupted. Job history is kept. To use the machine again, enroll it anew."
            confirmLabel="Remove worker"
            onConfirm={async () => {
              await remove.mutateAsync({ workerId: w.id });
              await invalidateService(queryClient, WorkerAdminService);
              toast.success(`Removed ${w.name}`);
              await navigate({ to: "/workers" });
            }}
          />
        </>
      )}
    </>
  );
}
