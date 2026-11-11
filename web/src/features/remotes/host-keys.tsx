import { Code, ConnectError } from "@connectrpc/connect";
import { useMutation } from "@connectrpc/connect-query";
import { CircleCheck, CircleX, KeyRound, ShieldAlert } from "lucide-react";
import { useState } from "react";
import { useFormContext } from "react-hook-form";

import { CodeBlock } from "@/components/app/copy-button";
import { TextField, type FieldValues } from "@/components/app/descriptor-form";
import { WorkerPicker } from "@/components/app/worker-picker";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import type { Worker } from "@/gen/syncr/v1/api_pb";
import type { SftpHostKey } from "@/gen/syncr/v1/link_pb";
import { ProbeService, type GetSftpHostKeysResponse } from "@/gen/syncr/v1/probe_pb";
import type { BackendField, Remote } from "@/gen/syncr/v1/remotes_pb";
import { errorMessage, fieldViolations } from "@/lib/errors";
import { cn } from "@/lib/utils";

/** HOST_KEYS_FIELD is the sftp option that pins the server's host keys. */
export const HOST_KEYS_FIELD = "host_keys";

/** COMPARE_COMMAND prints a host key's fingerprint on the server itself. */
export const COMPARE_COMMAND = "ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub";

/** hasHostKeys reports whether a host_keys value pins at least one key. */
export function hasHostKeys(value: string | undefined): boolean {
  return !!value && value.split(/[\n,]/).some((l) => l.trim() !== "");
}

/**
 * isUnverified reports whether a remote reaches an SSH server without
 * pinned host keys: an sftp remote whose host_keys option is empty. The
 * option is not a secret, so the remote list has it.
 */
export function isUnverified(remote: Pick<Remote, "backendType" | "options">): boolean {
  return remote.backendType === "sftp" && !hasHostKeys(remote.options[HOST_KEYS_FIELD]);
}

/** hostKeysValue is the field value for fetched keys: one "algorithm key" per line. */
export function hostKeysValue(keys: SftpHostKey[]): string {
  return keys.map((k) => `${k.algorithm} ${k.key}`).join("\n");
}

/** keyLines splits a host_keys value into normalized "algorithm key" entries. */
function keyLines(value: string): string[] {
  return value
    .split(/[\n,]/)
    .map((l) => l.trim().split(/\s+/).join(" "))
    .filter(Boolean);
}

/** sameKeys reports whether the field already pins exactly these keys. */
export function sameKeys(value: string, keys: SftpHostKey[]): boolean {
  const have = new Set(keyLines(value));
  return (
    keys.length > 0 &&
    have.size === keys.length &&
    keys.every((k) => have.has(`${k.algorithm} ${k.key}`))
  );
}

/** UnverifiedWarning says what an sftp remote without host keys risks. */
export function UnverifiedWarning({ className }: { className?: string }) {
  return (
    <div
      role="note"
      data-testid="server-not-verified"
      className={cn(
        "flex gap-2.5 rounded-lg bg-warning-soft px-3.5 py-3 text-[13px] leading-5",
        className,
      )}
    >
      <ShieldAlert className="mt-0.5 size-4 shrink-0 text-warning" aria-hidden />
      <div>
        <strong className="font-semibold">Server not verified.</strong> Without host keys, the
        worker accepts any server that answers at this address, so a machine in between could pose
        as it and see the password and the files. Fetch the keys and compare them with the
        server&apos;s own. You can still save without them.
      </div>
    </div>
  );
}

/** UnverifiedBadge marks an unverified sftp remote in lists. */
export function UnverifiedBadge() {
  return (
    <span
      className="inline-flex items-center gap-1 text-xs font-medium text-warning"
      title="This SFTP remote has no host keys: the worker cannot tell its server from an impostor. Edit it and fetch the keys."
    >
      <ShieldAlert className="size-3.5" aria-hidden />
      Server not verified
    </span>
  );
}

interface FetchProblem {
  title: string;
  message: string;
  hint?: string;
}

/** problemOf turns a GetSftpHostKeys error into what the form shows. */
function problemOf(err: unknown): FetchProblem {
  const violations = fieldViolations(err);
  if (violations.length) {
    return {
      title: violations.some((v) => v.field === "worker_id")
        ? "Choose a worker"
        : "Check the host and port",
      message: violations
        .map((v) =>
          v.field === "host"
            ? `Host: ${v.message}`
            : v.field === "port"
              ? `Port: ${v.message}`
              : v.message,
        )
        .join(" "),
    };
  }
  if (err instanceof ConnectError && err.code === Code.FailedPrecondition) {
    return {
      title: "Could not read the server's keys",
      message: errorMessage(err),
      hint: "Check the host and port, and that the worker can reach the server. You can try through another worker.",
    };
  }
  if (err instanceof ConnectError && err.code === Code.Unavailable) {
    return {
      title: "The worker did not answer",
      message: errorMessage(err),
      hint: "Try again, or through another worker.",
    };
  }
  return { title: "The fetch could not run", message: errorMessage(err) };
}

/** HostKeysFieldProps configures HostKeysField. */
export interface HostKeysFieldProps {
  field: BackendField;
  idPrefix: string;
  /** workers lets the user choose who connects to the server. */
  workers: Worker[];
}

/**
 * HostKeysField is the sftp host_keys field with "Fetch from server": a
 * worker connects to the form's host and port, reads the keys the server
 * offers (no credentials are sent) and shows their SHA-256 fingerprints to
 * compare with the server's own; "Use these keys" fills the field. Without
 * keys it warns that the server is not verified, and saving still works.
 */
