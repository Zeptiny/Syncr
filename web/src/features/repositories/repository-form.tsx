import type { MessageInitShape } from "@bufbuild/protobuf";
import { Eye, EyeOff, KeyRound } from "lucide-react";
import { useState } from "react";

import { ChoiceCards } from "@/components/app/choice-cards";
import { CopyButton } from "@/components/app/copy-button";
import { FieldShell } from "@/components/app/descriptor-form";
import { Panel } from "@/components/app/panel";
import { PathBrowserDialog } from "@/components/app/path-browser";
import { PathInput } from "@/components/app/path-input";
import { RemotePicker } from "@/components/app/remote-picker";
import { WorkerPicker } from "@/components/app/worker-picker";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import type { Worker } from "@/gen/syncr/v1/api_pb";
import type { Backend, Remote } from "@/gen/syncr/v1/remotes_pb";
import { RepositoryCreateMode, type RepositoryInputSchema } from "@/gen/syncr/v1/restic_pb";
import { errorMessage, fieldViolations } from "@/lib/errors";
import { FEATURE_RESTIC, hasFeature, isOnline, sortWorkers } from "@/lib/workers";

import { generatePassword } from "./model";

/** RepositoryFormState is the new-repository form. */
export interface RepositoryFormState {
  mode: RepositoryCreateMode;
  name: string;
  description: string;
  remoteId: string;
  path: string;
  password: string;
  /** confirm repeats the password when a new repository is created. */
  confirm: string;
  /** workerId is the worker that creates or opens it; "" lets the controller choose. */
  workerId: string;
}

/** RepositoryFormErrors are the form's problems, by field. */
export type RepositoryFormErrors = Partial<
  Record<
    "mode" | "name" | "description" | "remote" | "path" | "password" | "confirm" | "worker",
    string
  >
>;

/** emptyRepositoryForm creates a new repository in the given remote. */
export function emptyRepositoryForm(remoteId = ""): RepositoryFormState {
  return {
    mode: RepositoryCreateMode.INIT,
    name: "",
    description: "",
    remoteId,
    path: "",
    password: "",
    confirm: "",
    workerId: "",
  };
}

/** validateRepositoryForm checks what the browser can check before sending. */
export function validateRepositoryForm(
  s: RepositoryFormState,
  local: boolean,
): RepositoryFormErrors {
  const e: RepositoryFormErrors = {};
  if (!s.name.trim()) e.name = "Give the repository a name.";
  else if (s.name.trim().length > 64) e.name = "Use at most 64 characters.";
  if (!s.remoteId) e.remote = "Choose the remote that holds the repository.";
  if (!s.password) e.password = "Enter the repository's password.";
  else if (s.mode === RepositoryCreateMode.INIT && s.password !== s.confirm)
    e.confirm = "The two passwords differ.";
  if (local && !s.workerId) e.worker = "Choose the worker whose disk holds this repository.";
  return e;
}

/** toRepositoryInput builds the API input from the form. */
export function toRepositoryInput(
  s: RepositoryFormState,
): MessageInitShape<typeof RepositoryInputSchema> {
  return {
    name: s.name.trim(),
    description: s.description.trim(),
    remoteId: s.remoteId,
    path: s.path.trim().replace(/^\/+|\/+$/g, ""),
    password: s.password,
    mode: s.mode,
    workerId: s.workerId,
  };
}

const fieldOf: Record<string, keyof RepositoryFormErrors> = {
  name: "name",
  description: "description",
  remote_id: "remote",
  path: "path",
  password: "password",
  mode: "mode",
  worker_id: "worker",
};

/**
 * repositoryServerErrors places an API error on the form: field violations
 * on their fields (paths may carry an "input." prefix), a taken name or
 * location on the field it is about, and everything else as one message.
 */
export function repositoryServerErrors(err: unknown): {
  errors: RepositoryFormErrors;
  form?: string;
} {
  const errors: RepositoryFormErrors = {};
  const rest: string[] = [];
  for (const v of fieldViolations(err)) {
    const key = fieldOf[v.field.replace(/^input\./, "")];
    if (key) errors[key] = upper(v.message);
    else rest.push(`${v.field}: ${v.message}`);
  }
  if (Object.keys(errors).length || rest.length)
    return { errors, form: rest.length ? rest.join(" ") : undefined };
  const message = errorMessage(err);
  if (/with this name already exists/i.test(message)) return { errors: { name: message } };
  if (/at this location already exists|already one of your repositories/i.test(message))
    return { errors: { path: message } };
  return { errors, form: message };
}

