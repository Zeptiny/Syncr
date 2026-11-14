import { ChipsInput } from "@/components/app/chips-input";
import { ChoiceCards } from "@/components/app/choice-cards";
import { FieldShell } from "@/components/app/descriptor-form";
import { FieldWarning } from "@/components/app/field-warning";
import { Panel } from "@/components/app/panel";
import { PathsEditor } from "@/components/app/paths-editor";
import { Input } from "@/components/ui/input";
import { WorkerStatus, type Worker } from "@/gen/syncr/v1/api_pb";
import type { Backend, Remote } from "@/gen/syncr/v1/remotes_pb";
import type { Repository } from "@/gen/syncr/v1/restic_pb";
import { allowedFoldersHelp, allowedFoldersWarning } from "@/lib/allowed-folders";
import { REMOTE_TAG_PREFIX } from "@/lib/remote-sources";
import { isWindowsOS } from "@/lib/restic-paths";

import type { DefinitionErrors } from "./definition";
import { FilterRuleList, PatternList } from "./pattern-list";
import { RemoteFolderFields, RemoteMethodFields } from "./remote-source-editor";
import { RepositoryField } from "./repository-field";
import type { BackupSource, BackupState } from "./restic-definition";
import { NumberInput, Toggle } from "./transfer-editor";

/** BackupEditorProps configures BackupEditor. */
export interface BackupEditorProps {
  value: BackupState;
  onChange: (v: BackupState) => void;
  errors: DefinitionErrors;
  repositories: Repository[];
  /** remotes and backends offer the folder of a remote to back up. */
  remotes: Remote[];
  backends: Map<string, Backend>;
  /**
   * worker is whose folders are backed up: the job's worker, or for a
   * label target the first online worker with the label. Browse opens its
   * folders, and its OS decides whether VSS is offered.
   */
  worker?: Worker;
  /**
   * jobWorker is the worker chosen for the job, never a label's stand-in:
   * what it reported decides which ways of reading a remote it offers.
   */
  jobWorker?: Worker;
  /** workerNote replaces the help under the paths (see allowedFoldersHelp). */
  workerNote?: string;
  /**
   * runsOn are the workers the job may run on that have reported in: a
   * warning names paths outside their allowed folders. Default: worker.
   */
  runsOn?: Worker[];
  idPrefix: string;
}

const sources: { value: BackupSource; label: string; help: string }[] = [
  {
    value: "paths",
    label: "Folders on a worker",
    help: "Files and folders on the disks of the worker that runs the job.",
  },
  {
    value: "remote",
    label: "A folder on a remote",
    help: "A folder of one of your remotes, which the worker reads for restic.",
  },
];

/**
 * BackupEditor edits a backup: the repository, what it reads (folders on
 * the worker, or a folder of a remote and how the worker reads it) and the
 * options. Both sources keep what was typed in them while the other is
 * shown; only the chosen one is sent.
 */
