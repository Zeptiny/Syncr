import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Pencil } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { LabelChips, LabelEditor } from "@/components/app/label-editor";
import { Panel } from "@/components/app/panel";
import { Button } from "@/components/ui/button";
import { WorkerAdminService, type Worker } from "@/gen/syncr/v1/api_pb";
import { errorMessage, fieldViolations } from "@/lib/errors";
import { invalidateService } from "@/lib/invalidate";
import { normalizeLabels } from "@/lib/labels";

/**
 * WorkerLabelsPanel shows a worker's labels; administrators can edit them.
 * Schedules that target a label run on the workers that carry it.
 */
export function WorkerLabelsPanel({ worker, canEdit }: { worker: Worker; canEdit: boolean }) {
  const queryClient = useQueryClient();
  const save = useMutation(WorkerAdminService.method.setWorkerLabels);
  const all = useQuery(WorkerAdminService.method.listWorkers, {}, { enabled: canEdit });
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState<string[]>(worker.labels);
  const [error, setError] = useState<string | undefined>();
  const suggestions = [
    ...new Set(
      (all.data?.workers ?? []).filter((w) => w.id !== worker.id).flatMap((w) => w.labels),
    ),
  ].sort();
  const changed = normalizeLabels(draft).join(",") !== normalizeLabels(worker.labels).join(",");

  const submit = async () => {
    setError(undefined);
    try {
      await save.mutateAsync({ workerId: worker.id, labels: normalizeLabels(draft) });
      await invalidateService(queryClient, WorkerAdminService);
      toast.success(`Saved the labels of ${worker.name}`);
      setEditing(false);
    } catch (err) {
      setError(fieldViolations(err)[0]?.message ?? errorMessage(err));
    }
  };

  return (
    <Panel
      title="Labels"
      description="Schedules can target a label instead of one worker; each run goes to the least busy online worker that has it."
      actions={
        canEdit &&
        !editing && (
          <Button
            variant="outline"
            size="sm"
            onClick={() => {
              setDraft(worker.labels);
              setError(undefined);
              setEditing(true);
            }}
          >
            <Pencil aria-hidden />
            Edit labels
          </Button>
        )
      }
    >
      {editing ? (
        <div className="flex flex-col gap-3">
          <LabelEditor
            id="worker-labels"
            value={draft}
            onChange={setDraft}
            error={error}
            suggestions={suggestions}
            disabled={save.isPending}
          />
          <div className="flex justify-end gap-2">
            <Button variant="outline" onClick={() => setEditing(false)} disabled={save.isPending}>
              Cancel
            </Button>
            <Button onClick={() => void submit()} disabled={save.isPending || !changed}>
              {save.isPending ? "Saving…" : "Save labels"}
            </Button>
          </div>
        </div>
      ) : (
        <LabelChips
          labels={worker.labels}
          className="gap-1.5 [&>span]:h-6 [&>span]:text-[12.5px]"
          empty={
            <p className="text-[13px] text-muted-foreground">
              No labels.
              {canEdit ? " Add some, such as nas or offsite, so schedules can target them." : ""}
            </p>
          }
        />
      )}
    </Panel>
  );
}
