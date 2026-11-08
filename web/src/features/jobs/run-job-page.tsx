import { durationFromMs } from "@bufbuild/protobuf/wkt";
import type { MessageInitShape } from "@bufbuild/protobuf";
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { ArrowDown, ArrowRight, ChevronRight, Plus, Trash2 } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { FieldShell } from "@/components/app/descriptor-form";
import { LocationEditor, type Location } from "@/components/app/location-editor";
import { PageHeader } from "@/components/app/page-header";
import { Panel } from "@/components/app/panel";
import { WorkerPicker } from "@/components/app/worker-picker";
import { Button } from "@/components/ui/button";
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
import {
  JobService,
  WorkerAdminService,
  type Job,
  type RunJobRequestSchema,
} from "@/gen/syncr/v1/api_pb";
import { FilterAction, RcloneTransferMode } from "@/gen/syncr/v1/link_pb";
import { RemoteService } from "@/gen/syncr/v1/remotes_pb";
import { useBackendMap } from "@/hooks/use-backends";
import { errorMessage, fieldViolations } from "@/lib/errors";
import { durationSeconds, toNumber } from "@/lib/format";
import { invalidateService } from "@/lib/invalidate";
import { transferModes } from "@/lib/status";
import { cn } from "@/lib/utils";

import { transferOf } from "./model";

type Compare = "default" | "checksum" | "size";

/** RunJobState is the run-job form. Numbers are kept as typed text. */
export interface RunJobState {
  workerId: string;
  mode: RcloneTransferMode;
  source: Location;
  destination: Location;
  transfers: string;
  checkers: string;
  bwlimitMiB: string;
  dryRun: boolean;
  compare: Compare;
  ignoreExisting: boolean;
  update: boolean;
  createEmptySrcDirs: boolean;
  deleteEmptySrcDirs: boolean;
  trackRenames: boolean;
  fastList: boolean;
  maxTransferGiB: string;
  maxDurationMin: string;
  retries: string;
  lowLevelRetries: string;
  filters: { action: FilterAction; pattern: string }[];
}

const MiB = 1024 ** 2;
const GiB = 1024 ** 3;

/** emptyRunJob is a blank form. */
export function emptyRunJob(): RunJobState {
  return {
    workerId: "",
    mode: RcloneTransferMode.COPY,
    source: { remoteId: "", path: "" },
    destination: { remoteId: "", path: "" },
    transfers: "",
    checkers: "",
    bwlimitMiB: "",
    dryRun: false,
    compare: "default",
    ignoreExisting: false,
    update: false,
    createEmptySrcDirs: false,
    deleteEmptySrcDirs: false,
    trackRenames: false,
    fastList: false,
    maxTransferGiB: "",
    maxDurationMin: "",
    retries: "",
    lowLevelRetries: "",
    filters: [],
  };
}

const num = (v: number | bigint | undefined, scale = 1) =>
  v === undefined ? "" : String(Math.round((toNumber(v) / scale) * 100) / 100);

/** fromJob prefills the form from an earlier job ("Edit and run"). */
export function fromJob(job: Job): RunJobState {
  const t = transferOf(job);
  const base = emptyRunJob();
  if (!t) return { ...base, workerId: job.workerId };
  const o = t.options;
  return {
    ...base,
    workerId: job.workerId,
    mode: t.mode || RcloneTransferMode.COPY,
    source: { remoteId: t.source?.remoteId ?? "", path: t.source?.path ?? "" },
    destination: { remoteId: t.destination?.remoteId ?? "", path: t.destination?.path ?? "" },
    transfers: num(o?.transfers),
    checkers: num(o?.checkers),
    bwlimitMiB: num(o?.bandwidthLimitBytesPerSecond, MiB),
    dryRun: o?.dryRun ?? false,
    compare: o?.checksum ? "checksum" : o?.sizeOnly ? "size" : "default",
    ignoreExisting: o?.ignoreExisting ?? false,
    update: o?.update ?? false,
    createEmptySrcDirs: o?.createEmptySrcDirs ?? false,
    deleteEmptySrcDirs: o?.deleteEmptySrcDirs ?? false,
    trackRenames: o?.trackRenames ?? false,
    fastList: o?.fastList ?? false,
    maxTransferGiB: num(o?.maxTransferBytes, GiB),
    maxDurationMin: o?.maxDuration ? String(durationSeconds(o.maxDuration) / 60) : "",
    retries: num(o?.retries),
    lowLevelRetries: num(o?.lowLevelRetries),
    filters: (o?.filters ?? []).map((f) => ({ action: f.action, pattern: f.pattern })),
  };
}

