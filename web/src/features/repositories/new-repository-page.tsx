import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { toast } from "sonner";

import { PageHeader } from "@/components/app/page-header";
import { Button } from "@/components/ui/button";
import { WorkerAdminService } from "@/gen/syncr/v1/api_pb";
import { RemoteService } from "@/gen/syncr/v1/remotes_pb";
import { RepositoryCreateMode, ResticService } from "@/gen/syncr/v1/restic_pb";
import { useBackendMap } from "@/hooks/use-backends";
import { invalidateService } from "@/lib/invalidate";

import {
  emptyRepositoryForm,
  isLocalRemote,
  RepositoryForm,
  repositoryServerErrors,
  toRepositoryInput,
  validateRepositoryForm,
  type RepositoryFormErrors,
  type RepositoryFormState,
} from "./repository-form";

/**
 * NewRepositoryPage creates a restic repository in a remote, or connects
 * to one that exists. A worker does the work, so it can take a moment.
 */
export function NewRepositoryPage({ remote }: { remote?: string }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const backends = useBackendMap();
  const remotes = useQuery(RemoteService.method.listRemotes, {});
  const workers = useQuery(WorkerAdminService.method.listWorkers, {});
  const create = useMutation(ResticService.method.createRepository);
  const [s, setS] = useState<RepositoryFormState>(() => emptyRepositoryForm(remote));
  const [errors, setErrors] = useState<RepositoryFormErrors>({});
  const [formError, setFormError] = useState<string | undefined>();
  const remoteList = remotes.data?.remotes ?? [];
  const workerList = workers.data?.workers ?? [];
  const init = s.mode === RepositoryCreateMode.INIT;
  const workerName = workerList.find((w) => w.id === s.workerId)?.name;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setFormError(undefined);
    const local = validateRepositoryForm(
      s,
      isLocalRemote(
        remoteList.find((r) => r.id === s.remoteId),
        backends,
      ),
    );
    setErrors(local);
    if (Object.keys(local).length) {
      setFormError("Check the highlighted fields.");
      return;
    }
    try {
      const res = await create.mutateAsync({ input: toRepositoryInput(s) });
      await invalidateService(queryClient, ResticService);
      const by = res.worker?.name ? ` on ${res.worker.name}` : "";
      toast.success(init ? `Created ${s.name.trim()}${by}` : `Connected ${s.name.trim()}${by}`);
      if (res.repository)
        await navigate({
          to: "/repositories/$repositoryId",
          params: { repositoryId: res.repository.id },
        });
    } catch (err) {
      const server = repositoryServerErrors(err);
      setErrors(server.errors);
      setFormError(
        server.form ?? (Object.keys(server.errors).length ? "Check the highlighted fields." : ""),
      );
    }
  };

  const busy = init ? "Initializing" : "Connecting";
  return (
    <>
      <PageHeader
        back={{ to: "/repositories", label: "Repositories" }}
        title="Add a repository"
        description="A restic repository keeps encrypted, deduplicated snapshots of your folders inside a remote."
      />
      <form onSubmit={(e) => void submit(e)} noValidate className="flex max-w-5xl flex-col gap-6">
        <RepositoryForm
          value={s}
          onChange={setS}
          errors={errors}
          remotes={remoteList}
          backends={backends}
          workers={workerList}
          disabled={create.isPending}
        />
        {formError && (
          <p role="alert" className="rounded-md bg-danger-soft px-3 py-2 text-[13px] text-danger">
            {formError}
          </p>
        )}
        <div className="flex justify-end gap-2">
          <Button
            type="button"
            variant="outline"
            onClick={() => void navigate({ to: "/repositories" })}
          >
            Cancel
          </Button>
          <Button type="submit" disabled={create.isPending}>
            {create.isPending
              ? `${busy}${workerName ? ` on ${workerName}` : ""}…`
              : init
                ? "Create repository"
                : "Connect repository"}
          </Button>
        </div>
      </form>
    </>
  );
}
