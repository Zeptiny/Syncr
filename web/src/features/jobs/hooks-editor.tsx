import { Plus, X } from "lucide-react";

import { FieldShell } from "@/components/app/descriptor-form";
import { Panel } from "@/components/app/panel";
import { Select, SelectContent, SelectItem, SelectTrigger } from "@/components/ui/select";
import { formatLimit } from "@/lib/format";
import { cn } from "@/lib/utils";

import {
  hasHooks,
  hookPhases,
  hooksNote,
  MAX_HOOKS_PER_PHASE,
  offeredHooks,
  unavailableHooks,
  type HookChoice,
  type HookPhaseMeta,
  type HooksState,
  type HooksTarget,
} from "./hooks";

/** HooksFieldsProps configures HooksFields. */
export interface HooksFieldsProps {
  value: HooksState;
  onChange: (v: HooksState) => void;
  /**
   * target is where the job runs, as far as hooks go: the workers it may
   * run on that have reported in, and the label when it runs on any worker
   * with one.
   */
  target: HooksTarget;
  /** error is a problem with the hooks as a whole (the server's, usually). */
  error?: string;
  /** idPrefix keeps input IDs unique on the page. */
  idPrefix: string;
  /** className lays out the four lists (default: two columns on wide screens). */
  className?: string;
}

/**
 * hooksEmptyText says why HooksFields has nothing to offer; undefined when
 * it has (the target offers hooks, or the form already names some).
 */
export function hooksEmptyText(value: HooksState, target: HooksTarget): string | undefined {
  if (hasHooks(value) || offeredHooks(target.workers).length > 0) return undefined;
  if (target.workers.length === 0) {
    return "Choose where the job runs to see the hooks its worker offers.";
  }
  const note = hooksNote(value, target);
  if (note && note.includes("upgraded")) return note;
  const who = target.label
    ? `The workers labelled ${target.label} define`
    : `${target.workers[0].name} defines`;
  return `${who} no hooks. A worker's operator adds them to its configuration file, under “hooks”.`;
}

/**
 * HooksFields picks the hooks a job runs around its operation: for each of
 * the four phases, an ordered list of the hooks its worker offers. A hook
 * the target does not offer (the worker was changed, or its operator
 * removed the hook) stays in its list, marked, until the user removes it:
 * the form never drops a choice silently.
 */
export function HooksFields({
  value,
  onChange,
  target,
  error,
  idPrefix,
  className,
}: HooksFieldsProps) {
  const offered = offeredHooks(target.workers);
  const unavailable = new Set(unavailableHooks(value, target));
  const note = hooksNote(value, target);
  const who = target.label
    ? `No worker labelled ${target.label} defines`
    : `${target.workers[0]?.name ?? "The worker"} does not define`;
  return (
    <div className="flex flex-col gap-5">
      <div className={cn("grid gap-x-8 gap-y-6 lg:grid-cols-2", className)}>
        {hookPhases.map((phase) => (
          <HookList
            key={phase.key}
            id={`${idPrefix}-hooks-${phase.field}`}
            phase={phase}
            value={value[phase.key]}
            onChange={(names) => onChange({ ...value, [phase.key]: names })}
            offered={offered}
            unavailable={unavailable}
            unavailableTitle={`${who} this hook`}
            showWorkers={!!target.label && target.workers.length > 1}
            workerCount={target.workers.length}
          />
        ))}
      </div>
      {unavailable.size > 0 && (
        <p role="alert" className="text-xs leading-5 text-danger">
          {who} {unavailable.size === 1 ? "the hook" : "the hooks"} {[...unavailable].join(", ")}.
          Remove {unavailable.size === 1 ? "it" : "them"}, or choose another place to run.
        </p>
      )}
      {error && (
        <p role="alert" className="text-xs leading-5 text-danger">
          {error}
        </p>
      )}
      {note && <p className="text-xs leading-5 text-muted-foreground">{note}</p>}
    </div>
  );
}

