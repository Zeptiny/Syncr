import { Code, ConnectError } from "@connectrpc/connect";
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { FolderPlus } from "lucide-react";
import { useState } from "react";

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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectSeparator,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { ScheduleService, type ScheduleGroup } from "@/gen/syncr/v1/schedules_pb";
import { errorMessage, fieldViolations } from "@/lib/errors";
import { invalidateService } from "@/lib/invalidate";

/** useScheduleGroups lists the user's schedule groups, by name. */
export function useScheduleGroups() {
  return useQuery(ScheduleService.method.listScheduleGroups, {});
}

/** groupError says what was wrong with a group name the API refused. */
export function groupError(err: unknown, name: string): string {
  if (err instanceof ConnectError && err.code === Code.AlreadyExists)
    return `You already have a group named “${name.trim()}”.`;
  const v = fieldViolations(err).find((f) => f.field === "name");
  if (v) return `The name ${v.message}.`;
  return errorMessage(err);
}

/**
 * GroupNameDialog creates a group, or renames group. onSaved gets the
 * stored group (e.g. to move schedules into a new one).
 */
export function GroupNameDialog({
  open,
  onOpenChange,
  group,
  onSaved,
  description,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** group is the group to rename; without it the dialog creates one. */
  group?: ScheduleGroup;
  onSaved?: (g: ScheduleGroup) => Promise<unknown> | void;
  description?: string;
}) {
  const queryClient = useQueryClient();
  const createGroup = useMutation(ScheduleService.method.createScheduleGroup);
  const renameGroup = useMutation(ScheduleService.method.renameScheduleGroup);
  const [name, setName] = useState(group?.name ?? "");
  const [error, setError] = useState<string>();
  const pending = createGroup.isPending || renameGroup.isPending;
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(undefined);
    try {
      const res = group
        ? await renameGroup.mutateAsync({ groupId: group.id, name: name.trim() })
        : await createGroup.mutateAsync({ name: name.trim() });
      if (res.group) await onSaved?.(res.group);
      await invalidateService(queryClient, ScheduleService);
      onOpenChange(false);
    } catch (err) {
      setError(groupError(err, name));
    }
  };
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (!pending) onOpenChange(o);
      }}
    >
      <DialogContent>
        <form onSubmit={(e) => void submit(e)} className="flex flex-col gap-4" noValidate>
          <DialogHeader>
            <DialogTitle>{group ? `Rename ${group.name}` : "New group"}</DialogTitle>
            <DialogDescription>
              {description ??
                "Groups keep your schedule list in order. They change nothing about when or where schedules run."}
            </DialogDescription>
          </DialogHeader>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="group-name">Name</Label>
            <Input
              id="group-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="Photos"
              maxLength={64}
              autoComplete="off"
              autoFocus
              aria-invalid={error ? true : undefined}
            />
            {error && (
              <p role="alert" className="text-xs text-danger">
                {error}
              </p>
            )}
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              disabled={pending}
              onClick={() => onOpenChange(false)}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={!name.trim() || pending}>
              {pending ? "Saving…" : group ? "Rename" : "Create group"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

const NONE = "__none__";
const NEW = "__new__";

/**
 * GroupPicker chooses a schedule's group: none, one of the user's, or a
 * new one (created at once, in a dialog).
 */
export function GroupPicker({
  id,
  value,
  onChange,
  groups,
  invalid,
}: {
  id?: string;
  value: string;
  onChange: (groupId: string) => void;
  groups: ScheduleGroup[];
  invalid?: boolean;
}) {
  const [creating, setCreating] = useState(false);
  // A group deleted since the form opened reads as none.
  const current = groups.some((g) => g.id === value) ? value : NONE;
  return (
    <>
      <Select
        value={current}
        onValueChange={(v) => {
          if (v === NEW) setCreating(true);
          else onChange(v === NONE ? "" : v);
        }}
      >
        <SelectTrigger id={id} className="w-full bg-card" aria-invalid={invalid ? true : undefined}>
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value={NONE}>No group</SelectItem>
          {groups.length > 0 && <SelectSeparator />}
          {groups.map((g) => (
            <SelectItem key={g.id} value={g.id}>
              {g.name}
            </SelectItem>
          ))}
          <SelectSeparator />
          <SelectItem value={NEW}>
            <FolderPlus aria-hidden />
            New group…
          </SelectItem>
        </SelectContent>
      </Select>
      {creating && (
        <GroupNameDialog
          open={creating}
          onOpenChange={setCreating}
          onSaved={(g) => onChange(g.id)}
        />
      )}
    </>
  );
}
