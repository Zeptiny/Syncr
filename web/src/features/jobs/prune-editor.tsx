import { Panel } from "@/components/app/panel";
import type { Repository } from "@/gen/syncr/v1/restic_pb";

import type { DefinitionErrors } from "./definition";
import { RepositoryField } from "./repository-field";
import type { PruneState } from "./restic-definition";
import { Toggle } from "./transfer-editor";

/** PruneEditorProps configures PruneEditor. */
export interface PruneEditorProps {
  value: PruneState;
  onChange: (v: PruneState) => void;
  errors: DefinitionErrors;
  repositories: Repository[];
  idPrefix: string;
}

/** PruneEditor edits a prune: the repository and whether it is a dry run. */
export function PruneEditor({
  value: s,
  onChange,
  errors,
  repositories,
  idPrefix,
}: PruneEditorProps) {
  return (
    <Panel
      title="What to prune"
      description="Pruning frees the space of data that no snapshot uses any more, for example after a retention job that did not prune."
    >
      <div className="grid gap-6 lg:grid-cols-2">
        <RepositoryField
          id={`${idPrefix}-repo`}
          repositories={repositories}
          value={s.repositoryId}
          onChange={(v) => onChange({ ...s, repositoryId: v })}
          error={errors.repository}
        />
        <div className="flex flex-col gap-2 lg:pt-6">
          <Toggle
            id={`${idPrefix}-dry`}
            label="Dry run"
            help="Report how much space would be freed without changing the repository."
            checked={s.dryRun}
            onChange={(v) => onChange({ ...s, dryRun: v })}
          />
          {errors.options && (
            <p role="alert" className="text-xs text-danger">
              {errors.options}
            </p>
          )}
        </div>
      </div>
    </Panel>
  );
}
