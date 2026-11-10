import { useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Trash2 } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { FieldShell } from "@/components/app/descriptor-form";
import { Panel } from "@/components/app/panel";
import { WorkerPicker } from "@/components/app/worker-picker";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import type { Worker } from "@/gen/syncr/v1/api_pb";
import { ResticService, type Repository } from "@/gen/syncr/v1/restic_pb";
import { errorMessage, fieldViolations } from "@/lib/errors";
import { invalidateService } from "@/lib/invalidate";

import { isLocalRepository } from "./model";

type Errors = Partial<Record<"name" | "description" | "password" | "worker" | "form", string>>;

function serverErrors(err: unknown): Errors {
  const e: Errors = {};
  const rest: string[] = [];
  for (const v of fieldViolations(err)) {
    const m = v.message.charAt(0).toUpperCase() + v.message.slice(1);
    if (v.field === "name" || v.field === "description" || v.field === "password") e[v.field] = m;
    else if (v.field === "worker_id") e.worker = m;
    else rest.push(`${v.field}: ${v.message}`);
  }
  if (rest.length) e.form = rest.join(" ");
  else if (!Object.keys(e).length) {
    const message = errorMessage(err);
    if (/with this name already exists/i.test(message)) e.name = message;
    else e.form = message;
  }
  return e;
}

/**
 * RepositorySettings renames a repository, replaces its stored password
 * and offers to delete it. The stored password is never shown: the field
 * starts empty and only a new value is sent.
 */
export function RepositorySettings({
  repository: repo,
  workers,
  onDelete,
}: {
  repository: Repository;
  workers: Worker[];
  onDelete: () => void;
}) {
  const queryClient = useQueryClient();
  const update = useMutation(ResticService.method.updateRepository);
  const [name, setName] = useState(repo.name);
  const [description, setDescription] = useState(repo.description);
  const [password, setPassword] = useState("");
  const [workerId, setWorkerId] = useState("");
  const [errors, setErrors] = useState<Errors>({});
  const [saving, setSaving] = useState<"details" | "password" | undefined>();
  const locked = repo.deleting;
  const local = isLocalRepository(repo);

  const save = async (what: "details" | "password") => {
    const local: Errors = {};
    if (what === "details" && !name.trim()) local.name = "Give the repository a name.";
    if (what === "password" && !password) local.password = "Enter the repository's password.";
    if (what === "password" && isLocalRepository(repo) && !workerId)
      local.worker = "Choose the worker whose disk holds this repository.";
    setErrors(local);
    if (Object.keys(local).length) return;
    setSaving(what);
    try {
      await update.mutateAsync({
        repositoryId: repo.id,
        version: repo.version,
        name: what === "details" ? name.trim() : repo.name,
        description: what === "details" ? description.trim() : repo.description,
        password: what === "password" ? password : "",
        workerId: what === "password" ? workerId : "",
      });
      await invalidateService(queryClient, ResticService);
      if (what === "password") setPassword("");
      toast.success(what === "password" ? "Stored the new password" : `Saved ${name.trim()}`);
    } catch (err) {
      setErrors(serverErrors(err));
    } finally {
      setSaving(undefined);
    }
  };

  return (
    <div className="flex max-w-3xl flex-col gap-6">
      <Panel title="Name and description">
        <form
          noValidate
          className="flex flex-col gap-5"
          onSubmit={(e) => {
            e.preventDefault();
            void save("details");
          }}
        >
          <FieldShell id="repo-set-name" label="Name" error={errors.name}>
            <Input
              id="repo-set-name"
              className="max-w-md bg-card"
              value={name}
              onChange={(e) => setName(e.target.value)}
              maxLength={64}
              autoComplete="off"
              disabled={locked}
              aria-invalid={errors.name ? true : undefined}
            />
          </FieldShell>
          <FieldShell id="repo-set-desc" label="Description" optional error={errors.description}>
            <Input
              id="repo-set-desc"
              className="bg-card"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              autoComplete="off"
              disabled={locked}
            />
          </FieldShell>
          <div className="flex justify-end">
            <Button type="submit" disabled={locked || saving !== undefined}>
              {saving === "details" ? "Saving…" : "Save changes"}
            </Button>
          </div>
        </form>
      </Panel>

      <Panel
        title="Update stored password"
        description="Use this after the repository's password was changed with restic. A worker opens the repository with the new password before it is stored. The stored password is never shown."
      >
        <form
          noValidate
          className="flex flex-col gap-5"
          onSubmit={(e) => {
            e.preventDefault();
            void save("password");
          }}
        >
          <div className="grid gap-5 md:grid-cols-2">
            <FieldShell id="repo-set-password" label="New password" error={errors.password}>
              <Input
                id="repo-set-password"
                type="password"
                className="bg-card font-mono text-[13px]"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                autoComplete="new-password"
                disabled={locked}
                aria-invalid={errors.password ? true : undefined}
              />
            </FieldShell>
            <FieldShell
              id="repo-set-worker"
              label="Check it on"
              optional={!local}
              error={errors.worker}
            >
              <WorkerPicker
                id="repo-set-worker"
                workers={workers}
                value={workerId}
                onChange={setWorkerId}
                autoLabel={local ? undefined : "Automatic"}
                disabled={locked}
                invalid={!!errors.worker}
                className="bg-card"
              />
            </FieldShell>
          </div>
          <div className="flex justify-end">
            <Button type="submit" variant="outline" disabled={locked || saving !== undefined}>
              {saving === "password" ? "Checking…" : "Update password"}
            </Button>
          </div>
        </form>
      </Panel>

      {errors.form && (
        <p role="alert" className="rounded-md bg-danger-soft px-3 py-2 text-[13px] text-danger">
          {errors.form}
        </p>
      )}

      <Panel
        title="Delete"
        description="Forget the repository in Syncr and keep the backups where they are, or also delete the backup data."
      >
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className="max-w-prose text-[13px] text-muted-foreground">
            {locked
              ? "The data is being deleted. Cancel the deletion job to keep the repository."
              : "Forgetting is the default and can be undone by connecting the repository again."}
          </p>
          <Button
            type="button"
            variant="outline"
            className="text-danger hover:text-danger"
            disabled={locked}
            onClick={onDelete}
          >
            <Trash2 aria-hidden />
            Delete repository
          </Button>
        </div>
      </Panel>
    </div>
  );
}
