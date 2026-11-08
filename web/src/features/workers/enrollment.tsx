import { durationFromMs } from "@bufbuild/protobuf/wkt";
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { KeyRound } from "lucide-react";
import { useState } from "react";

import { CopyCommand } from "@/components/app/copy-command";
import { FieldShell } from "@/components/app/descriptor-form";
import { RelativeTime } from "@/components/app/relative-time";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { WorkerAdminService, type CreateEnrollmentTokenResponse } from "@/gen/syncr/v1/api_pb";
import { errorMessage, fieldViolations } from "@/lib/errors";
import { tsDate } from "@/lib/format";
import { invalidateService } from "@/lib/invalidate";

const ttls = [
  { value: String(15 * 60_000), label: "15 minutes" },
  { value: String(60 * 60_000), label: "1 hour" },
  { value: String(24 * 60 * 60_000), label: "24 hours" },
  { value: String(7 * 24 * 60 * 60_000), label: "7 days" },
];

/**
 * ActiveEnrollment creates a one-time enrollment token and shows the
 * install commands that use it. Active workers connect out to this
 * controller, so they work behind NAT.
 */
export function ActiveEnrollment() {
  const [ttl, setTtl] = useState(ttls[1].value);
  const [result, setResult] = useState<CreateEnrollmentTokenResponse | undefined>();
  const create = useMutation(WorkerAdminService.method.createEnrollmentToken);

  const generate = async () => {
    const res = await create.mutateAsync({
      ttl: durationFromMs(Number(ttl)),
      controllerUrl: window.location.origin,
    });
    setResult(res);
  };

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end gap-3">
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="enroll-ttl" className="text-[13px]">
            Token valid for
          </Label>
          <Select value={ttl} onValueChange={setTtl}>
            <SelectTrigger id="enroll-ttl" className="w-44 bg-card">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {ttls.map((t) => (
                <SelectItem key={t.value} value={t.value}>
                  {t.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <Button onClick={() => void generate()} disabled={create.isPending}>
          <KeyRound aria-hidden />
          {result ? "Generate a new command" : "Generate install command"}
        </Button>
      </div>
      {create.error && (
        <p role="alert" className="text-[13px] text-danger">
          {errorMessage(create.error)}
        </p>
      )}
      {result && (
        <CopyCommand
          commands={result.installCommands}
          shownOnce={
            <>
              <strong className="font-semibold">
                Copy it now: this command is shown only once.
              </strong>{" "}
              It contains a single-use enrollment token that expires{" "}
              <RelativeTime date={tsDate(result.expiresAt)} />. The worker shows up in the list as
              soon as it connects.
            </>
          }
        />
      )}
    </div>
  );
}

/**
 * PassiveSetup shows the passive install commands and registers the worker
 * from the URL, fingerprint and pairing secret its setup printed.
 */
export function PassiveSetup() {
  const commands = useQuery(WorkerAdminService.method.getPassiveInstallCommands, {});
  const register = useMutation(WorkerAdminService.method.registerPassiveWorker);
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [form, setForm] = useState({ name: "", url: "", certFingerprint: "", pairingSecret: "" });
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [formError, setFormError] = useState<string | undefined>();
  const set = (k: keyof typeof form) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setForm((f) => ({ ...f, [k]: e.target.value }));

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setErrors({});
    setFormError(undefined);
    const missing: Record<string, string> = {};
    if (!form.name.trim()) missing.name = "Give the worker a name.";
    if (!form.url.trim()) missing.url = "Paste the URL the worker printed.";
    if (!form.certFingerprint.trim())
      missing.cert_fingerprint = "Paste the certificate fingerprint.";
    if (!form.pairingSecret.trim()) missing.pairing_secret = "Paste the pairing secret.";
    if (Object.keys(missing).length) {
      setErrors(missing);
      return;
    }
    try {
      const res = await register.mutateAsync({
        name: form.name.trim(),
        url: form.url.trim(),
        certFingerprint: form.certFingerprint.trim(),
        pairingSecret: form.pairingSecret.trim(),
      });
      await invalidateService(queryClient, WorkerAdminService);
      if (res.worker)
        await navigate({ to: "/workers/$workerId", params: { workerId: res.worker.id } });
    } catch (err) {
      const v = fieldViolations(err);
      if (v.length) setErrors(Object.fromEntries(v.map((x) => [x.field, x.message])));
      else setFormError(errorMessage(err));
    }
  };

  return (
    <ol className="flex flex-col gap-8">
      <li className="grid gap-4 md:grid-cols-[2rem_minmax(0,1fr)]">
        <StepNumber n={1} />
        <div className="flex min-w-0 flex-col gap-3">
          <div>
            <h3 className="text-[15px] font-semibold">Install the worker in passive mode</h3>
            <p className="mt-0.5 text-[13px] text-muted-foreground">
              The controller will connect to it on port 7443, so that port must be reachable from
              here.
            </p>
          </div>
          {commands.data ? (
            <CopyCommand commands={commands.data.commands} />
          ) : commands.error ? (
            <p className="text-[13px] text-danger">{errorMessage(commands.error)}</p>
          ) : (
            <div className="h-32 animate-pulse rounded-lg bg-muted" />
          )}
        </div>
      </li>
      <li className="grid gap-4 md:grid-cols-[2rem_minmax(0,1fr)]">
        <StepNumber n={2} />
        <form onSubmit={(e) => void submit(e)} noValidate className="flex min-w-0 flex-col gap-4">
          <div>
            <h3 className="text-[15px] font-semibold">Register it here</h3>
            <p className="mt-0.5 text-[13px] text-muted-foreground">
              Enter what the setup command printed.
            </p>
          </div>
          <div className="grid gap-4 md:grid-cols-2">
            <FieldShell id="pw-name" label="Name" error={errors.name}>
              <Input
                id="pw-name"
                className="bg-card"
                value={form.name}
                onChange={set("name")}
                placeholder="backup-host"
              />
            </FieldShell>
            <FieldShell id="pw-url" label="URL" error={errors.url}>
              <Input
                id="pw-url"
                className="bg-card font-mono text-[13px]"
                value={form.url}
                onChange={set("url")}
                placeholder="https://backup-host.lan:7443"
              />
            </FieldShell>
            <FieldShell id="pw-fp" label="Certificate fingerprint" error={errors.cert_fingerprint}>
              <Input
                id="pw-fp"
                className="bg-card font-mono text-[13px]"
                value={form.certFingerprint}
                onChange={set("certFingerprint")}
                placeholder="sha256:…"
              />
            </FieldShell>
            <FieldShell id="pw-secret" label="Pairing secret" error={errors.pairing_secret}>
              <Input
                id="pw-secret"
                type="password"
                autoComplete="off"
                className="bg-card font-mono text-[13px]"
                value={form.pairingSecret}
                onChange={set("pairingSecret")}
              />
            </FieldShell>
          </div>
          {formError && (
            <p role="alert" className="rounded-md bg-danger-soft px-3 py-2 text-[13px] text-danger">
              {formError}
            </p>
          )}
          <div>
            <Button type="submit" disabled={register.isPending}>
              {register.isPending ? "Registering…" : "Register worker"}
            </Button>
          </div>
        </form>
      </li>
    </ol>
  );
}

/** StepNumber marks a step in a numbered sequence. */
export function StepNumber({ n, done }: { n: number; done?: boolean }) {
  return (
    <span
      className={
        done
          ? "grid size-8 place-items-center rounded-full bg-primary text-[13px] font-semibold text-primary-foreground"
          : "grid size-8 place-items-center rounded-full border bg-card text-[13px] font-semibold text-foreground"
      }
      aria-hidden
    >
      {n}
    </span>
  );
}
