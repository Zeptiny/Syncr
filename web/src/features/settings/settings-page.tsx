import { durationFromMs } from "@bufbuild/protobuf/wkt";
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { createColumnHelper } from "@tanstack/react-table";
import { KeyRound, Plus, Trash2, TriangleAlert } from "lucide-react";
import { useMemo, useState } from "react";
import { toast } from "sonner";

import { ConfirmDialog } from "@/components/app/confirm-dialog";
import { CodeBlock } from "@/components/app/copy-button";
import { DataTable } from "@/components/app/data-table";
import { FieldShell } from "@/components/app/descriptor-form";
import { EmptyState } from "@/components/app/empty-state";
import { KeyValueList } from "@/components/app/key-value-list";
import { PageHeader } from "@/components/app/page-header";
import { Panel } from "@/components/app/panel";
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
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { AuthService, type APIToken } from "@/gen/syncr/v1/auth_pb";
import { UserRole } from "@/gen/syncr/v1/users_pb";
import { useSession } from "@/hooks/use-session";
import { errorMessage, fieldViolations } from "@/lib/errors";
import { formatDate, tsDate } from "@/lib/format";
import { invalidateService } from "@/lib/invalidate";
import { MIN_PASSWORD } from "@/features/users/users-page";

const col = createColumnHelper<APIToken>();
const DAY = 86_400_000;
const expiries = [
  { value: String(30 * DAY), label: "30 days" },
  { value: String(90 * DAY), label: "90 days" },
  { value: String(365 * DAY), label: "1 year" },
  { value: "0", label: "Never" },
];

function PasswordPanel() {
  const change = useMutation(AuthService.method.changePassword);
  const [f, setF] = useState({ current: "", next: "", confirm: "" });
  const [errors, setErrors] = useState<Record<string, string>>({});
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    const local: Record<string, string> = {};
    if (!f.current) local.current = "Enter your current password.";
    if (f.next.length < MIN_PASSWORD) local.next = `Use at least ${MIN_PASSWORD} characters.`;
    else if (f.next !== f.confirm) local.confirm = "The passwords don't match.";
    setErrors(local);
    if (Object.keys(local).length) return;
    try {
      await change.mutateAsync({ currentPassword: f.current, newPassword: f.next });
      setF({ current: "", next: "", confirm: "" });
      toast.success("Password changed. Your other sessions were signed out.");
    } catch (err) {
      const v = Object.fromEntries(fieldViolations(err).map((x) => [x.field, x.message]));
      setErrors({
        current: v.current_password,
        next: v.new_password,
        form: Object.keys(v).length ? "" : errorMessage(err),
      });
    }
  };
  return (
    <Panel title="Password" description="Changing it signs you out on every other device.">
      <form onSubmit={(e) => void submit(e)} noValidate className="flex flex-col gap-4">
        <input type="text" autoComplete="username" hidden readOnly />
        <FieldShell id="pw-current" label="Current password" error={errors.current}>
          <Input
            id="pw-current"
            type="password"
            autoComplete="current-password"
            className="bg-card"
            value={f.current}
            onChange={(e) => setF({ ...f, current: e.target.value })}
          />
        </FieldShell>
        <FieldShell
          id="pw-next"
          label="New password"
          error={errors.next}
          help={`At least ${MIN_PASSWORD} characters. A short sentence works well.`}
        >
          <Input
            id="pw-next"
            type="password"
            autoComplete="new-password"
            className="bg-card"
            value={f.next}
            onChange={(e) => setF({ ...f, next: e.target.value })}
          />
        </FieldShell>
        <FieldShell id="pw-confirm" label="Repeat new password" error={errors.confirm}>
          <Input
            id="pw-confirm"
            type="password"
            autoComplete="new-password"
            className="bg-card"
            value={f.confirm}
            onChange={(e) => setF({ ...f, confirm: e.target.value })}
          />
        </FieldShell>
        {errors.form && <p className="text-[13px] text-danger">{errors.form}</p>}
        <div>
          <Button type="submit" disabled={change.isPending}>
            Change password
          </Button>
        </div>
      </form>
    </Panel>
  );
}

