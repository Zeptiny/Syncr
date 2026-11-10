import { ChipsInput } from "@/components/app/chips-input";
import { FieldShell } from "@/components/app/descriptor-form";
import { Panel } from "@/components/app/panel";
import { RetentionFields } from "@/components/app/retention-fields";
import { Input } from "@/components/ui/input";
import type { Repository } from "@/gen/syncr/v1/restic_pb";

import type { DefinitionErrors } from "./definition";
import { RepositoryField } from "./repository-field";
import type { RetentionState } from "./restic-definition";
import { Toggle } from "./transfer-editor";

/** RetentionEditorProps configures RetentionEditor. */
export interface RetentionEditorProps {
  value: RetentionState;
  onChange: (v: RetentionState) => void;
  errors: DefinitionErrors;
  repositories: Repository[];
  idPrefix: string;
}

/**
 * RetentionEditor edits a retention job: which snapshots of a repository
 * to keep, whether to free their space at once, and which snapshots the
 * rules look at.
 */
export function RetentionEditor({
  value: s,
  onChange,
  errors,
  repositories,
  idPrefix,
}: RetentionEditorProps) {
  const set = <K extends keyof RetentionState>(k: K, v: RetentionState[K]) =>
    onChange({ ...s, [k]: v });
  return (
    <>
      <Panel
        title="What to keep"
        description="Snapshots that no rule keeps are removed from the repository."
      >
        <div className="flex flex-col gap-6">
          <div className="max-w-md">
            <RepositoryField
              id={`${idPrefix}-repo`}
              repositories={repositories}
              value={s.repositoryId}
              onChange={(v) => set("repositoryId", v)}
              error={errors.repository}
            />
          </div>
          <RetentionFields
            idPrefix={`${idPrefix}-keep`}
            value={s.policy}
            onChange={(policy) => set("policy", policy)}
            error={errors.policy}
          />
        </div>
      </Panel>
      <Panel title="Options">
        <div className="flex flex-col gap-6">
          <div className="grid gap-x-8 gap-y-4 md:grid-cols-2">
            <Toggle
              id={`${idPrefix}-prune`}
              label="Prune afterwards"
              help="Frees the space of the removed snapshots right away. Takes longer."
              checked={s.prune}
              onChange={(v) => set("prune", v)}
            />
            <Toggle
              id={`${idPrefix}-dry`}
              label="Dry run"
              help="Report which snapshots would be removed without removing them."
              checked={s.dryRun}
              onChange={(v) => set("dryRun", v)}
            />
          </div>
          <div className="grid gap-6 md:grid-cols-2">
            <FieldShell
              id={`${idPrefix}-host`}
              label="Only snapshots of host"
              optional
              help="Leave empty to apply the rules to every host, each on its own."
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
            <FieldShell id={`${idPrefix}-tags`} label="Only snapshots tagged" optional>
              <ChipsInput
                id={`${idPrefix}-tags`}
                noun="tag"
                value={s.tags}
                onChange={(v) => set("tags", v)}
                placeholder="photos…"
                separators={/,$/}
              />
            </FieldShell>
          </div>
          {errors.options && (
            <p role="alert" className="text-xs text-danger">
              {errors.options}
            </p>
          )}
        </div>
      </Panel>
    </>
  );
}
