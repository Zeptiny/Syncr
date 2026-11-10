import { KindPicker } from "@/components/app/kind-picker";
import { Panel } from "@/components/app/panel";
import type { Worker } from "@/gen/syncr/v1/api_pb";
import type { Backend, Remote } from "@/gen/syncr/v1/remotes_pb";
import type { Repository } from "@/gen/syncr/v1/restic_pb";

import { BackupEditor } from "./backup-editor";
import { CheckEditor } from "./check-editor";
import {
  editableKinds,
  withKind,
  type DefinitionErrors,
  type EditableKind,
  type JobDefinitionState,
} from "./definition";
import { PruneEditor } from "./prune-editor";
import { RetentionEditor } from "./retention-editor";
import { TransferEditor } from "./transfer-editor";

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
  /** how renders next to the kind's first fields, e.g. the worker picker of "Run a job". */
  how?: React.ReactNode;
  /** idPrefix keeps input IDs unique when two editors share a page. */
  idPrefix?: string;
  /** kinds limits the kinds offered (default: every editable kind). */
  kinds?: EditableKind[];
  /** onKindChange is told when the user picks another kind (e.g. to update the URL). */
  onKindChange?: (kind: EditableKind) => void;
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
  how,
  idPrefix = "run",
  kinds = editableKinds,
  onKindChange,
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
          onChange={(transfer) => onChange({ kind: s.kind, transfer })}
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
            <Panel title="Where it runs">
              <div className="max-w-md">{how}</div>
            </Panel>
          )}
          {s.kind === "restic_backup" && (
            <BackupEditor
              {...restic}
              value={s.backup}
              onChange={(backup) => onChange({ kind: s.kind, backup })}
              worker={worker ?? browseWorker}
              workerNote={worker ? undefined : browseNote}
            />
          )}
          {s.kind === "restic_forget" && (
            <RetentionEditor
              {...restic}
              value={s.retention}
              onChange={(retention) => onChange({ kind: s.kind, retention })}
            />
          )}
          {s.kind === "restic_check" && (
            <CheckEditor
              {...restic}
              value={s.check}
              onChange={(check) => onChange({ kind: s.kind, check })}
            />
          )}
          {s.kind === "restic_prune" && (
            <PruneEditor
              {...restic}
              value={s.prune}
              onChange={(prune) => onChange({ kind: s.kind, prune })}
            />
          )}
        </>
      )}
    </>
  );
}
