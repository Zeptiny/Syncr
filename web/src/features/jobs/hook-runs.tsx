import { ChevronRight } from "lucide-react";
import { useState } from "react";

import { Panel } from "@/components/app/panel";
import { StatusPill } from "@/components/app/status-badge";
import type { Job } from "@/gen/syncr/v1/api_pb";
import { HookStatus, type HookRun } from "@/gen/syncr/v1/link_pb";
import { durationSeconds, formatDuration } from "@/lib/format";
import { isTerminal } from "@/lib/status";
import { cn } from "@/lib/utils";

import { hookStatusMeta, jobHookGroups, type HookRow } from "./hooks";

/** hookDuration renders how long a hook ran: "0.4s" below ten seconds, else as other durations. */
export function hookDuration(run: HookRun): string {
  const s = durationSeconds(run.duration);
  if (s <= 0) return "";
  return s < 10 ? `${s.toFixed(1)}s` : formatDuration(s);
}

function Row({ row, jobDone }: { row: HookRow; jobDone: boolean }) {
  const { run } = row;
  const failed = !!run && run.status !== HookStatus.SUCCEEDED;
  // What a hook that did not succeed printed is why one looks at it.
  const [open, setOpen] = useState(failed);
  const hasOutput = !!run && (run.output !== "" || run.outputTruncated);
  const id = `hook-output-${run ? `${run.phase}-` : ""}${row.name}`;
  return (
    <li className="px-5 py-3">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5">
        {hasOutput ? (
          <button
            type="button"
            onClick={() => setOpen((o) => !o)}
            aria-expanded={open}
            aria-controls={id}
            className="-ml-1.5 inline-flex min-w-0 items-center gap-1 rounded px-1.5 py-0.5 font-mono text-[13px] font-medium hover:bg-accent"
          >
            <ChevronRight
              className={cn("size-3.5 shrink-0 transition-transform", open && "rotate-90")}
              aria-hidden
            />
            <span className="truncate">{row.name}</span>
            <span className="sr-only">: {open ? "hide" : "show"} what it printed</span>
          </button>
        ) : (
          <span className="min-w-0 truncate pl-[1.125rem] font-mono text-[13px] font-medium">
            {row.name}
          </span>
        )}
        {run ? (
          <StatusPill meta={hookStatusMeta(run.status)} />
        ) : (
          <span className="text-xs text-muted-foreground">
            {jobDone ? "Did not run" : "Has not run yet"}
          </span>
        )}
        {run && (
          <span className="tabular ml-auto text-xs text-muted-foreground">
            {[hookDuration(run), failed && run.exitCode >= 0 ? `exit code ${run.exitCode}` : ""]
              .filter(Boolean)
              .join(" · ")}
          </span>
        )}
      </div>
      {run && run.message !== "" && (
        <p className="mt-1 pl-[1.125rem] text-[13px] text-danger">{run.message}</p>
      )}
      {hasOutput && open && (
        <div id={id} className="mt-2 pl-[1.125rem]">
          {run.outputTruncated && (
            <p className="mb-1 text-xs text-muted-foreground">
              Only the end of what it printed is kept.
            </p>
          )}
          <pre className="max-h-64 overflow-auto rounded-md bg-muted px-3 py-2 font-mono text-[12px] leading-5 break-words whitespace-pre-wrap">
            {run.output || "(nothing)"}
          </pre>
        </div>
      )}
    </li>
  );
}

/**
 * HookRunsPanel lists a job's hooks by phase: how each run ended, how long
 * it took and what it printed, and the hooks the job names that did not
 * run. It renders nothing for a job without hooks.
 */
export function HookRunsPanel({ job }: { job: Job }) {
  const groups = jobHookGroups(job);
  if (groups.length === 0) return null;
  const done = isTerminal(job.status);
  return (
    <Panel
      title="Hooks"
      description={
        done
          ? "The worker's own commands this job ran around its operation, in the order they ran."
          : "The worker's own commands this job runs around its operation. How each went shows once the job has finished."
      }
      flush
    >
      <div className="flex flex-col divide-y">
        {groups.map((g) => (
          <section key={g.phase.key} aria-label={`Hooks: ${g.phase.label.toLowerCase()}`}>
            <h3 className="bg-muted/40 px-5 py-1.5 text-xs font-medium text-muted-foreground">
              {g.phase.label}
            </h3>
            <ul className="flex flex-col divide-y">
              {g.rows.map((row) => (
                <Row key={row.name} row={row} jobDone={done} />
              ))}
            </ul>
          </section>
        ))}
      </div>
    </Panel>
  );
}