type Errors = Partial<
  Record<"worker" | "sourceRemote" | "sourcePath" | "destRemote" | "destPath" | "options", string>
>;

function isAbsolute(p: string): boolean {
  return p.startsWith("/") || /^[A-Za-z]:[\\/]/.test(p) || p.startsWith("\\\\");
}

/** validateRunJob checks what the browser can check before sending. */
export function validateRunJob(s: RunJobState): Errors {
  const e: Errors = {};
  if (!s.workerId) e.worker = "Choose the worker that runs the job.";
  for (const [loc, path] of [
    [s.source, "sourcePath"],
    [s.destination, "destPath"],
  ] as const) {
    if (loc.remoteId === "" && !isAbsolute(loc.path.trim())) {
      e[path] = "Enter an absolute path on the worker, such as /srv/data.";
    }
  }
  if (
    s.source.remoteId === s.destination.remoteId &&
    s.source.path.trim() === s.destination.path.trim()
  ) {
    e.destPath = "The destination is the same as the source.";
  }
  const ints = [s.transfers, s.checkers, s.retries, s.lowLevelRetries];
  const decimals = [s.bwlimitMiB, s.maxTransferGiB, s.maxDurationMin];
  if (
    ints.some((v) => v !== "" && !/^\d+$/.test(v)) ||
    decimals.some((v) => v !== "" && !(Number(v) >= 0))
  ) {
    e.options = "Numbers must be zero or more.";
  }
  return e;
}

const optInt = (v: string) => (v === "" ? undefined : Number(v));
const optBig = (v: string, scale: number) =>
  v === "" ? undefined : BigInt(Math.round(Number(v) * scale));

/** toRunJobRequest builds the RunJob request from the form. */
export function toRunJobRequest(s: RunJobState): MessageInitShape<typeof RunJobRequestSchema> {
  const mode = s.mode;
  return {
    workerId: s.workerId,
    definition: {
      operation: {
        case: "rcloneTransfer",
        value: {
          mode,
          source: { remoteId: s.source.remoteId, path: s.source.path.trim() },
          destination: { remoteId: s.destination.remoteId, path: s.destination.path.trim() },
          options: {
            transfers: optInt(s.transfers),
            checkers: optInt(s.checkers),
            bandwidthLimitBytesPerSecond: optBig(s.bwlimitMiB, MiB),
            dryRun: s.dryRun,
            checksum: s.compare === "checksum",
            sizeOnly: s.compare === "size",
            ignoreExisting: s.ignoreExisting,
            update: s.update,
            createEmptySrcDirs: s.createEmptySrcDirs,
            deleteEmptySrcDirs: mode === RcloneTransferMode.MOVE && s.deleteEmptySrcDirs,
            trackRenames: mode === RcloneTransferMode.SYNC && s.trackRenames,
            fastList: s.fastList,
            maxTransferBytes: optBig(s.maxTransferGiB, GiB),
            maxDuration:
              s.maxDurationMin === ""
                ? undefined
                : durationFromMs(Number(s.maxDurationMin) * 60_000),
            retries: optInt(s.retries),
            lowLevelRetries: optInt(s.lowLevelRetries),
            filters: s.filters
              .filter((f) => f.pattern.trim())
              .map((f) => ({ action: f.action, pattern: f.pattern.trim() })),
          },
        },
      },
    },
  };
}

