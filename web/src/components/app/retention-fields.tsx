import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  describeRetention,
  matchingPreset,
  retentionCounts,
  retentionPresets,
  type RetentionPolicyState,
} from "@/lib/retention";
import { cn } from "@/lib/utils";

import { ChipsInput } from "./chips-input";

/** RetentionFieldsProps configures RetentionFields. */
export interface RetentionFieldsProps {
  idPrefix: string;
  value: RetentionPolicyState;
  onChange: (v: RetentionPolicyState) => void;
  /** error is a problem with the policy as a whole. */
  error?: string;
}

/**
 * RetentionFields edits which snapshots a retention job keeps: presets, a
 * grid of keep-N rules, a keep-within period and tags that are always
 * kept. A line under the grid says what the rules mean together.
 */
export function RetentionFields({ idPrefix, value, onChange, error }: RetentionFieldsProps) {
  const preset = matchingPreset(value);
  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col gap-1.5">
        <span className="text-[13px] font-medium" id={`${idPrefix}-presets`}>
          Presets
        </span>
        <div role="group" aria-labelledby={`${idPrefix}-presets`} className="flex flex-wrap gap-2">
          {retentionPresets.map((p) => (
            <button
              key={p.id}
              type="button"
              aria-pressed={preset === p.id}
              onClick={() => onChange({ ...p.policy, keepTags: [] })}
              className={cn(
                "rounded-md border bg-card px-3 py-1.5 text-[13px] transition-colors",
                preset === p.id
                  ? "border-primary font-medium ring-[3px] ring-primary/20"
                  : "hover:border-foreground/25",
              )}
            >
              {p.label}
            </button>
          ))}
        </div>
      </div>
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
        {retentionCounts.map((c) => {
          const id = `${idPrefix}-${c.key}`;
          return (
            <div key={c.key} className="flex flex-col gap-1.5">
              <Label htmlFor={id} className="text-xs font-normal text-muted-foreground">
                Keep {c.label.toLowerCase()}
              </Label>
              <Input
                id={id}
                inputMode="numeric"
                className="tabular bg-card"
                placeholder="0"
                value={value[c.key]}
                onChange={(e) =>
                  onChange({ ...value, [c.key]: e.target.value.replace(/\D/g, "").slice(0, 5) })
                }
              />
            </div>
          );
        })}
      </div>
      <div className="grid gap-4 md:grid-cols-2">
        <div className="flex flex-col gap-1.5">
          <Label htmlFor={`${idPrefix}-within`} className="text-[13px]">
            Keep everything from the last
          </Label>
          <Input
            id={`${idPrefix}-within`}
            className="bg-card font-mono text-[13px]"
            placeholder="e.g. 30d or 1y6m"
            value={value.keepWithin}
            onChange={(e) => onChange({ ...value, keepWithin: e.target.value.trim() })}
            spellCheck={false}
            autoComplete="off"
          />
          <p className="text-xs leading-5 text-muted-foreground">
            Years, months, days and hours, such as 1y6m, 30d or 12h. Leave empty for no period.
          </p>
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor={`${idPrefix}-keep-tags`} className="text-[13px]">
            Always keep snapshots tagged
          </Label>
          <ChipsInput
            id={`${idPrefix}-keep-tags`}
            noun="tag"
            value={value.keepTags}
            onChange={(keepTags) => onChange({ ...value, keepTags })}
            placeholder="keep, yearly…"
            separators={/,$/}
          />
        </div>
      </div>
      <p
        role={error ? "alert" : "status"}
        className={cn(
          "rounded-md px-3 py-2 text-[13px]",
          error ? "bg-danger-soft text-danger" : "bg-muted text-muted-foreground",
        )}
      >
        {error ??
          `${describeRetention(value)} Each rule adds snapshots to keep, per host and paths.`}
      </p>
    </div>
  );
}