function CreateTokenDialog({
  open,
  onOpenChange,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onCreated: (secret: string, name: string) => void;
}) {
  const create = useMutation(AuthService.method.createAPIToken);
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [ttl, setTtl] = useState(expiries[1].value);
  const [error, setError] = useState<string | undefined>();
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!name.trim()) {
      setError("Name the token after what will use it.");
      return;
    }
    try {
      const res = await create.mutateAsync({
        name: name.trim(),
        ttl: ttl === "0" ? undefined : durationFromMs(Number(ttl)),
      });
      await invalidateService(queryClient, AuthService);
      onCreated(res.secret, name.trim());
      setName("");
      onOpenChange(false);
    } catch (err) {
      setError(fieldViolations(err)[0]?.message ?? errorMessage(err));
    }
  };
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <form onSubmit={(e) => void submit(e)} noValidate className="flex flex-col gap-4">
          <DialogHeader>
            <DialogTitle>Create an API token</DialogTitle>
            <DialogDescription>
              Scripts send it as{" "}
              <code className="rounded bg-muted px-1 text-xs">Authorization: Bearer …</code>. It
              acts as you, with your role.
            </DialogDescription>
          </DialogHeader>
          <FieldShell id="tok-name" label="Name" error={error}>
            <Input
              id="tok-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="Nightly script"
              autoFocus
            />
          </FieldShell>
          <FieldShell id="tok-ttl" label="Expires after">
            <Select value={ttl} onValueChange={setTtl}>
              <SelectTrigger id="tok-ttl" className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {expiries.map((x) => (
                  <SelectItem key={x.value} value={x.value}>
                    {x.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </FieldShell>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={create.isPending}>
              Create token
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/** TokenSecretDialog shows a new token's secret, once. */
export function TokenSecretDialog({
  secret,
  name,
  onClose,
}: {
  secret: string | undefined;
  name: string;
  onClose: () => void;
}) {
  return (
    <Dialog open={!!secret} onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Copy your new token</DialogTitle>
          <DialogDescription>{name}</DialogDescription>
        </DialogHeader>
        <div
          className="flex gap-2.5 rounded-lg bg-warning-soft px-3.5 py-3 text-[13px] leading-5"
          role="note"
        >
          <TriangleAlert className="mt-0.5 size-4 shrink-0 text-warning" aria-hidden />
          <span>
            <strong className="font-semibold">This is the only time it is shown.</strong> Store it
            somewhere safe; if you lose it, delete it and create a new one.
          </span>
        </div>
        {secret && <CodeBlock value={secret} label="API token" copyLabel="Copy token" />}
        <DialogFooter>
          <Button onClick={onClose}>I've copied it</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function TokensPanel() {
  const { viaToken } = useSession();
  const queryClient = useQueryClient();
  const q = useQuery(AuthService.method.listAPITokens, {});
  const del = useMutation(AuthService.method.deleteAPIToken);
  const [creating, setCreating] = useState(false);
  const [shown, setShown] = useState<{ secret: string; name: string } | undefined>();
  const [deleting, setDeleting] = useState<APIToken | undefined>();
  const columns = useMemo(
    () => [
      col.accessor("name", {
        header: "Name",
        cell: ({ row: { original: t } }) => (
          <div className="flex flex-col">
            <span className="font-medium">{t.name}</span>
            <span className="font-mono text-xs text-muted-foreground">{t.prefix}…</span>
          </div>
        ),
      }),
      col.accessor((t) => tsDate(t.lastUsedAt), {
        id: "used",
        header: "Last used",
        cell: (c) => <RelativeTime date={c.getValue()} className="text-[13px]" />,
      }),
      col.accessor((t) => tsDate(t.expiresAt), {
        id: "expires",
        header: "Expires",
        cell: (c) => (
          <span className="text-[13px]">{c.getValue() ? formatDate(c.getValue()) : "Never"}</span>
        ),
      }),
      col.accessor((t) => tsDate(t.createdAt), {
        id: "created",
        header: "Created",
        cell: (c) => <RelativeTime date={c.getValue()} className="text-[13px]" />,
      }),
      col.display({
        id: "actions",
        header: () => <span className="sr-only">Actions</span>,
        cell: ({ row }) =>
          viaToken ? null : (
            <Button
              variant="ghost"
              size="icon-sm"
              className="text-muted-foreground hover:text-danger"
              aria-label={`Delete ${row.original.name}`}
              onClick={() => setDeleting(row.original)}
            >
              <Trash2 aria-hidden />
            </Button>
          ),
        meta: { className: "w-12 text-right" },
      }),
    ],
    [viaToken],
  );
  return (
    <Panel
      flush
      title="API tokens"
      description="For scripts and automation that call the API as you."
      actions={
        !viaToken && (
          <Button size="sm" onClick={() => setCreating(true)}>
            <Plus aria-hidden />
            Create token
          </Button>
        )
      }
    >
      <DataTable
        aria-label="API tokens"
        className="rounded-none border-0"
        columns={columns}
        data={q.data?.tokens}
        isLoading={q.isPending}
        error={q.error ?? undefined}
        onRetry={() => void q.refetch()}
        getRowId={(t) => t.id}
        skeletonRows={2}
        empty={
          <EmptyState
            icon={KeyRound}
            title="No API tokens"
            description="Create one to call the API from curl, scripts or home automation."
          />
        }
      />
      <CreateTokenDialog
        open={creating}
        onOpenChange={setCreating}
        onCreated={(secret, name) => setShown({ secret, name })}
      />
      <TokenSecretDialog
        secret={shown?.secret}
        name={shown?.name ?? ""}
        onClose={() => setShown(undefined)}
      />
      <ConfirmDialog
        open={!!deleting}
        onOpenChange={(o) => !o && setDeleting(undefined)}
        destructive
        title={`Delete ${deleting?.name ?? "this token"}?`}
        description="Anything still using it stops working at once."
        confirmLabel="Delete token"
        onConfirm={async () => {
          if (!deleting) return;
          await del.mutateAsync({ tokenId: deleting.id });
          await invalidateService(queryClient, AuthService);
          toast.success(`Deleted ${deleting.name}`);
        }}
      />
    </Panel>
  );
}

/** SettingsPage holds the signed-in user's account settings. */
export function SettingsPage() {
  const { user, viaToken } = useSession();
  return (
    <>
      <PageHeader title="Settings" description="Your account on this controller." />
      <div className="flex flex-col gap-6">
        <TokensPanel />
        <div className="grid items-start gap-6 lg:grid-cols-2">
          <Panel title="Account">
            <KeyValueList
              items={[
                { label: "Username", value: user?.username, mono: true },
                { label: "Display name", value: user?.displayName || "–" },
                { label: "Role", value: user?.role === UserRole.ADMIN ? "Administrator" : "User" },
                { label: "Last sign-in", value: <RelativeTime date={tsDate(user?.lastLoginAt)} /> },
              ]}
            />
          </Panel>
          {!viaToken && <PasswordPanel />}
        </div>
      </div>
    </>
  );
}
