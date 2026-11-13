import { ArrowDown, ArrowRight, ChevronRight } from "lucide-react";
import { useState } from "react";

import { FieldShell } from "@/components/app/descriptor-form";
import { LocationEditor } from "@/components/app/location-editor";
import { Panel } from "@/components/app/panel";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import type { Worker } from "@/gen/syncr/v1/api_pb";
import { RcloneTransferMode } from "@/gen/syncr/v1/link_pb";
import type { Backend, Remote } from "@/gen/syncr/v1/remotes_pb";
import { transferModes } from "@/lib/status";
import { cn } from "@/lib/utils";

import type { Compare, DefinitionErrors, TransferState } from "./definition";
import { FilterRuleList } from "./pattern-list";
import { byteUnits, durationUnits } from "./units";

/** Toggle is a labelled switch with optional help. */
export function Toggle({
  id,
  label,
  help,
  checked,
  onChange,
  disabled,
}: {
  id: string;
  label: string;
  help?: string;
  checked: boolean;
  onChange: (v: boolean) => void;
  disabled?: boolean;
}) {
  return (
    <div className={cn("flex items-start justify-between gap-4", disabled && "opacity-50")}>
      <div>
        <Label htmlFor={id} className="text-[13px]">
          {label}
        </Label>
        {help && <p className="mt-0.5 text-xs leading-5 text-muted-foreground">{help}</p>}
      </div>
      <Switch
        id={id}
        checked={checked && !disabled}
        onCheckedChange={onChange}
        disabled={disabled}
      />
    </div>
  );
}

/** NumberInput is a numeric text field with an optional unit, help and error. */
export function NumberInput({
  id,
  label,
  value,
  onChange,
  unit,
  placeholder,
  help,
  error,
}: {
  id: string;
  label: string;
  value: string;
  onChange: (v: string) => void;
  unit?: string;
  placeholder?: string;
  help?: React.ReactNode;
  error?: string;
}) {
  return (
    <FieldShell id={id} label={label} help={help} error={error}>
      <div className="relative">
        <Input
          id={id}
          inputMode="decimal"
          className={cn("tabular bg-card", unit && "pr-14")}
          value={value}
          placeholder={placeholder ?? "Default"}
          aria-invalid={error ? true : undefined}
          aria-describedby={error ? `${id}-error` : help ? `${id}-help` : undefined}
          onChange={(e) => onChange(e.target.value.trim())}
        />
        {unit && (
          <span className="pointer-events-none absolute top-1/2 right-3 -translate-y-1/2 text-xs text-muted-foreground">
            {unit}
          </span>
        )}
      </div>
    </FieldShell>
  );
}

/**
 * QuantityInput is a numeric text field next to a unit picker, for sizes and
 * durations that are stored in one base unit.
 */