function upper(s: string): string {
  return s ? s.charAt(0).toUpperCase() + s.slice(1) : s;
}

/** RepositoryFormProps configures RepositoryForm. */
export interface RepositoryFormProps {
  value: RepositoryFormState;
  onChange: (v: RepositoryFormState) => void;
  errors: RepositoryFormErrors;
  remotes: Remote[];
  backends: Map<string, Backend>;
  workers: Worker[];
  disabled?: boolean;
}

/** isLocalRemote reports whether a remote is a folder on a worker's own disk. */
export function isLocalRemote(remote: Remote | undefined, backends: Map<string, Backend>): boolean {
  return !!remote && !!backends.get(remote.backendType)?.capabilities?.local;
}

/** resticWorkers are the workers that can open a repository now. */
export function resticWorkers(workers: Worker[]): Worker[] {
  return sortWorkers(workers).filter((w) => isOnline(w) && hasFeature(w, FEATURE_RESTIC));
}

/**
 * RepositoryForm edits a new repository: whether to create or connect, its
 * name, where it lives, its password and which worker reaches it. The
 * password is only ever sent; it is never read back from the controller.
 */
export function RepositoryForm({
  value: s,
  onChange,
  errors,
  remotes,
  backends,
  workers,
  disabled,
}: RepositoryFormProps) {
  const [show, setShow] = useState(false);
  const [browsing, setBrowsing] = useState(false);
  const [generated, setGenerated] = useState(false);
  const set = <K extends keyof RepositoryFormState>(k: K, v: RepositoryFormState[K]) =>
    onChange({ ...s, [k]: v });
  const init = s.mode === RepositoryCreateMode.INIT;
  const remote = remotes.find((r) => r.id === s.remoteId);
  const local = isLocalRemote(remote, backends);
  const usable = resticWorkers(workers);

  return (
    <>
      <Panel title="Repository">
        <div className="flex flex-col gap-6">
          <div className="flex flex-col gap-1.5">
            <ChoiceCards
              label="What do you want to do?"
              labelId="repo-mode"
              className="sm:grid-cols-2"
              value={s.mode}
              onChange={(mode) => onChange({ ...s, mode, confirm: "" })}
              options={[
                {
                  value: RepositoryCreateMode.INIT,
                  label: "Create a new repository",
                  help: "Sets up an empty restic repository at the path, protected by the password you choose.",
                },
                {
                  value: RepositoryCreateMode.CONNECT,
                  label: "Connect to an existing one",
                  help: "Uses a repository that is already there. You need its password; nothing in it changes.",
                },
              ]}
            />
            {errors.mode && (
              <p role="alert" className="text-xs text-danger">
                {errors.mode}
              </p>
            )}
          </div>
          <div className="grid gap-5 md:grid-cols-[minmax(0,1fr)_minmax(0,1.4fr)]">
            <FieldShell id="repo-name" label="Name" error={errors.name}>
              <Input
                id="repo-name"
                className="bg-card"
                value={s.name}
                onChange={(e) => set("name", e.target.value)}
                placeholder="Photos vault"
                maxLength={64}
                autoComplete="off"
                disabled={disabled}
                aria-invalid={errors.name ? true : undefined}
              />
            </FieldShell>
            <FieldShell id="repo-desc" label="Description" optional error={errors.description}>
              <Input
                id="repo-desc"
                className="bg-card"
                value={s.description}
                onChange={(e) => set("description", e.target.value)}
                placeholder="What is backed up here"
                autoComplete="off"
                disabled={disabled}
              />
            </FieldShell>
          </div>
        </div>
      </Panel>

      <Panel title="Where it lives">
        <div className="grid gap-5 md:grid-cols-[minmax(0,1fr)_minmax(0,1.4fr)]">
          <FieldShell
            id="repo-remote"
            label="Remote"
            error={errors.remote}
            help="Only remotes that can hold a restic repository are listed."
          >
            <RemotePicker
              id="repo-remote"
              remotes={remotes}
              backends={backends}
              filter={(_r, b) => !!b?.capabilities?.resticRepository}
              value={s.remoteId}
              onChange={(remoteId) => {
                const next = remotes.find((r) => r.id === remoteId);
                // A repository on a worker's disk needs that worker: start with one.
                const workerId =
                  isLocalRemote(next, backends) && !s.workerId ? (usable[0]?.id ?? "") : s.workerId;
                onChange({ ...s, remoteId, workerId });
              }}
              invalid={!!errors.remote}
              disabled={disabled}
              className="w-full bg-card"
            />
          </FieldShell>
          <FieldShell
            id="repo-path"
            label="Path"
            error={errors.path}
            help={
              init
                ? "A folder inside the remote for this repository alone. It is created if missing. Leave empty to use the remote's root."
                : "The folder inside the remote that holds the repository (the one with config, keys and data)."
            }
          >
            <PathInput
              id="repo-path"
              value={s.path}
              onChange={(path) => set("path", path)}
              prefix={remote?.name}
              placeholder="restic/photos"
              invalid={!!errors.path}
              disabled={disabled}
              onBrowse={remote ? () => setBrowsing(true) : undefined}
            />
          </FieldShell>
        </div>
        {remote && (
          <PathBrowserDialog
            open={browsing}
            onOpenChange={setBrowsing}
            source={{ kind: "remote", remoteId: remote.id, workerId: s.workerId || undefined }}
            title={`Browse ${remote.name}`}
            rootLabel={remote.name}
            mode="dir"
            initialPath={s.path}
            onSelect={([path]) => path !== undefined && set("path", path)}
          />
        )}
      </Panel>

      <Panel
        title="Password"
        description="restic encrypts everything with this password. Syncr stores it encrypted and never shows it again."
      >
        <div className="flex flex-col gap-5">
          <div className="grid gap-5 md:grid-cols-2">
            <FieldShell id="repo-password" label="Password" error={errors.password}>
              <div className="flex gap-2">
                <Input
                  id="repo-password"
                  type={show ? "text" : "password"}
                  className="min-w-0 flex-1 bg-card font-mono text-[13px]"
                  value={s.password}
                  onChange={(e) => {
                    setGenerated(false);
                    set("password", e.target.value);
                  }}
                  autoComplete="new-password"
                  spellCheck={false}
                  disabled={disabled}
                  aria-invalid={errors.password ? true : undefined}
                />
                <Button
                  type="button"
                  variant="outline"
                  size="icon"
                  aria-label={show ? "Hide the password" : "Show the password"}
                  aria-pressed={show}
                  onClick={() => setShow((v) => !v)}
                >
                  {show ? <EyeOff aria-hidden /> : <Eye aria-hidden />}
                </Button>
              </div>
            </FieldShell>
            {init && (
              <FieldShell id="repo-confirm" label="Repeat the password" error={errors.confirm}>
                <Input
                  id="repo-confirm"
                  type={show ? "text" : "password"}
                  className="bg-card font-mono text-[13px]"
                  value={s.confirm}
                  onChange={(e) => set("confirm", e.target.value)}
                  autoComplete="new-password"
                  spellCheck={false}
                  disabled={disabled}
                  aria-invalid={errors.confirm ? true : undefined}
                />
              </FieldShell>
            )}
          </div>
          {init && (
            <div className="flex flex-wrap items-center gap-2">
              <Button
                type="button"
                variant="outline"
                size="sm"
                disabled={disabled}
                onClick={() => {
                  const password = generatePassword(24);
                  onChange({ ...s, password, confirm: password });
                  setGenerated(true);
                  setShow(true);
                }}
              >
                <KeyRound aria-hidden />
                Generate
              </Button>
              {generated && s.password && <CopyButton value={s.password} label="Copy password" />}
              <span className="text-xs text-muted-foreground">
                24 random letters and digits, made in your browser.
              </span>
            </div>
          )}
          <p className="rounded-md bg-warning-soft px-3 py-2 text-[13px] text-warning">
            Store this password safely; without it the backups cannot be read.
          </p>
        </div>
      </Panel>

      <Panel title="Run on">
        <div className="max-w-md">
          <FieldShell
            id="repo-worker"
            label="Worker"
            optional={!local}
            error={errors.worker}
            help={
              local
                ? "This remote is a folder on a worker's disk, so that worker has to do it."
                : init
                  ? "The worker that creates the repository. Automatic picks an online one."
                  : "The worker that opens the repository to check the password. Automatic picks an online one."
            }
          >
            <WorkerPicker
              id="repo-worker"
              workers={workers}
              value={s.workerId}
              onChange={(workerId) => set("workerId", workerId)}
              autoLabel={local ? undefined : "Automatic"}
              invalid={!!errors.worker}
              disabled={disabled}
              className="bg-card"
            />
          </FieldShell>
        </div>
      </Panel>
    </>
  );
}
