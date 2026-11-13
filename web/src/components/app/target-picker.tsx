import { AlertTriangle, Info, Server, Tag } from "lucide-react";

import { Input } from "@/components/ui/input";
import { WorkerStatus, type Worker } from "@/gen/syncr/v1/api_pb";
import { labelledWorkers, labelProblem } from "@/lib/labels";
import { cn } from "@/lib/utils";

import { FieldShell } from "./descriptor-form";
import { WorkerPicker } from "./worker-picker";

/** Target is a schedule's target: one worker, or any worker with a label. */
export interface Target {
  kind: "worker" | "label";
  workerId: string;
  label: string;
}

/** TargetPickerProps configures TargetPicker. */
export interface TargetPickerProps {
  id: string;
  value: Target;
  onChange: (t: Target) => void;
  workers: Worker[];
  error?: string;
  /**
   * labelDisabled, when set, is why the job cannot run on a label (e.g. a
   * two-way sync keeps its state on one worker): the label choice is
   * disabled and the reason shown.
   */
  labelDisabled?: string;
}

/**
 * TargetPicker chooses where a schedule's jobs run: a specific worker, or
 * the least busy online worker with a label. For a label it shows which
 * workers have it, and warns when none does yet.
 */
export function TargetPicker({
  id,
  value,
  onChange,
  workers,
  error,
  labelDisabled,
}: TargetPickerProps) {
  const allLabels = [...new Set(workers.flatMap((w) => w.labels))].sort();
  const matching = labelledWorkers(workers, value.label);
  const online = matching.filter((w) => w.status === WorkerStatus.ONLINE);
  const typed = value.label.trim().toLowerCase();
  const bad = typed ? labelProblem(typed) : undefined;

  const tabs: { kind: Target["kind"]; label: string; icon: typeof Server }[] = [
    { kind: "worker", label: "A specific worker", icon: Server },
    { kind: "label", label: "Any worker with a label", icon: Tag },
  ];

  return (
    <div className="flex flex-col gap-4">
      <div
        role="radiogroup"
        aria-label="Run on"
        className="flex flex-col rounded-lg border bg-muted p-0.5 sm:flex-row sm:self-start"
      >
        {tabs.map((t) => {
          const on = value.kind === t.kind;
          const disabled = t.kind === "label" && !!labelDisabled;
          return (
            <button
              key={t.kind}
              type="button"
              role="radio"
              aria-checked={on}
              disabled={disabled}
              aria-describedby={disabled ? `${id}-label-disabled` : undefined}
              onClick={() => onChange({ ...value, kind: t.kind })}
              className={cn(
                "inline-flex flex-1 items-center justify-center gap-1.5 rounded-md px-3 py-1.5 text-[13px] font-medium whitespace-nowrap transition-colors disabled:cursor-not-allowed disabled:opacity-50 sm:flex-none",
                on
                  ? "bg-card text-foreground shadow-xs"
                  : "text-muted-foreground enabled:hover:text-foreground",
              )}
            >
              <t.icon className="size-3.5" aria-hidden />
              {t.label}
            </button>
          );
        })}
      </div>

      {labelDisabled && (
        <p
          id={`${id}-label-disabled`}
          className="-mt-1 flex items-start gap-1.5 text-xs leading-5 text-muted-foreground"
        >
          <Info className="mt-0.5 size-3.5 shrink-0" aria-hidden />
          {labelDisabled}
        </p>
      )}

      {value.kind === "worker" ? (
        <FieldShell
          id={`${id}-worker`}
          label="Worker"
          error={error}
          help="If it is offline when a run is due, the missed-run policy below decides what happens."
        >
          <WorkerPicker
            id={`${id}-worker`}
            workers={workers}
            value={value.workerId}
            onChange={(workerId) => onChange({ ...value, workerId })}
            invalid={!!error}
            className="bg-card sm:max-w-md"
          />
        </FieldShell>
      ) : (
        <FieldShell
          id={`${id}-label`}
          label="Label"
          error={error ?? bad}
          help="Each run goes to the least busy online worker with this label that can run the job."
        >
          <Input
            id={`${id}-label`}
            value={value.label}
            onChange={(e) => onChange({ ...value, label: e.target.value })}
            placeholder="nas"
            className="bg-card font-mono text-[13px] sm:max-w-xs"
            spellCheck={false}
            autoComplete="off"
            list={`${id}-labels`}
            aria-invalid={error || bad ? true : undefined}
          />
          <datalist id={`${id}-labels`}>
            {allLabels.map((l) => (
              <option key={l} value={l} />
            ))}
          </datalist>
          {allLabels.length > 0 && (
            <div className="flex flex-wrap items-center gap-1.5 pt-1 text-xs text-muted-foreground">
              In use:
              {allLabels.map((l) => (
                <button
                  key={l}
                  type="button"
                  onClick={() => onChange({ ...value, label: l })}
                  className={cn(
                    "rounded px-1.5 py-0.5 font-mono transition-colors",
                    l === typed
                      ? "bg-primary text-primary-foreground"
                      : "bg-muted hover:bg-accent hover:text-foreground",
                  )}
                >
                  {l}
                </button>
              ))}
            </div>
          )}
        </FieldShell>
      )}

      {value.kind === "label" && typed && !bad && (
        <div
          className={cn(
            "flex items-start gap-2.5 rounded-lg px-3.5 py-2.5 text-[13px]",
            matching.length ? "bg-muted/70" : "bg-warning-soft text-warning",
          )}
          role="status"
        >
          {matching.length ? (
            <Server className="mt-0.5 size-4 shrink-0 text-muted-foreground" aria-hidden />
          ) : (
            <AlertTriangle className="mt-0.5 size-4 shrink-0" aria-hidden />
          )}
          {matching.length ? (
            <span>
              {matching.length === 1 ? "1 worker has" : `${matching.length} workers have`} this
              label:{" "}
              {matching.map((w, i) => (
                <span key={w.id}>
                  {i > 0 && ", "}
                  <span className="font-medium">{w.name}</span>
                  <span className="text-muted-foreground">
                    {" "}
                    ({w.status === WorkerStatus.ONLINE ? "online" : "offline"})
                  </span>
                </span>
              ))}
              {online.length === 0 && ". None is online right now."}
            </span>
          ) : (
            <span>
              No worker has the label <span className="font-mono">{typed}</span> yet. Runs fail
              until an administrator adds it to a worker.
            </span>
          )}
        </div>
      )}
    </div>
  );
}