export function QuantityInput<U extends string>({
  id,
  label,
  value,
  unit,
  units,
  onChange,
  placeholder,
  help,
  error,
  optional,
}: {
  id: string;
  label: string;
  value: string;
  unit: U;
  units: { id: U; label?: string }[];
  onChange: (value: string, unit: U) => void;
  placeholder?: string;
  help?: React.ReactNode;
  error?: string;
  optional?: boolean;
}) {
  return (
    <FieldShell id={id} label={label} help={help} error={error} optional={optional}>
      <div className="flex gap-2">
        <Input
          id={id}
          inputMode="decimal"
          className="tabular min-w-0 flex-1 bg-card"
          value={value}
          placeholder={placeholder ?? "Default"}
          aria-invalid={error ? true : undefined}
          aria-describedby={error ? `${id}-error` : help ? `${id}-help` : undefined}
          onChange={(e) => onChange(e.target.value.trim(), unit)}
        />
        <Select value={unit} onValueChange={(u) => onChange(value, u as U)}>
          <SelectTrigger aria-label={`${label} unit`} className="w-[5.5rem] shrink-0 bg-card">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {units.map((u) => (
              <SelectItem key={u.id} value={u.id}>
                {u.label ?? u.id}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
    </FieldShell>
  );
}

/** TransferEditorProps configures TransferEditor. */
export interface TransferEditorProps {
  value: TransferState;
  onChange: (v: TransferState) => void;
  errors: DefinitionErrors;
  remotes: Remote[];
  backends: Map<string, Backend>;
  /**
   * worker is whose folders local paths are browsed on and suggested
   * from: the job's worker, or for a label target its stand-in.
   */
  worker?: Worker;
  /** workerNote replaces the help under local paths (see allowedFoldersHelp). */
  workerNote?: string;
  /**
   * runsOn are the workers the job may run on that have reported in: a
   * warning names local paths outside their allowed folders. Default: worker.
   */
  runsOn?: Worker[];
  /** how renders next to the mode, e.g. the worker picker of "Run a job". */
  how?: React.ReactNode;
  /** idPrefix keeps input IDs unique when two editors share a page. */
  idPrefix?: string;
}

/**
 * TransferEditor edits a transfer: its mode, source, destination, options
 * and filters. JobDefinitionEditor shows it for the Transfer kind.
 */
export function TransferEditor({
  value: s,
  onChange,
  errors,
  remotes: remoteList,
  backends,
  worker,
  workerNote,
  runsOn,
  how,
  idPrefix = "run",
}: TransferEditorProps) {
  const [more, setMore] = useState(false);
  const set = <K extends keyof TransferState>(k: K, v: TransferState[K]) =>
    onChange({ ...s, [k]: v });
  return (
    <>
      <Panel title="How">
        <div
          className={cn(
            "grid gap-6",
            how !== undefined && "lg:grid-cols-[minmax(0,1fr)_minmax(0,2fr)]",
          )}
        >
          {how}
          <div className="flex flex-col gap-1.5">
            <span className="text-[13px] font-medium" id={`${idPrefix}-mode-label`}>
              Mode
            </span>
            <div
              role="radiogroup"
              aria-labelledby={`${idPrefix}-mode-label`}
              className="grid gap-2 sm:grid-cols-3"
            >
              {transferModes.map((m) => {
                const on = s.mode === m.mode;
                return (
                  <button
                    key={m.mode}
                    type="button"
                    role="radio"
                    aria-checked={on}
                    onClick={() => set("mode", m.mode)}
                    className={cn(
                      "rounded-lg border bg-card px-3.5 py-3 text-left transition-colors",
                      on
                        ? "border-primary ring-[3px] ring-primary/20"
                        : "hover:border-foreground/25",
                    )}
                  >
                    <span className="flex items-center gap-2 text-[13px] font-semibold">
                      <span
                        className={cn(
                          "grid size-3.5 place-items-center rounded-full border",
                          on && "border-primary",
                        )}
                        aria-hidden
                      >
                        {on && <span className="size-2 rounded-full bg-primary" />}
                      </span>
                      {m.label}
                    </span>
                    <span className="mt-1 block text-xs leading-5 text-muted-foreground">
                      {m.help}
                    </span>
                  </button>
                );
              })}
            </div>
          </div>
        </div>
      </Panel>

      <Panel title="What">
        <div className="grid items-start gap-6 md:grid-cols-[minmax(0,1fr)_2rem_minmax(0,1fr)]">
          <LocationEditor
            id={`${idPrefix}-src`}
            label="From"
            value={s.source}
            onChange={(v) => set("source", v)}
            remotes={remoteList}
            backends={backends}
            worker={worker}
            workerNote={workerNote}
            runsOn={runsOn}
            errors={{ remote: errors.sourceRemote, path: errors.sourcePath }}
          />
          <div className="flex justify-center text-muted-foreground md:pt-14" aria-hidden>
            <ArrowRight className="hidden size-5 md:block" />
            <ArrowDown className="size-5 md:hidden" />
          </div>
          <LocationEditor
            id={`${idPrefix}-dst`}
            label="To"
            value={s.destination}
            onChange={(v) => set("destination", v)}
            remotes={remoteList}
            backends={backends}
            worker={worker}
            workerNote={workerNote}
            runsOn={runsOn}
            errors={{ remote: errors.destRemote, path: errors.destPath }}
          />
        </div>
      </Panel>

      <Panel title="Options" description="Leave a field empty to use rclone's default.">
        <div className="flex flex-col gap-6">
          <div className="grid gap-4 sm:grid-cols-3">
            <NumberInput
              id="opt-transfers"
              label="Parallel transfers"
              value={s.transfers}
              onChange={(v) => set("transfers", v)}
              placeholder="4"
            />
            <NumberInput
              id="opt-checkers"
              label="Parallel checkers"
              value={s.checkers}
              onChange={(v) => set("checkers", v)}
              placeholder="8"
            />
            <QuantityInput
              id="opt-bw"
              label="Bandwidth limit"
              value={s.bwlimit}
              unit={s.bwlimitUnit}
              units={byteUnits.map((u) => ({ id: u.id, label: `${u.id}/s` }))}
              onChange={(bwlimit, bwlimitUnit) => onChange({ ...s, bwlimit, bwlimitUnit })}
              placeholder="Unlimited"
            />
          </div>
          <div className="grid gap-6 md:grid-cols-2">
            <FieldShell id="opt-compare" label="Decide what changed by">
              <Select value={s.compare} onValueChange={(v) => set("compare", v as Compare)}>
                <SelectTrigger id="opt-compare" className="w-full bg-card">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="default">Modification time and size</SelectItem>
                  <SelectItem value="checksum">Checksum (slower, exact)</SelectItem>
                  <SelectItem value="size">Size only</SelectItem>
                </SelectContent>
              </Select>
            </FieldShell>
            <Toggle
              id="opt-dry"
              label="Dry run"
              help="Report what would change without changing anything."
              checked={s.dryRun}
              onChange={(v) => set("dryRun", v)}
            />
          </div>
          {errors.options && <p className="text-xs text-danger">{errors.options}</p>}

          <Collapsible open={more} onOpenChange={setMore} className="rounded-lg border">
            <CollapsibleTrigger asChild>
              <button
                type="button"
                className="flex w-full items-center gap-2 rounded-lg px-4 py-3 text-left text-[13px] font-medium hover:bg-accent/50"
              >
                <ChevronRight
                  className={cn("size-4 transition-transform", more && "rotate-90")}
                  aria-hidden
                />
                More options and filters
                {s.filters.length > 0 && (
                  <span className="font-normal text-muted-foreground">
                    {s.filters.length} {s.filters.length === 1 ? "filter" : "filters"}
                  </span>
                )}
              </button>
            </CollapsibleTrigger>
            <CollapsibleContent>
              <div className="flex flex-col gap-6 border-t px-4 py-5">
                <div className="grid gap-x-8 gap-y-4 md:grid-cols-2">
                  <Toggle
                    id="opt-ignore"
                    label="Skip files that already exist"
                    checked={s.ignoreExisting}
                    onChange={(v) => set("ignoreExisting", v)}
                  />
                  <Toggle
                    id="opt-update"
                    label="Skip files newer at the destination"
                    checked={s.update}
                    onChange={(v) => set("update", v)}
                  />
                  <Toggle
                    id="opt-empty"
                    label="Create empty folders"
                    checked={s.createEmptySrcDirs}
                    onChange={(v) => set("createEmptySrcDirs", v)}
                  />
                  <Toggle
                    id="opt-fast"
                    label="Fast listing"
                    help="Fewer API calls on object stores, more memory."
                    checked={s.fastList}
                    onChange={(v) => set("fastList", v)}
                  />
                  <Toggle
                    id="opt-renames"
                    label="Detect renames"
                    help="Sync only."
                    checked={s.trackRenames}
                    onChange={(v) => set("trackRenames", v)}
                    disabled={s.mode !== RcloneTransferMode.SYNC}
                  />
                  <Toggle
                    id="opt-delete-empty"
                    label="Remove emptied source folders"
                    help="Move only."
                    checked={s.deleteEmptySrcDirs}
                    onChange={(v) => set("deleteEmptySrcDirs", v)}
                    disabled={s.mode !== RcloneTransferMode.MOVE}
                  />
                </div>
                <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
                  <QuantityInput
                    id="opt-maxt"
                    label="Stop after"
                    value={s.maxTransfer}
                    unit={s.maxTransferUnit}
                    units={byteUnits}
                    onChange={(maxTransfer, maxTransferUnit) =>
                      onChange({ ...s, maxTransfer, maxTransferUnit })
                    }
                    placeholder="No limit"
                  />
                  <QuantityInput
                    id="opt-maxd"
                    label="Stop starting after"
                    value={s.maxDuration}
                    unit={s.maxDurationUnit}
                    units={durationUnits}
                    onChange={(maxDuration, maxDurationUnit) =>
                      onChange({ ...s, maxDuration, maxDurationUnit })
                    }
                    placeholder="No limit"
                    help="rclone's max duration: no new transfer starts after this. For a hard stop, set a time limit."
                  />
                  <NumberInput
                    id="opt-retries"
                    label="Retries"
                    value={s.retries}
                    onChange={(v) => set("retries", v)}
                    placeholder="3"
                  />
                  <NumberInput
                    id="opt-llr"
                    label="Low-level retries"
                    value={s.lowLevelRetries}
                    onChange={(v) => set("lowLevelRetries", v)}
                    placeholder="10"
                  />
                </div>
                <FilterRuleList
                  value={s.filters}
                  onChange={(v) => set("filters", v)}
                  help={
                    <>
                      rclone filter patterns, applied in order. The first match decides; for example
                      exclude <code className="rounded bg-muted px-1">*.tmp</code>.
                    </>
                  }
                />
              </div>
            </CollapsibleContent>
          </Collapsible>
        </div>
      </Panel>
    </>
  );
}