/** serverErrors places API violations on the form's sections. */
function serverErrors(err: unknown): { errors: Errors; rest: string[] } {
  const errors: Errors = {};
  const rest: string[] = [];
  for (const v of fieldViolations(err)) {
    const f = v.field;
    if (f.includes("worker")) errors.worker = v.message;
    else if (f.includes("source"))
      errors[f.endsWith("remote_id") ? "sourceRemote" : "sourcePath"] = v.message;
    else if (f.includes("destination"))
      errors[f.endsWith("remote_id") ? "destRemote" : "destPath"] = v.message;
    else if (f.includes("options")) errors.options = v.message;
    else rest.push(`${f}: ${v.message}`);
  }
  return { errors, rest };
}

function Toggle({
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

function NumberInput({
  id,
  label,
  value,
  onChange,
  unit,
  placeholder,
}: {
  id: string;
  label: string;
  value: string;
  onChange: (v: string) => void;
  unit?: string;
  placeholder?: string;
}) {
  return (
    <FieldShell id={id} label={label}>
      <div className="relative">
        <Input
          id={id}
          inputMode="decimal"
          className={cn("tabular bg-card", unit && "pr-14")}
          value={value}
          placeholder={placeholder ?? "Default"}
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

/** RunJobPage queues a transfer on a worker. */
export function RunJobPage({ from }: { from?: Job }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const backends = useBackendMap();
  const workers = useQuery(WorkerAdminService.method.listWorkers, {});
  const remotes = useQuery(RemoteService.method.listRemotes, {});
  const run = useMutation(JobService.method.runJob);
  const [s, setS] = useState<RunJobState>(() => (from ? fromJob(from) : emptyRunJob()));
  const [errors, setErrors] = useState<Errors>({});
  const [formError, setFormError] = useState<string | undefined>();
  const [more, setMore] = useState(false);
  const set = <K extends keyof RunJobState>(k: K, v: RunJobState[K]) =>
    setS((p) => ({ ...p, [k]: v }));
  const worker = workers.data?.workers.find((w) => w.id === s.workerId);
  const remoteList = remotes.data?.remotes ?? [];

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setFormError(undefined);
    const local = validateRunJob(s);
    setErrors(local);
    if (Object.keys(local).length) return;
    try {
      const res = await run.mutateAsync(toRunJobRequest(s));
      await invalidateService(queryClient, JobService);
      toast.success(s.dryRun ? "Dry run queued" : "Job queued");
      if (res.job) await navigate({ to: "/jobs/$jobId", params: { jobId: res.job.id } });
    } catch (err) {
      const { errors: server, rest } = serverErrors(err);
      setErrors(server);
      if (rest.length) setFormError(rest.join(" "));
      else if (!Object.keys(server).length) setFormError(errorMessage(err));
    }
  };

  return (
    <>
      <PageHeader
        back={{ to: "/jobs", label: "Jobs" }}
        title={from ? "Run again with changes" : "Run a job"}
        description="Choose where the files are, where they go and which worker moves them. The job starts as soon as the worker picks it up."
      />
      <form onSubmit={(e) => void submit(e)} noValidate className="flex max-w-5xl flex-col gap-6">
        <Panel title="How">
          <div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_minmax(0,2fr)]">
            <FieldShell
              id="run-worker"
              label="Worker"
              error={errors.worker}
              help="Online workers are listed first."
            >
              <WorkerPicker
                id="run-worker"
                workers={workers.data?.workers ?? []}
                value={s.workerId}
                onChange={(v) => set("workerId", v)}
                invalid={!!errors.worker}
                className="bg-card"
              />
            </FieldShell>
            <div className="flex flex-col gap-1.5">
              <span className="text-[13px] font-medium" id="run-mode-label">
                Mode
              </span>
              <div
                role="radiogroup"
                aria-labelledby="run-mode-label"
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
              id="run-src"
              label="From"
              value={s.source}
              onChange={(v) => set("source", v)}
              remotes={remoteList}
              backends={backends}
              worker={worker}
              errors={{ remote: errors.sourceRemote, path: errors.sourcePath }}
            />
            <div className="flex justify-center text-muted-foreground md:pt-14" aria-hidden>
              <ArrowRight className="hidden size-5 md:block" />
              <ArrowDown className="size-5 md:hidden" />
            </div>
            <LocationEditor
              id="run-dst"
              label="To"
              value={s.destination}
              onChange={(v) => set("destination", v)}
              remotes={remoteList}
              backends={backends}
              worker={worker}
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
              <NumberInput
                id="opt-bw"
                label="Bandwidth limit"
                value={s.bwlimitMiB}
                onChange={(v) => set("bwlimitMiB", v)}
                unit="MiB/s"
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
                    <NumberInput
                      id="opt-maxt"
                      label="Stop after"
                      value={s.maxTransferGiB}
                      onChange={(v) => set("maxTransferGiB", v)}
                      unit="GiB"
                      placeholder="No limit"
                    />
                    <NumberInput
                      id="opt-maxd"
                      label="Stop starting after"
                      value={s.maxDurationMin}
                      onChange={(v) => set("maxDurationMin", v)}
                      unit="min"
                      placeholder="No limit"
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
                  <div className="flex flex-col gap-2">
                    <div>
                      <div className="text-[13px] font-medium">Filters</div>
                      <p className="text-xs leading-5 text-muted-foreground">
                        rclone filter patterns, applied in order. The first match decides; for
                        example exclude <code className="rounded bg-muted px-1">*.tmp</code>.
                      </p>
                    </div>
                    {s.filters.map((f, i) => (
                      <div key={i} className="flex items-center gap-2">
                        <Select
                          value={String(f.action)}
                          onValueChange={(v) =>
                            set(
                              "filters",
                              s.filters.map((x, j) => (j === i ? { ...x, action: Number(v) } : x)),
                            )
                          }
                        >
                          <SelectTrigger
                            className="w-32 bg-card"
                            aria-label={`Filter ${i + 1} action`}
                          >
                            <SelectValue />
                          </SelectTrigger>
                          <SelectContent>
                            <SelectItem value={String(FilterAction.EXCLUDE)}>Exclude</SelectItem>
                            <SelectItem value={String(FilterAction.INCLUDE)}>Include</SelectItem>
                          </SelectContent>
                        </Select>
                        <Input
                          aria-label={`Filter ${i + 1} pattern`}
                          className="bg-card font-mono text-[13px]"
                          value={f.pattern}
                          placeholder="*.tmp"
                          onChange={(e) =>
                            set(
                              "filters",
                              s.filters.map((x, j) =>
                                j === i ? { ...x, pattern: e.target.value } : x,
                              ),
                            )
                          }
                        />
                        <Button
                          type="button"
                          variant="ghost"
                          size="icon"
                          aria-label={`Remove filter ${i + 1}`}
                          onClick={() =>
                            set(
                              "filters",
                              s.filters.filter((_, j) => j !== i),
                            )
                          }
                        >
                          <Trash2 aria-hidden />
                        </Button>
                      </div>
                    ))}
                    <div>
                      <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        onClick={() =>
                          set("filters", [
                            ...s.filters,
                            { action: FilterAction.EXCLUDE, pattern: "" },
                          ])
                        }
                      >
                        <Plus aria-hidden />
                        Add filter
                      </Button>
                    </div>
                  </div>
                </div>
              </CollapsibleContent>
            </Collapsible>
          </div>
        </Panel>

        {formError && (
          <p role="alert" className="rounded-md bg-danger-soft px-3 py-2 text-[13px] text-danger">
            {formError}
          </p>
        )}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="outline" onClick={() => void navigate({ to: "/jobs" })}>
            Cancel
          </Button>
          <Button type="submit" disabled={run.isPending}>
            {run.isPending ? "Queuing…" : s.dryRun ? "Start dry run" : "Run job"}
          </Button>
        </div>
      </form>
    </>
  );
}
