import { ArrowDownUp, ArrowLeftRight } from "lucide-react";

import { ChoiceCards } from "@/components/app/choice-cards";
import { FieldShell } from "@/components/app/descriptor-form";
import { LocationEditor } from "@/components/app/location-editor";
import { Panel } from "@/components/app/panel";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { Worker } from "@/gen/syncr/v1/api_pb";
import { BisyncConflictLoser, type BisyncResyncMode } from "@/gen/syncr/v1/link_pb";
import type { Backend, Remote } from "@/gen/syncr/v1/remotes_pb";

import {
  conflictResolutions,
  DEFAULT_MAX_DELETE,
  resyncModes,
  type BisyncErrors,
  type BisyncState,
  type CompareState,
} from "./bisync-definition";
import { FilterRuleList } from "./pattern-list";
import { NumberInput, QuantityInput, Toggle } from "./transfer-editor";
import { byteUnits } from "./units";

/** BisyncEditorProps configures BisyncEditor. */
export interface BisyncEditorProps {
  value: BisyncState;
  onChange: (v: BisyncState) => void;
  errors: BisyncErrors;
  remotes: Remote[];
  backends: Map<string, Backend>;
  /** worker offers its allowed folders for local paths. */
  worker?: Worker;
  /**
   * filtersNeedResync says that the filters differ from the saved ones of
   * the same pair, so its next run needs a resync.
   */
  filtersNeedResync?: boolean;
  /** idPrefix keeps input IDs unique when two editors share a page. */
  idPrefix?: string;
}

const compareOptions: { key: keyof CompareState; label: string }[] = [
  { key: "size", label: "Size" },
  { key: "modtime", label: "Modification time" },
  { key: "checksum", label: "Checksum" },
];

/**
 * BisyncEditor edits a two-way sync (rclone bisync): its two folders, the
 * safety checks, how conflicts are kept, how changes are found, what a
 * resync keeps, the filters and the tuning options. resync and force are
 * never edited here: they belong to a single run, started from a failed
 * job (BisyncRecovery). JobDefinitionEditor shows it for the Two-way sync
 * kind.
 */
