import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { createColumnHelper } from "@tanstack/react-table";
import { MoreHorizontal, Plus, UserRound } from "lucide-react";
import { useMemo, useState } from "react";
import { toast } from "sonner";

import { ConfirmDialog } from "@/components/app/confirm-dialog";
import { DataTable } from "@/components/app/data-table";
import { FieldShell } from "@/components/app/descriptor-form";
import { EmptyState } from "@/components/app/empty-state";
import { PageHeader } from "@/components/app/page-header";
import { RelativeTime } from "@/components/app/relative-time";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { UserRole, UserService, type User } from "@/gen/syncr/v1/users_pb";
import { useSession } from "@/hooks/use-session";
import { errorMessage, fieldViolations } from "@/lib/errors";
import { tsDate } from "@/lib/format";
import { invalidateService } from "@/lib/invalidate";

const col = createColumnHelper<User>();

/** MIN_PASSWORD is the controller's password policy minimum. */
export const MIN_PASSWORD = 12;

function violationsOf(err: unknown): Record<string, string> {
  return Object.fromEntries(fieldViolations(err).map((v) => [v.field, v.message]));
}

function RoleSelect({
  id,
  value,
  onChange,
}: {
  id: string;
  value: UserRole;
  onChange: (r: UserRole) => void;
}) {
  return (
    <Select value={String(value)} onValueChange={(v) => onChange(Number(v))}>
      <SelectTrigger id={id} className="w-full">
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        <SelectItem value={String(UserRole.USER)}>
          User: runs jobs with their own remotes
        </SelectItem>
        <SelectItem value={String(UserRole.ADMIN)}>
          Administrator: also manages users and workers
        </SelectItem>
      </SelectContent>
    </Select>
  );
}

function CreateUserDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
}) {
  const create = useMutation(UserService.method.createUser);
  const queryClient = useQueryClient();
  const [f, setF] = useState({ username: "", displayName: "", password: "", role: UserRole.USER });
  const [errors, setErrors] = useState<Record<string, string>>({});
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    const local: Record<string, string> = {};
    if (!/^[a-z0-9][a-z0-9._-]{0,63}$/.test(f.username)) {
      local.username = "Use lower-case letters, digits, dots, dashes or underscores.";
    }
    if (f.password.length < MIN_PASSWORD)
      local.password = `Use at least ${MIN_PASSWORD} characters.`;
    setErrors(local);
    if (Object.keys(local).length) return;
    try {
      await create.mutateAsync(f);
      await invalidateService(queryClient, UserService);
      toast.success(`Added ${f.username}`);
      setF({ username: "", displayName: "", password: "", role: UserRole.USER });
      onOpenChange(false);
    } catch (err) {
      const v = violationsOf(err);
      setErrors(Object.keys(v).length ? v : { form: errorMessage(err) });
    }
  };
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <form onSubmit={(e) => void submit(e)} noValidate className="flex flex-col gap-4">
          <DialogHeader>
            <DialogTitle>Add a user</DialogTitle>
            <DialogDescription>
              Share the password with them; they can change it in Settings.
            </DialogDescription>
          </DialogHeader>
          <FieldShell id="nu-username" label="Username" error={errors.username}>
            <Input
              id="nu-username"
              value={f.username}
              autoCapitalize="none"
              onChange={(e) => setF({ ...f, username: e.target.value.toLowerCase() })}
            />
          </FieldShell>
          <FieldShell id="nu-display" label="Display name" optional error={errors.display_name}>
            <Input
              id="nu-display"
              value={f.displayName}
              onChange={(e) => setF({ ...f, displayName: e.target.value })}
            />
          </FieldShell>
          <FieldShell
            id="nu-password"
            label="Password"
            error={errors.password}
            help={`At least ${MIN_PASSWORD} characters.`}
          >
            <Input
              id="nu-password"
              type="password"
              autoComplete="new-password"
              value={f.password}
              onChange={(e) => setF({ ...f, password: e.target.value })}
            />
          </FieldShell>
          <FieldShell id="nu-role" label="Role" error={errors.role}>
            <RoleSelect id="nu-role" value={f.role} onChange={(role) => setF({ ...f, role })} />
          </FieldShell>
          {errors.form && <p className="text-[13px] text-danger">{errors.form}</p>}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={create.isPending}>
              Add user
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function EditUserDialog({ user, onClose }: { user: User; onClose: () => void }) {
  const update = useMutation(UserService.method.updateUser);
  const queryClient = useQueryClient();
  const [displayName, setDisplayName] = useState(user.displayName);
  const [role, setRole] = useState(user.role);
  const [disabled, setDisabled] = useState(user.disabled);
  const [error, setError] = useState<string | undefined>();
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    try {
      await update.mutateAsync({
        userId: user.id,
        version: user.version,
        displayName: displayName === user.displayName ? undefined : displayName,
        role: role === user.role ? UserRole.UNSPECIFIED : role,
        disabled: disabled === user.disabled ? undefined : disabled,
      });
      await invalidateService(queryClient, UserService);
      toast.success(`Saved ${user.username}`);
      onClose();
    } catch (err) {
      setError(errorMessage(err));
    }
  };
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <form onSubmit={(e) => void submit(e)} className="flex flex-col gap-4">
          <DialogHeader>
            <DialogTitle>Edit {user.username}</DialogTitle>
            <DialogDescription>
              Changing the role or disabling the account signs the user out everywhere.
            </DialogDescription>
          </DialogHeader>
          <FieldShell id="eu-display" label="Display name" optional>
            <Input
              id="eu-display"
              value={displayName}
              onChange={(e) => setDisplayName(e.target.value)}
            />
          </FieldShell>
          <FieldShell id="eu-role" label="Role">
            <RoleSelect id="eu-role" value={role} onChange={setRole} />
          </FieldShell>
          <div className="flex items-start justify-between gap-4">
            <div>
              <Label htmlFor="eu-disabled" className="text-[13px]">
                Disable sign-in
              </Label>
              <p className="text-xs text-muted-foreground">
                Their remotes and job history are kept.
              </p>
            </div>
            <Switch id="eu-disabled" checked={disabled} onCheckedChange={setDisabled} />
          </div>
          {error && <p className="text-[13px] text-danger">{error}</p>}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" disabled={update.isPending}>
              Save changes
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function ResetPasswordDialog({ user, onClose }: { user: User; onClose: () => void }) {
  const reset = useMutation(UserService.method.resetPassword);
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | undefined>();
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (password.length < MIN_PASSWORD) {
      setError(`Use at least ${MIN_PASSWORD} characters.`);
      return;
    }
    try {
      await reset.mutateAsync({ userId: user.id, newPassword: password });
      toast.success(`Password reset for ${user.username}`);
      onClose();
    } catch (err) {
      setError(violationsOf(err).new_password ?? errorMessage(err));
    }
  };
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <form onSubmit={(e) => void submit(e)} noValidate className="flex flex-col gap-4">
          <DialogHeader>
            <DialogTitle>Reset the password of {user.username}</DialogTitle>
            <DialogDescription>
              They are signed out everywhere and sign in with the new password.
            </DialogDescription>
          </DialogHeader>
          <FieldShell
            id="rp-password"
            label="New password"
            error={error}
            help={`At least ${MIN_PASSWORD} characters.`}
          >
            <Input
              id="rp-password"
              type="password"
              autoComplete="new-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoFocus
            />
          </FieldShell>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" disabled={reset.isPending}>
              Reset password
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/** UsersPage lets administrators manage accounts. */
export function UsersPage() {
  const { user: me } = useSession();
  const queryClient = useQueryClient();
  const q = useQuery(UserService.method.listUsers, {});
  const del = useMutation(UserService.method.deleteUser);
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<User | undefined>();
  const [resetting, setResetting] = useState<User | undefined>();
  const [deleting, setDeleting] = useState<User | undefined>();

  const columns = useMemo(
    () => [
      col.accessor("username", {
        header: "User",
        cell: ({ row: { original: u } }) => (
          <div className="flex flex-col">
            <span className="font-medium">
              {u.displayName || u.username}
              {u.id === me?.id && (
                <span className="ml-1.5 text-xs font-normal text-muted-foreground">(you)</span>
              )}
            </span>
            <span className="text-xs text-muted-foreground">{u.username}</span>
          </div>
        ),
      }),
      col.accessor("role", {
        header: "Role",
        cell: (c) => (
          <span className="text-[13px]">
            {c.getValue() === UserRole.ADMIN ? "Administrator" : "User"}
          </span>
        ),
      }),
      col.accessor("disabled", {
        header: "Sign-in",
        cell: (c) =>
          c.getValue() ? (
            <span className="rounded-full bg-neutral-soft px-2 py-0.5 text-xs font-medium text-neutral">
              Disabled
            </span>
          ) : (
            <span className="text-[13px]">Allowed</span>
          ),
      }),
      col.accessor((u) => tsDate(u.lastLoginAt), {
        id: "lastLogin",
        header: "Last sign-in",
        cell: (c) => <RelativeTime date={c.getValue()} className="text-[13px]" />,
      }),
      col.accessor((u) => tsDate(u.createdAt), {
        id: "created",
        header: "Added",
        cell: (c) => <RelativeTime date={c.getValue()} className="text-[13px]" />,
      }),
      col.display({
        id: "actions",
        header: () => <span className="sr-only">Actions</span>,
        cell: ({ row: { original: u } }) => (
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${u.username}`}>
                <MoreHorizontal aria-hidden />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem onSelect={() => setEditing(u)}>Edit</DropdownMenuItem>
              <DropdownMenuItem onSelect={() => setResetting(u)}>Reset password</DropdownMenuItem>
              {u.id !== me?.id && (
                <>
                  <DropdownMenuSeparator />
                  <DropdownMenuItem variant="destructive" onSelect={() => setDeleting(u)}>
                    Delete
                  </DropdownMenuItem>
                </>
              )}
            </DropdownMenuContent>
          </DropdownMenu>
        ),
        meta: { className: "w-12 text-right" },
      }),
    ],
    [me?.id],
  );

  return (
    <>
      <PageHeader
        title="Users"
        description="Each user has their own remotes and jobs. Administrators can also see everyone's jobs and manage workers."
        actions={
          <Button onClick={() => setCreating(true)}>
            <Plus aria-hidden />
            Add user
          </Button>
        }
      />
      <DataTable
        aria-label="Users"
        columns={columns}
        data={q.data?.users}
        isLoading={q.isPending}
        error={q.error ?? undefined}
        onRetry={() => void q.refetch()}
        getRowId={(u) => u.id}
        empty={<EmptyState icon={UserRound} title="No users" />}
      />
      <CreateUserDialog open={creating} onOpenChange={setCreating} />
      {editing && <EditUserDialog user={editing} onClose={() => setEditing(undefined)} />}
      {resetting && (
        <ResetPasswordDialog user={resetting} onClose={() => setResetting(undefined)} />
      )}
      <ConfirmDialog
        open={!!deleting}
        onOpenChange={(o) => !o && setDeleting(undefined)}
        destructive
        title={`Delete ${deleting?.username ?? ""}?`}
        description="Only users without remotes or jobs can be deleted. To keep their history, disable the account instead."
        confirmLabel="Delete user"
        onConfirm={async () => {
          if (!deleting) return;
          await del.mutateAsync({ userId: deleting.id });
          await invalidateService(queryClient, UserService);
          toast.success(`Deleted ${deleting.username}`);
        }}
      />
    </>
  );
}