export function HostKeysField({ field, idPrefix, workers }: HostKeysFieldProps) {
  const form = useFormContext<FieldValues>();
  const path = `values.${field.name}` as const;
  const value = form.watch(path);
  const text = typeof value === "string" ? value : "";
  const fetchKeys = useMutation(ProbeService.method.getSftpHostKeys);
  const [result, setResult] = useState<GetSftpHostKeysResponse | undefined>();
  const [problem, setProblem] = useState<FetchProblem | undefined>();
  const [workerId, setWorkerId] = useState("");
  const [askWorker, setAskWorker] = useState(false);

  const run = async () => {
    setResult(undefined);
    setProblem(undefined);
    if (!(await form.trigger(["values.host", "values.port"]))) {
      setProblem({
        title: "Check the host and port",
        message: "Enter the server's host (and port, if it is not 22) first.",
      });
      return;
    }
    const host = String(form.getValues("values.host") ?? "").trim();
    const portText = String(form.getValues("values.port") ?? "").trim();
    try {
      const res = await fetchKeys.mutateAsync({
        host,
        port: portText === "" ? 0 : Number(portText),
        workerId,
      });
      setResult(res);
    } catch (err) {
      const violations = fieldViolations(err);
      for (const v of violations) {
        if (v.field === "host" || v.field === "port")
          form.setError(`values.${v.field}`, { type: "server", message: v.message });
      }
      if (
        violations.some((v) => v.field === "worker_id") ||
        (err instanceof ConnectError &&
          (err.code === Code.FailedPrecondition || err.code === Code.Unavailable))
      ) {
        setAskWorker(true);
      }
      setProblem(problemOf(err));
    }
  };

  const keys = result?.keys ?? [];
  const used = !!result && sameKeys(text, keys);
  return (
    <div className="flex flex-col gap-3">
      <TextField field={field} idPrefix={idPrefix} />
      {!hasHostKeys(text) && <UnverifiedWarning />}
      <div className="flex flex-wrap items-center gap-2">
        <Button
          type="button"
          variant="outline"
          disabled={fetchKeys.isPending}
          onClick={() => void run()}
        >
          <KeyRound aria-hidden />
          {fetchKeys.isPending ? "Fetching…" : "Fetch from server"}
        </Button>
        {askWorker && (
          <div className="w-full sm:w-64">
            <WorkerPicker
              workers={workers}
              value={workerId}
              onChange={setWorkerId}
              autoLabel="Any online worker"
              aria-label="Fetch through"
              className="bg-card"
            />
          </div>
        )}
        {!result && !problem && !fetchKeys.isPending && (
          <span className="text-xs text-muted-foreground">
            A worker connects to the host and port above and reads the keys the server offers. No
            password is sent.
          </span>
        )}
      </div>
      {result && (
        <Alert data-testid="host-keys-result">
          <CircleCheck className="text-success" aria-hidden />
          <AlertTitle className="line-clamp-none">
            {keys.length === 0
              ? "The server offered no keys"
              : `The server offered ${keys.length === 1 ? "1 key" : `${keys.length} keys`}`}
            {result.worker?.name ? ` (through ${result.worker.name})` : ""}
          </AlertTitle>
          <AlertDescription className="w-full">
            {keys.length > 0 && (
              <>
                <ul className="flex w-full flex-col gap-1.5" aria-label="Fetched host keys">
                  {keys.map((k) => (
                    <li
                      key={`${k.algorithm} ${k.key}`}
                      className="flex w-full flex-col gap-0.5 rounded-md bg-muted px-3 py-2 sm:flex-row sm:items-baseline sm:gap-3"
                    >
                      <span className="shrink-0 font-mono text-[12.5px] text-foreground sm:w-44">
                        {k.algorithm}
                      </span>
                      <span className="font-mono text-[12.5px] break-all text-foreground">
                        {k.fingerprintSha256}
                      </span>
                    </li>
                  ))}
                </ul>
                <p className="mt-1">
                  Before you use them, compare the fingerprints with the server&apos;s own. On the
                  server, run:
                </p>
                <CodeBlock
                  value={COMPARE_COMMAND}
                  label="Shell"
                  copyLabel="Copy command"
                  className="w-full"
                />
                <p className="text-xs">
                  It prints the key&apos;s size, then its <code>SHA256:</code> fingerprint. For the
                  other key types, use <code>ssh_host_ecdsa_key.pub</code> or{" "}
                  <code>ssh_host_rsa_key.pub</code>. A storage provider usually publishes its
                  fingerprints.
                </p>
                <div className="mt-1 flex flex-wrap items-center gap-3">
                  <Button
                    type="button"
                    size="sm"
                    disabled={used}
                    onClick={() =>
                      form.setValue(path, hostKeysValue(keys), {
                        shouldDirty: true,
                        shouldTouch: true,
                        shouldValidate: true,
                      })
                    }
                  >
                    Use these keys
                  </Button>
                  {used && (
                    <span className="text-xs text-success">
                      The field holds these keys. Save to pin them.
                    </span>
                  )}
                </div>
              </>
            )}
          </AlertDescription>
        </Alert>
      )}
      {problem && (
        <Alert variant="destructive" data-testid="host-keys-error">
          <CircleX aria-hidden />
          <AlertTitle>{problem.title}</AlertTitle>
          <AlertDescription>
            <p>{problem.message}</p>
            {problem.hint && <p>{problem.hint}</p>}
          </AlertDescription>
        </Alert>
      )}
    </div>
  );
}