export function BackupEditor({
  value: s,
  onChange,
  errors,
  repositories,
  remotes,
  backends,
  worker,
  jobWorker,
  workerNote,
  runsOn,
  idPrefix,
}: BackupEditorProps) {
  const set = <K extends keyof BackupState>(k: K, v: BackupState[K]) => onChange({ ...s, [k]: v });
  const remote = s.source === "remote";
  const windows = isWindowsOS(worker?.info?.os);
  const online = worker?.status === WorkerStatus.ONLINE;
  const warning = remote
    ? undefined
    : allowedFoldersWarning(s.paths, runsOn ?? (worker?.info ? [worker] : []));
  const repository = repositories.find((r) => r.id === s.repositoryId);
  return (
    <>
      <Panel title="What to back up">
        <div className="flex flex-col gap-6">
          <ChoiceCards
            labelId={`${idPrefix}-source`}
            label="Source"
            value={s.source}
            onChange={(v) => set("source", v)}
            options={sources}
            className="sm:grid-cols-2"
          />
          <div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_minmax(0,1.4fr)]">
            <RepositoryField
              id={`${idPrefix}-repo`}
              repositories={repositories}
              value={s.repositoryId}
              onChange={(v) => set("repositoryId", v)}
              error={errors.repository}
            />
            {remote ? (
              <RemoteFolderFields
                value={s.remote}
                onChange={(v) => set("remote", v)}
                errors={errors}
                remotes={remotes}
                backends={backends}
                worker={jobWorker ?? worker}
                repository={repository}
                idPrefix={idPrefix}
              />
            ) : (
              <FieldShell
                id={`${idPrefix}-paths`}
                label="Folders and files"
                error={errors.paths}
                help={allowedFoldersHelp(worker, workerNote, true)}
              >
                <PathsEditor
                  id={`${idPrefix}-paths`}
                  label="Path to back up"
                  value={s.paths}
                  onChange={(v) => set("paths", v)}
                  placeholder={windows ? "C:\\Users\\ada\\Documents" : "/srv/data"}
                  suggestions={worker?.info?.localRoots}
                  invalid={!!errors.paths}
                  browse={
                    worker && online
                      ? {
                          source: { kind: "worker", workerId: worker.id },
                          title: `Folders on ${worker.name}`,
                        }
                      : undefined
                  }
                />
                {warning && <FieldWarning>{warning}</FieldWarning>}
              </FieldShell>
            )}
          </div>
        </div>
      </Panel>

      {remote && (
        <Panel
          title="How the worker reads it"
          description="Snapshots look the same either way: the folder's contents at their root."
        >
          <RemoteMethodFields
            value={s.remote}
            onChange={(v) => set("remote", v)}
            errors={errors}
            worker={jobWorker}
            idPrefix={idPrefix}
          />
        </Panel>
      )}

      <Panel title="Options">
        <div className="flex flex-col gap-6">
          <div className="grid gap-6 md:grid-cols-2">
            {remote ? (
              <FilterRuleList
                value={s.remote.filters}
                onChange={(filters) => set("remote", { ...s.remote, filters })}
                error={errors.sourceFilters}
                help={
                  <>
                    rclone filter patterns, applied in order; the first match decides. For example
                    exclude <code className="rounded bg-muted px-1">.thumbnails/**</code>. Both ways
                    of reading apply them, so a mirror does not download what is left out.
                  </>
                }
              />
            ) : (
              <FieldShell
                id={`${idPrefix}-excludes`}
                label="Exclude"
                optional
                error={errors.excludes}
                help="restic exclude patterns, such as *.tmp or /srv/data/cache."
              >
                <PatternList
                  id={`${idPrefix}-excludes`}
                  value={s.excludes}
                  onChange={(v) => set("excludes", v)}
                />
              </FieldShell>
            )}
            <div className="flex flex-col gap-5">
              <FieldShell id={`${idPrefix}-tags`} label="Tags" optional error={errors.tags}>
                <ChipsInput
                  id={`${idPrefix}-tags`}
                  noun="tag"
                  value={s.tags}
                  onChange={(v) => set("tags", v)}
                  placeholder="photos, nightly…"
                  separators={/,$/}
                  help={
                    remote
                      ? `Tags mark the snapshot, so retention rules can pick it out. Syncr adds ${REMOTE_TAG_PREFIX}… itself to name the remote, so up to 31 of your own.`
                      : "Tags mark the snapshot, so retention rules can pick it out."
                  }
                />
              </FieldShell>
              <FieldShell
                id={`${idPrefix}-host`}
                label="Host name"
                optional
                help="Recorded in the snapshot. Leave empty to use the worker's host name."
              >
                <Input
                  id={`${idPrefix}-host`}
                  className="bg-card"
                  value={s.host}
                  onChange={(e) => set("host", e.target.value)}
                  autoComplete="off"
                  spellCheck={false}
                />
              </FieldShell>
            </div>
          </div>
          <div className="grid gap-x-8 gap-y-4 md:grid-cols-2">
            {!remote && (
              <Toggle
                id={`${idPrefix}-vss`}
                label="Use a volume snapshot (VSS)"
                help={
                  windows
                    ? "Backs up open and locked files. Needs the worker to run as a system service."
                    : "Windows workers only. Needs the worker to run as a system service."
                }
                checked={s.useFsSnapshot}
                onChange={(v) => set("useFsSnapshot", v)}
                disabled={!windows}
              />
            )}
            <Toggle
              id={`${idPrefix}-caches`}
              label="Skip cache folders"
              help="Folders marked with a CACHEDIR.TAG file."
              checked={s.excludeCaches}
              onChange={(v) => set("excludeCaches", v)}
            />
            {!remote && (
              <Toggle
                id={`${idPrefix}-onefs`}
                label="Stay on one file system"
                help="Do not cross into other mounted disks."
                checked={s.oneFileSystem}
                onChange={(v) => set("oneFileSystem", v)}
              />
            )}
            <Toggle
              id={`${idPrefix}-dry`}
              label="Dry run"
              help={
                remote
                  ? "Report what would be saved without writing a snapshot. A mirror is still brought up to date for real."
                  : "Report what would be saved without writing a snapshot."
              }
              checked={s.dryRun}
              onChange={(v) => set("dryRun", v)}
            />
          </div>
          <div className="grid gap-4 sm:grid-cols-3">
            <NumberInput
              id={`${idPrefix}-larger`}
              label="Skip files larger than"
              value={s.excludeLargerThan}
              onChange={(v) => set("excludeLargerThan", v)}
              unit="MiB"
              placeholder="No limit"
            />
          </div>
          {errors.options && (
            <p role="alert" className="text-xs leading-5 text-danger">
              {errors.options}
            </p>
          )}
        </div>
      </Panel>
    </>
  );
}
