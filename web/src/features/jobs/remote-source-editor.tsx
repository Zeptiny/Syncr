import { ChevronRight } from "lucide-react";
import { useState } from "react";

import { ChoiceCards } from "@/components/app/choice-cards";
import { FieldShell } from "@/components/app/descriptor-form";
import { FieldWarning } from "@/components/app/field-warning";
import { PathBrowserDialog } from "@/components/app/path-browser";
import { PathInput } from "@/components/app/path-input";
import { RemotePicker } from "@/components/app/remote-picker";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { WorkerStatus, type Worker } from "@/gen/syncr/v1/api_pb";
import { ResticRemoteSourceMethod } from "@/gen/syncr/v1/link_pb";
import type { Backend, Remote } from "@/gen/syncr/v1/remotes_pb";
import type { Repository } from "@/gen/syncr/v1/restic_pb";
import { methodRefusal } from "@/lib/remote-sources";
import { cn } from "@/lib/utils";

import type { DefinitionErrors } from "./definition";
import { isMirror, repositoryOverlap, type RemoteSourceState } from "./remote-source";
import { NumberInput, QuantityInput, Toggle } from "./transfer-editor";
import { byteUnits } from "./units";

/** LOCAL_REMOTE_REASON is the controller's refusal of a Local directory remote as a source. */
export const LOCAL_REMOTE_REASON =
  "this remote is a folder on a worker; back up that folder instead";

const isLocal = (b: Backend | undefined) => !!b?.capabilities?.local;

/** RemoteFolderFields picks the remote and the folder of a backup of a remote. */
export function RemoteFolderFields({
  value: s,
  onChange,
  errors,
  remotes,
  backends,
  worker,
  repository,
  idPrefix,
}: {
  value: RemoteSourceState;
  onChange: (v: RemoteSourceState) => void;
  errors: DefinitionErrors;
  remotes: Remote[];
  backends: Map<string, Backend>;
  /** worker browses the folder when it is online (otherwise the controller picks one). */
  worker?: Worker;
  /** repository is the one backed up into: a folder that holds it, or is in it, is refused. */
  repository?: Repository;
  idPrefix: string;
}) {
  const [browsing, setBrowsing] = useState(false);
  const remote = remotes.find((r) => r.id === s.remoteId);
  const local = remote ? isLocal(backends.get(remote.backendType)) : false;
  const online = worker?.status === WorkerStatus.ONLINE ? worker : undefined;
  const overlap = repositoryOverlap(s, repository);
  const remoteId = `${idPrefix}-source-remote`;
  const folderId = `${idPrefix}-source-folder`;
  return (
    <div className="flex flex-col gap-5">
      <FieldShell
        id={remoteId}
        label="Remote"
        error={errors.sourceRemoteId}
        help={
          remotes.length === 0
            ? "You have no remote yet: add one on the Remotes page first."
            : "One of your remotes. A Local directory remote is a folder on a worker: back that folder up from “Folders on a worker” instead."
        }
      >
        <RemotePicker
          id={remoteId}
          remotes={remotes}
          backends={backends}
          value={s.remoteId}
          onChange={(remoteId) => onChange({ ...s, remoteId })}
          unavailable={(_, b) => (isLocal(b) ? LOCAL_REMOTE_REASON : undefined)}
          invalid={!!errors.sourceRemoteId}
          className="w-full bg-card"
        />
        {local && (
          <FieldWarning>
            {remote?.name} is a folder on a worker: back that folder up from “Folders on a worker”
            instead.
          </FieldWarning>
        )}
      </FieldShell>
      <FieldShell
        id={folderId}
        label="Folder"
        optional
        error={errors.sourceFolder}
        help="Relative to the remote's root. Leave it empty to back up the whole remote."
      >
        <PathInput
          id={folderId}
          value={s.path}
          onChange={(path) => onChange({ ...s, path })}
          prefix={remote?.name}
          placeholder="Photos"
          invalid={!!errors.sourceFolder}
          onBrowse={remote && !local ? () => setBrowsing(true) : undefined}
          aria-describedby={errors.sourceFolder ? `${folderId}-error` : `${folderId}-help`}
        />
        {overlap && !errors.sourceFolder && <FieldWarning>{overlap}</FieldWarning>}
        {remote && !local && (
          <PathBrowserDialog
            open={browsing}
            onOpenChange={setBrowsing}
            source={{ kind: "remote", remoteId: remote.id, workerId: online?.id }}
            title={`Browse ${remote.name}`}
            rootLabel={remote.name}
            mode="dir"
            initialPath={s.path}
            onSelect={([path]) =>
              path !== undefined && onChange({ ...s, path: path.replace(/^\/+/, "") })
            }
          />
        )}
      </FieldShell>
    </div>
  );
}

/** methodHelp is the design's comparison of the two methods, short. */
const methodHelp: Record<"mirror" | "mount", string> = {
  mirror:
    "Keeps a copy of the folder on the worker and brings it up to date before each backup. Needs disk space for the folder (after filters); works on every worker, also with remotes that don't keep modification times. An interrupted run's copy is reused.",
  mount:
    "Reads the folder through rclone mount: no disk space on the worker. Needs FUSE 3 on Linux or WinFsp on Windows. With remotes that don't keep modification times, every file is read again each run.",
};

