import { KindPicker } from "@/components/app/kind-picker";
import { Panel } from "@/components/app/panel";
import type { Worker } from "@/gen/syncr/v1/api_pb";
import type { Backend, Remote } from "@/gen/syncr/v1/remotes_pb";
import type { Repository } from "@/gen/syncr/v1/restic_pb";

import { BackupEditor } from "./backup-editor";
import { filtersNeedResync } from "./bisync-definition";
import { BisyncEditor } from "./bisync-editor";
import { CheckEditor } from "./check-editor";
import type { HooksTarget } from "./hooks";
import { HooksPanel } from "./hooks-editor";
import {
  editableKinds,
  withKind,
  type DefinitionErrors,
  type EditableKind,
  type JobDefinitionState,
} from "./definition";
import { PruneEditor } from "./prune-editor";
import { RetentionEditor } from "./retention-editor";
import { QuantityInput, TransferEditor } from "./transfer-editor";
import { timeLimitUnits } from "./units";

/** JobDefinitionEditorProps configures JobDefinitionEditor. */
export interface JobDefinitionEditorProps {
  value: JobDefinitionState;
  onChange: (v: JobDefinitionState) => void;
  errors: DefinitionErrors;
  remotes: Remote[];
  backends: Map<string, Backend>;
  repositories: Repository[];
  /** worker is the job's worker, when one is chosen: its folders and OS. */
  worker?: Worker;
  /**
   * browseWorker stands in for worker when the job runs on a label: the
   * first online worker with it. browseNote tells the user so.
   */
  browseWorker?: Worker;
  browseNote?: string;
  /**
   * hooksTarget is where the job runs, as far as hooks go: the workers it
   * may run on (one, or a label's) that have reported in. Default: worker.
   */
  hooksTarget?: HooksTarget;
  /** how renders next to the kind's first fields, e.g. the worker picker of "Run a job". */
  how?: React.ReactNode;
  /** idPrefix keeps input IDs unique when two editors share a page. */
  idPrefix?: string;
  /** kinds limits the kinds offered (default: every editable kind). */
  kinds?: EditableKind[];
  /** onKindChange is told when the user picks another kind (e.g. to update the URL). */
  onKindChange?: (kind: EditableKind) => void;
  /**
   * saved is the stored definition being changed (a schedule's, or the job
   * of "Edit and run"), so the form can say what a change means: changed
   * filters of the same two-way sync pair need a resync.
   */
  saved?: JobDefinitionState;
}

/**
 * JobDefinitionEditor edits what a job does: it picks the kind, then shows
 * that kind's editor. "Run a job" and the schedule form both use it. The
 * kinds are the ones a user may start: a restore starts from a snapshot,
 * and a repository's data is only deleted from the repository.
 */
export function JobDefinitionEditor({
  value: s,
  onChange,
  errors,
  remotes,
  backends,
  repositories,
  worker,
  browseWorker,
  browseNote,
  hooksTarget,
  how,
  idPrefix = "run",
  kinds = editableKinds,
  onKindChange,
  saved,
}: JobDefinitionEditorProps) {
  const restic = { errors, repositories, idPrefix: `${idPrefix}-${s.kind}` };
  return (
    <>
      <Panel title="What kind of job">
        <KindPicker
          labelId={`${idPrefix}-kind`}
          value={s.kind}
          kinds={kinds}
          onChange={(k) => {
            const kind = k as EditableKind;
            if (kind === s.kind) return;
            onChange(withKind(s, kind));
            onKindChange?.(kind);
          }}
        />
      </Panel>
      {s.kind === "rclone_transfer" ? (
        <TransferEditor
          value={s.transfer}
          onChange={(transfer) => onChange({ ...s, transfer })}
          errors={errors}
          remotes={remotes}
          backends={backends}
          worker={worker}
          how={how}
          idPrefix={idPrefix}
        />
      ) : (
        <>
          {how !== undefined && (
            <Panel
              title="Where it runs"
              description={
                s.kind === "rclone_bisync"
                  ? "A two-way sync keeps its state on the worker that runs it. Its first run on a worker is a resync: it merges both folders and deletes nothing."
                  : undefined
              }
            >
              <div className="max-w-md">{how}</div>
            </Panel>
          )}
          {s.kind === "rclone_bisync" && (
            <BisyncEditor
              value={s.bisync}
              onChange={(bisync) => onChange({ ...s, bisync })}
              errors={errors}
              remotes={remotes}
              backends={backends}
              worker={worker}
              filtersNeedResync={
                saved?.kind === "rclone_bisync" && filtersNeedResync(saved.bisync, s.bisync)
              }
              idPrefix={idPrefix}
            />
          )}
          {s.kind === "restic_backup" && (
            <BackupEditor
              {...restic}
              value={s.backup}
              onChange={(backup) => onChange({ ...s, backup })}
              worker={worker ?? browseWorker}
              workerNote={worker ? undefined : browseNote}
            />
          )}
          {s.kind === "restic_forget" && (
            <RetentionEditor
              {...restic}
              value={s.retention}
              onChange={(retention) => onChange({ ...s, retention })}
            />
          )}
          {s.kind === "restic_check" && (
            <CheckEditor
              {...restic}
              value={s.check}
              onChange={(check) => onChange({ ...s, check })}
            />
          )}
          {s.kind === "restic_prune" && (
            <PruneEditor
              {...restic}
              value={s.prune}
              onChange={(prune) => onChange({ ...s, prune })}
            />
          )}
        </>
      )}
      <HooksPanel
        idPrefix={idPrefix}
        value={s.hooks}
        onChange={(hooks) => onChange({ ...s, hooks })}
        target={hooksTarget ?? { workers: worker?.info ? [worker] : [] }}
        error={errors.hooks}
      />
      <Panel title="Time limit">
        <div className="max-w-md">
          <QuantityInput
            id={`${idPrefix}-time-limit`}
            label="Stop the job after"
            value={s.timeLimit.text}
            unit={s.timeLimit.unit}
            units={timeLimitUnits}
            onChange={(text, unit) => onChange({ ...s, timeLimit: { text, unit } })}
            placeholder="No limit"
            optional
            error={errors.timeLimit}
            help={timeLimitHelp(s.kind)}
          />
        </div>
      </Panel>
    </>
  );
}

/**
 * timeLimitHelp says what the time limit does, and for transfers how it
 * differs from rclone's max duration ("Stop starting after").
 */
export function timeLimitHelp(kind: EditableKind): string {
  const base =
    "A hard stop: once the job has run this long, the worker stops it and the job fails. From 1 minute to 366 days; empty means no limit unless the controller sets a default.";
  return kind === "rclone_transfer"
    ? `${base} Unlike "Stop starting after" (rclone's max duration), which only stops new transfers from starting, it ends the job.`
    : base;
}