export function BisyncEditor({
  value: s,
  onChange,
  errors,
  remotes,
  backends,
  worker,
  filtersNeedResync,
  idPrefix = "run",
}: BisyncEditorProps) {
  const set = <K extends keyof BisyncState>(k: K, v: BisyncState[K]) => onChange({ ...s, [k]: v });
  const id = (name: string) => `${idPrefix}-bisync-${name}`;
  return (
    <>
      <Panel
        title="Folders"
        description="Changes on either side spread to the other: new files, edits and deletions. Folder A is rclone's path 1, Folder B its path 2."
      >
        <div className="grid items-start gap-6 md:grid-cols-[minmax(0,1fr)_2rem_minmax(0,1fr)]">
          <LocationEditor
            id={`${idPrefix}-a`}
            label="Folder A"
            value={s.path1}
            onChange={(v) => set("path1", v)}
            remotes={remotes}
            backends={backends}
            worker={worker}
            errors={{ remote: errors.path1Remote, path: errors.path1Path }}
          />
          <div className="flex justify-center text-muted-foreground md:pt-14" aria-hidden>
            <ArrowLeftRight className="hidden size-5 md:block" />
            <ArrowDownUp className="size-5 md:hidden" />
          </div>
          <LocationEditor
            id={`${idPrefix}-b`}
            label="Folder B"
            value={s.path2}
            onChange={(v) => set("path2", v)}
            remotes={remotes}
            backends={backends}
            worker={worker}
            errors={{ remote: errors.path2Remote, path: errors.path2Path }}
          />
        </div>
        {errors.options && (
          <p role="alert" className="mt-4 text-xs leading-5 text-danger">
            {errors.options}
          </p>
        )}
      </Panel>

      <Panel
        title="Safety"
        description="Guards against a drive that is not mounted or a wrong path, which would make every file look deleted."
      >
        <div className="grid gap-x-8 gap-y-5 md:grid-cols-2">
          <NumberInput
            id={id("max-delete")}
            label="Delete limit"
            unit="%"
            value={s.maxDelete}
            onChange={(v) => set("maxDelete", v)}
            placeholder={String(DEFAULT_MAX_DELETE)}
            error={errors.maxDelete}
            help="A run that would delete more than this share of the files on either side stops with “Safety check stopped the sync”. If the deletions were intended, run it once anyway from the job page."
          />
          <Toggle
            id={id("check-access")}
            label="Check access"
            help="Run only when both folders hold an RCLONE_TEST file. Create an empty file with that name in each folder first: if a drive is not mounted or a path is wrong, the file is missing and the run stops before it changes anything."
            checked={s.checkAccess}
            onChange={(v) => set("checkAccess", v)}
          />
        </div>
      </Panel>

      <Panel
        title="Conflicts"
        description="A conflict is a file changed on both sides since the last run."
      >
        <div className="grid gap-x-8 gap-y-5 md:grid-cols-2">
          <div className="flex flex-col gap-5">
            <FieldShell
              id={id("conflict-resolve")}
              label="Which version wins"
              error={errors.conflicts}
              help="The winner keeps the file's name on both sides. When the versions cannot be told apart that way, both are kept."
            >
              <Select
                value={String(s.conflictResolve)}
                onValueChange={(v) => set("conflictResolve", Number(v))}
              >
                <SelectTrigger
                  id={id("conflict-resolve")}
                  className="w-full bg-card"
                  aria-invalid={errors.conflicts ? true : undefined}
                >
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {conflictResolutions.map((c) => (
                    <SelectItem key={c.value} value={String(c.value)}>
                      {c.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </FieldShell>
            <FieldShell
              id={id("conflict-suffix")}
              label="Suffix"
              optional
              error={errors.conflictSuffix}
              help="Added to the names of the versions kept, e.g. report.docx.conflict1. One suffix, or two separated by a comma: Folder A's, then Folder B's."
            >
              <Input
                id={id("conflict-suffix")}
                className="bg-card font-mono text-[13px]"
                value={s.conflictSuffix}
                placeholder="conflict"
                spellCheck={false}
                autoComplete="off"
                aria-invalid={errors.conflictSuffix ? true : undefined}
                aria-describedby={
                  errors.conflictSuffix
                    ? `${id("conflict-suffix")}-error`
                    : `${id("conflict-suffix")}-help`
                }
                onChange={(e) => set("conflictSuffix", e.target.value)}
              />
            </FieldShell>
          </div>
          <ChoiceCards
            label="Name the versions kept"
            labelId={id("conflict-loser")}
            value={s.conflictLoser}
            onChange={(v) => set("conflictLoser", v)}
            options={[
              {
                value: BisyncConflictLoser.NUM,
                label: "Numbered (recommended)",
                help: "report.docx.conflict1, .conflict2 and so on: a later conflict takes the next free number, so no kept version is ever replaced.",
              },
              {
                value: BisyncConflictLoser.PATHNAME,
                label: "By side",
                help: "report.docx.conflict1 is Folder A's version, .conflict2 Folder B's. A later conflict of the same file replaces them.",
              },
            ]}
          />
        </div>
      </Panel>

      <div className="grid gap-6 lg:grid-cols-2">
        <Panel title="Compare" description="How a run tells that a file changed.">
          <fieldset className="flex flex-col gap-2.5" aria-describedby={id("compare-help")}>
            <legend className="sr-only">Compare files by</legend>
            {compareOptions.map((c) => (
              <div key={c.key} className="flex items-center gap-2.5">
                <Checkbox
                  id={id(`compare-${c.key}`)}
                  checked={s.compare[c.key]}
                  onCheckedChange={(v) => set("compare", { ...s.compare, [c.key]: v === true })}
                  aria-invalid={errors.compare ? true : undefined}
                />
                <Label htmlFor={id(`compare-${c.key}`)} className="text-[13px] font-normal">
                  {c.label}
                </Label>
              </div>
            ))}
            {errors.compare ? (
              <p role="alert" className="text-xs leading-5 text-danger">
                {errors.compare}
              </p>
            ) : (
              <p id={id("compare-help")} className="text-xs leading-5 text-muted-foreground">
                Size and modification time is the default. A checksum is exact but slower, and both
                sides need a hash in common.
              </p>
            )}
          </fieldset>
        </Panel>
        <Panel
          title="Resync"
          description="What a resync keeps of a file that differs on the two sides."
        >
          <FieldShell
            id={id("resync-mode")}
            label="Keep"
            error={errors.resyncMode}
            help="A resync merges both folders: each gets the files it lacks and nothing is deleted; the other copy of a differing file is overwritten. The first run on a worker is a resync, and so is Resync now."
          >
            <Select
              value={String(s.resyncMode)}
              onValueChange={(v) => set("resyncMode", Number(v) as BisyncResyncMode)}
            >
              <SelectTrigger id={id("resync-mode")} className="w-full bg-card">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {resyncModes.map((m, i) => (
                  <SelectItem key={m.value} value={String(m.value)}>
                    {m.label}
                    {i === 0 ? " (default)" : ""}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </FieldShell>
        </Panel>
      </div>

      <Panel title="Filters">
        <FilterRuleList
          value={s.filters}
          onChange={(v) => set("filters", v)}
          error={errors.filters}
          help={
            <>
              rclone filter patterns, applied in order to both folders. The first match decides; for
              example exclude <code className="rounded bg-muted px-1">*.tmp</code>. Changing them
              later needs a resync.
            </>
          }
          notice={
            filtersNeedResync && (
              <p
                role="status"
                className="rounded-md bg-warning-soft px-3 py-2 text-[13px] leading-5 text-warning"
              >
                The filters changed, so the pair's next run will stop with “Resync required”. Then
                Resync now, on that job's page or the schedule's, accepts the new filters.
              </p>
            )
          }
        />
      </Panel>

      <Panel title="Advanced" description="Leave a field empty to use rclone's default.">
        <div className="flex flex-col gap-6">
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
            <NumberInput
              id={id("transfers")}
              label="Parallel transfers"
              value={s.transfers}
              onChange={(v) => set("transfers", v)}
              placeholder="4"
              error={errors.transfers}
            />
            <NumberInput
              id={id("checkers")}
              label="Parallel checkers"
              value={s.checkers}
              onChange={(v) => set("checkers", v)}
              placeholder="8"
              error={errors.checkers}
            />
            <QuantityInput
              id={id("bw")}
              label="Bandwidth limit"
              value={s.bwlimit}
              unit={s.bwlimitUnit}
              units={byteUnits.map((u) => ({ id: u.id, label: `${u.id}/s` }))}
              onChange={(bwlimit, bwlimitUnit) => onChange({ ...s, bwlimit, bwlimitUnit })}
              placeholder="Unlimited"
              error={errors.bwlimit}
            />
            <NumberInput
              id={id("llr")}
              label="Low-level retries"
              value={s.lowLevelRetries}
              onChange={(v) => set("lowLevelRetries", v)}
              placeholder="10"
              error={errors.lowLevelRetries}
            />
          </div>
          <div className="grid gap-x-8 gap-y-4 md:grid-cols-2">
            <Toggle
              id={id("empty-dirs")}
              label="Sync empty folders"
              help="Create and delete empty folders too."
              checked={s.createEmptySrcDirs}
              onChange={(v) => set("createEmptySrcDirs", v)}
            />
            <Toggle
              id={id("dry")}
              label="Dry run"
              help="Report what would change without changing anything. The pair's state is kept as it was."
              checked={s.dryRun}
              onChange={(v) => set("dryRun", v)}
            />
          </div>
        </div>
      </Panel>
    </>
  );
}
