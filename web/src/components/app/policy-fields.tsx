import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { OverlapPolicy } from "@/gen/syncr/v1/schedules_pb";
import {
  overlapPolicies,
  withinLabel,
  withinPresets,
  type MissedState,
} from "@/features/schedules/model";

import { ChoiceCards } from "./choice-cards";

/** PolicyFieldsProps configures PolicyFields. */
export interface PolicyFieldsProps {
  idPrefix: string;
  missed: MissedState;
  onMissedChange: (m: MissedState) => void;
  overlap: OverlapPolicy;
  onOverlapChange: (o: OverlapPolicy) => void;
  missedError?: string;
}

/**
 * PolicyFields edits what a schedule does when its worker is offline at
 * run time (run late within a window, or skip) and when a run is due while
 * the last one is still going (skip, wait, or run both).
 */
export function PolicyFields({
  idPrefix,
  missed,
  onMissedChange,
  overlap,
  onOverlapChange,
  missedError,
}: PolicyFieldsProps) {
  const presets = withinPresets.some((p) => p.seconds === missed.within)
    ? withinPresets
    : [...withinPresets, { seconds: missed.within, label: withinLabel(missed.within) }];
  return (
    <div className="grid grid-cols-1 gap-6 lg:grid-cols-2">
      <div className="flex flex-col gap-2">
        <ChoiceCards
          label="If the worker is offline when a run is due"
          labelId={`${idPrefix}-missed`}
          value={missed.action}
          onChange={(action) => onMissedChange({ ...missed, action })}
          options={[
            {
              value: "run_late",
              label: "Run it when the worker is back",
              help: "Good for desktops and laptops that sleep. Runs missed meanwhile are merged into one.",
              children: (
                <div className="flex flex-wrap items-center gap-2 text-[13px]">
                  <label htmlFor={`${idPrefix}-within`} className="text-muted-foreground">
                    Give up after
                  </label>
                  <Select
                    value={String(missed.within)}
                    onValueChange={(v) => onMissedChange({ ...missed, within: Number(v) })}
                  >
                    <SelectTrigger id={`${idPrefix}-within`} className="h-8 w-56 bg-card">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {presets.map((p) => (
                        <SelectItem key={p.seconds} value={String(p.seconds)}>
                          {p.label}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>
              ),
            },
            {
              value: "skip",
              label: "Skip it and notify",
              help: "The run is recorded as missed; the next one runs on time.",
            },
          ]}
        />
        {missedError && (
          <p role="alert" className="text-xs text-danger">
            {missedError}
          </p>
        )}
      </div>
      <ChoiceCards
        label="If the last run is still going"
        labelId={`${idPrefix}-overlap`}
        value={overlap}
        onChange={onOverlapChange}
        options={overlapPolicies.map((p) => ({ value: p.value, label: p.label, help: p.help }))}
      />
    </div>
  );
}