function HookList({
  id,
  phase,
  value,
  onChange,
  offered,
  unavailable,
  unavailableTitle,
  showWorkers,
  workerCount,
}: {
  id: string;
  phase: HookPhaseMeta;
  value: string[];
  onChange: (names: string[]) => void;
  offered: HookChoice[];
  unavailable: Set<string>;
  unavailableTitle: string;
  showWorkers: boolean;
  workerCount: number;
}) {
  const free = offered.filter((h) => !value.includes(h.name));
  const full = value.length >= MAX_HOOKS_PER_PHASE;
  const add = `Add a hook: ${phase.label.toLowerCase()}`;
  return (
    <FieldShell id={id} label={phase.label} help={phase.help}>
      {value.length > 0 && (
        <ol className="flex flex-wrap gap-1.5" aria-label={`Hooks: ${phase.label.toLowerCase()}`}>
          {value.map((name, i) => {
            const missing = unavailable.has(name);
            return (
              <li
                key={name}
                title={missing ? unavailableTitle : undefined}
                className={cn(
                  "inline-flex h-7 items-center gap-1.5 rounded-md border bg-secondary pr-0.5 pl-2 font-mono text-[12.5px] text-secondary-foreground",
                  missing && "border-danger/40 bg-danger-soft text-danger",
                )}
              >
                {value.length > 1 && (
                  <span className="tabular text-[11px] text-muted-foreground" aria-hidden>
                    {i + 1}
                  </span>
                )}
                {name}
                {missing && <span className="sr-only"> (not defined)</span>}
                <button
                  type="button"
                  onClick={() => onChange(value.filter((n) => n !== name))}
                  className="grid size-5 place-items-center rounded text-muted-foreground hover:bg-background hover:text-foreground"
                  aria-label={`Remove hook ${name}: ${phase.label.toLowerCase()}`}
                >
                  <X className="size-3" aria-hidden />
                </button>
              </li>
            );
          })}
        </ol>
      )}
      {free.length > 0 && !full ? (
        <Select value="" onValueChange={(name) => onChange([...value, name])}>
          <SelectTrigger id={id} className="w-full bg-card sm:w-64" aria-label={add}>
            <span className="flex items-center gap-2 text-muted-foreground">
              <Plus className="size-4" aria-hidden />
              {value.length ? "Add another hook" : "Add a hook"}
            </span>
          </SelectTrigger>
          <SelectContent position="popper" align="start" className="max-w-[26rem]">
            {free.map((h) => (
              <SelectItem key={h.name} value={h.name} textValue={h.name}>
                <span className="flex min-w-0 flex-col gap-0.5 py-0.5 text-left">
                  <span className="font-mono text-[13px]">{h.name}</span>
                  <span className="text-xs whitespace-normal text-muted-foreground">
                    {[
                      h.description,
                      h.timeoutSeconds > 0 ? `up to ${formatLimit(h.timeoutSeconds)}` : "",
                      showWorkers ? `on ${h.workers} of ${workerCount} workers` : "",
                    ]
                      .filter(Boolean)
                      .join(" · ")}
                  </span>
                </span>
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      ) : (
        value.length === 0 && (
          <p id={id} className="text-[13px] text-muted-foreground">
            None
          </p>
        )
      )}
      {full && (
        <p className="text-xs leading-5 text-muted-foreground">
          A phase runs at most {MAX_HOOKS_PER_PHASE} hooks.
        </p>
      )}
    </FieldShell>
  );
}

/** HooksPanelProps configures HooksPanel. */
export type HooksPanelProps = Omit<HooksFieldsProps, "className">;

/**
 * HooksPanel is the hooks section of a job form: the four lists, or a line
 * saying why there is nothing to choose.
 */
export function HooksPanel(props: HooksPanelProps) {
  const empty = hooksEmptyText(props.value, props.target);
  return (
    <Panel
      title="Hooks"
      description="Commands the worker's operator prepared, run on the worker around the job: stop an app or dump a database before a backup, and start the app again afterwards."
    >
      {empty ? (
        <p className="text-[13px] text-muted-foreground">{empty}</p>
      ) : (
        <HooksFields {...props} />
      )}
      {empty && props.error && (
        <p role="alert" className="mt-2 text-xs leading-5 text-danger">
          {props.error}
        </p>
      )}
    </Panel>
  );
}
