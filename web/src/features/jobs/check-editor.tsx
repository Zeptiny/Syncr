import { FieldShell } from "@/components/app/descriptor-form";
import { Panel } from "@/components/app/panel";
import { Input } from "@/components/ui/input";
import type { Repository } from "@/gen/syncr/v1/restic_pb";
import { cn } from "@/lib/utils";

import type { DefinitionErrors } from "./definition";
import { RepositoryField } from "./repository-field";
import { readDataOptions, type CheckState } from "./restic-definition";

/** CheckEditorProps configures CheckEditor. */
export interface CheckEditorProps {
  value: CheckState;
  onChange: (v: CheckState) => void;
  errors: DefinitionErrors;
  repositories: Repository[];
  idPrefix: string;
}

/** CheckEditor edits a check: the repository and how much data to read back. */
export function CheckEditor({
  value: s,
  onChange,
  errors,
  repositories,
  idPrefix,
}: CheckEditorProps) {
  const chosen = readDataOptions.find((o) => o.value === s.readData);
  return (
    <Panel
      title="What to check"
      description="A check verifies the repository's structure, and can read back part of the data to catch damage in storage."
    >
      <div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_minmax(0,1.4fr)]">
        <RepositoryField
          id={`${idPrefix}-repo`}
          repositories={repositories}
          value={s.repositoryId}
          onChange={(v) => onChange({ ...s, repositoryId: v })}
          error={errors.repository}
        />
        <div className="flex flex-col gap-1.5">
          <span className="text-[13px] font-medium" id={`${idPrefix}-read-label`}>
            Read data
          </span>
          <div
            role="radiogroup"
            aria-labelledby={`${idPrefix}-read-label`}
            className="inline-flex w-full flex-wrap gap-1 rounded-lg border bg-muted/50 p-1 sm:w-fit"
          >
            {readDataOptions.map((o) => {
              const on = o.value === s.readData;
              return (
                <button
                  key={o.value}
                  type="button"
                  role="radio"
                  aria-checked={on}
                  onClick={() => onChange({ ...s, readData: o.value })}
                  className={cn(
                    "rounded-md px-3 py-1.5 text-[13px] transition-colors",
                    on
                      ? "bg-card font-medium shadow-xs ring-1 ring-border"
                      : "text-muted-foreground hover:text-foreground",
                  )}
                >
                  {o.label}
                </button>
              );
            })}
          </div>
          {s.readData === "custom" && (
            <FieldShell id={`${idPrefix}-read-custom`} label="Share of the data to read">
              <div className="relative w-32">
                <Input
                  id={`${idPrefix}-read-custom`}
                  inputMode="numeric"
                  className="tabular bg-card pr-8"
                  value={s.custom}
                  placeholder="25"
                  aria-invalid={errors.readData ? true : undefined}
                  onChange={(e) =>
                    onChange({ ...s, custom: e.target.value.replace(/\D/g, "").slice(0, 3) })
                  }
                />
                <span className="pointer-events-none absolute top-1/2 right-3 -translate-y-1/2 text-xs text-muted-foreground">
                  %
                </span>
              </div>
            </FieldShell>
          )}
          {errors.readData ? (
            <p role="alert" className="text-xs text-danger">
              {errors.readData}
            </p>
          ) : (
            <p className="text-xs leading-5 text-muted-foreground">{chosen?.help}</p>
          )}
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