/**
 * RemoteMethodFields chooses how the worker reads the folder (mirror or
 * mount) and the options of that: deleting the mirror after each run,
 * and under Advanced the download options and empty folders. A method
 * the worker cannot use is disabled, with the worker's reason.
 */
export function RemoteMethodFields({
  value: s,
  onChange,
  errors,
  worker,
  idPrefix,
}: {
  value: RemoteSourceState;
  onChange: (v: RemoteSourceState) => void;
  errors: DefinitionErrors;
  /** worker is the job's worker: what it reported decides which methods it can use. */
  worker?: Worker;
  idPrefix: string;
}) {
  const set = <K extends keyof RemoteSourceState>(k: K, v: RemoteSourceState[K]) =>
    onChange({ ...s, [k]: v });
  const mirror = isMirror(s);
  const refusal = {
    mirror: methodRefusal(worker, ResticRemoteSourceMethod.MIRROR),
    mount: methodRefusal(worker, ResticRemoteSourceMethod.MOUNT),
  };
  const chosenRefusal = mirror ? refusal.mirror : refusal.mount;
  const advancedSet =
    s.transfers !== "" || s.bwlimit !== "" || s.fastList || s.allowEmpty || !!errors.sourceOptions;
  const [more, setMore] = useState(advancedSet);
  const note = (why: string | undefined) => why && <span className="text-warning">{why}</span>;
  return (
    <div className="flex flex-col gap-5">
      <ChoiceCards
        labelId={`${idPrefix}-method`}
        label="Method"
        value={s.method}
        onChange={(method) => set("method", method)}
        className="md:grid-cols-2"
        options={[
          {
            value: ResticRemoteSourceMethod.MIRROR,
            label: (
              <>
                Mirror <span className="font-normal text-muted-foreground">(default)</span>
              </>
            ),
            help: methodHelp.mirror,
            disabled: !!refusal.mirror && mirror === false,
            note: note(refusal.mirror),
            children: (
              <Toggle
                id={`${idPrefix}-discard`}
                label="Delete the mirror after each run"
                help="Less disk on the worker, but the whole folder is downloaded every run. restic still reads only what changed."
                checked={s.discardMirror}
                onChange={(v) => set("discardMirror", v)}
              />
            ),
          },
          {
            value: ResticRemoteSourceMethod.MOUNT,
            label: "Mount",
            help: methodHelp.mount,
            disabled: !!refusal.mount && mirror,
            note: note(refusal.mount),
          },
        ]}
      />
      {chosenRefusal && (
        <FieldWarning>
          The job would fail: {chosenRefusal.replace(/\.$/, "")}. Choose the other method or another
          worker.
        </FieldWarning>
      )}
      {errors.sourceMethod && (
        <p role="alert" className="text-xs leading-5 text-danger">
          {errors.sourceMethod}
        </p>
      )}

      <Collapsible
        open={more || !!errors.sourceOptions}
        onOpenChange={setMore}
        className="rounded-lg border"
      >
        <CollapsibleTrigger asChild>
          <button
            type="button"
            className="flex w-full items-center gap-2 rounded-lg px-4 py-3 text-left text-[13px] font-medium hover:bg-accent/50"
          >
            <ChevronRight
              className={cn("size-4 transition-transform", more && "rotate-90")}
              aria-hidden
            />
            Advanced
            {s.allowEmpty && (
              <span className="font-normal text-muted-foreground">empty folder allowed</span>
            )}
          </button>
        </CollapsibleTrigger>
        <CollapsibleContent>
          <div className="flex flex-col gap-6 border-t px-4 py-5">
            <div className="grid gap-4 sm:grid-cols-2">
              <NumberInput
                id={`${idPrefix}-source-transfers`}
                label="Parallel downloads"
                value={mirror ? s.transfers : ""}
                onChange={(v) => set("transfers", v)}
                placeholder={mirror ? "4" : "Mirror only"}
                help={mirror ? "rclone's transfers while mirroring." : "Mirror only."}
              />
              <QuantityInput
                id={`${idPrefix}-source-bw`}
                label="Bandwidth limit"
                value={s.bwlimit}
                unit={s.bwlimitUnit}
                units={byteUnits.map((u) => ({ id: u.id, label: `${u.id}/s` }))}
                onChange={(bwlimit, bwlimitUnit) => onChange({ ...s, bwlimit, bwlimitUnit })}
                placeholder="Unlimited"
                help="For reading the remote, with either method."
              />
            </div>
            <div className="grid gap-x-8 gap-y-4 md:grid-cols-2">
              <Toggle
                id={`${idPrefix}-source-fast`}
                label="Fast listing"
                help={mirror ? "Fewer API calls on object stores, more memory." : "Mirror only."}
                checked={s.fastList}
                onChange={(v) => set("fastList", v)}
                disabled={!mirror}
              />
              <Toggle
                id={`${idPrefix}-allow-empty`}
                label="Back up an empty folder"
                help="Off: an empty folder fails the run, so an emptied or wrong folder never pushes real snapshots out of retention. On: an empty folder ends the run without a snapshot."
                checked={s.allowEmpty}
                onChange={(v) => set("allowEmpty", v)}
              />
            </div>
            {errors.sourceOptions && (
              <p role="alert" className="text-xs leading-5 text-danger">
                {errors.sourceOptions}
              </p>
            )}
          </div>
        </CollapsibleContent>
      </Collapsible>
    </div>
  );
}
