import { Code } from "@connectrpc/connect";
import { useQuery } from "@connectrpc/connect-query";
import { Link } from "@tanstack/react-router";
import { Check, LoaderCircle, TriangleAlert } from "lucide-react";
import { useEffect, useEffectEvent } from "react";

import { KeyValueList } from "@/components/app/key-value-list";
import { RelativeTime } from "@/components/app/relative-time";
import { Button } from "@/components/ui/button";
import {
  EnrollmentTokenState,
  WorkerAdminService,
  type EnrollmentToken,
  type Worker,
} from "@/gen/syncr/v1/api_pb";
import { errorMessage, hasCode } from "@/lib/errors";
import { tsDate } from "@/lib/format";
import { cn } from "@/lib/utils";

import { platformLabel } from "./workers-page";

/** ENROLLMENT_POLL_MS is how often the page asks while it waits. */
export const ENROLLMENT_POLL_MS = 3000;

/**
 * EnrollmentStage is how far an enrollment has come: the command has not
 * run yet, the worker enrolled but has not connected, it connected (its
 * first Hello is stored), or the token expired unused.
 */
export type EnrollmentStage = "waiting" | "enrolled" | "connected" | "expired";

/**
 * enrollmentStage derives the stage from the token and the worker it
 * enrolled. A token the controller no longer knows (gone) expired: it
 * keeps tokens for a day after they expire. A worker counts as connected
 * only once it reported what it is (Worker.info): any link call marks it
 * seen, which may come before its Hello is stored.
 */
export function enrollmentStage(
  token: EnrollmentToken | undefined,
  worker: Worker | undefined,
  gone: boolean,
): EnrollmentStage {
  if (gone || token?.state === EnrollmentTokenState.EXPIRED) return "expired";
  if (token?.state !== EnrollmentTokenState.USED) return "waiting";
  return worker?.info ? "connected" : "enrolled";
}

/** EnrollmentProgress is what useEnrollmentProgress reports. */
export interface EnrollmentProgress {
  stage: EnrollmentStage;
  token?: EnrollmentToken;
  /** The enrolled worker, once the token is used and it was fetched. */
  worker?: Worker;
  /** A failure other than the token being gone; polling goes on. */
  error?: unknown;
}

/**
 * useEnrollmentProgress follows the enrollment that the token tokenId
 * starts: it asks for the token every pollMs while it waits to be used,
 * then for the worker until it has connected. It stops once the worker
 * has connected or the token expired, and when the component unmounts
 * (and pauses while the browser tab is hidden). onConnected runs once the
 * worker has connected.
 */
export function useEnrollmentProgress(
  tokenId: string,
  {
    pollMs = ENROLLMENT_POLL_MS,
    onConnected,
  }: { pollMs?: number; onConnected?: (worker: Worker) => void } = {},
): EnrollmentProgress {
  const token = useQuery(
    WorkerAdminService.method.getEnrollmentToken,
    { tokenId },
    {
      refetchInterval: (q) => {
        if (hasCode(q.state.error, Code.NotFound)) return false;
        const state = q.state.data?.token?.state;
        return state === EnrollmentTokenState.USED || state === EnrollmentTokenState.EXPIRED
          ? false
          : pollMs;
      },
    },
  );
  const workerId = token.data?.token?.workerId ?? "";
  const worker = useQuery(
    WorkerAdminService.method.getWorker,
    { workerId },
    {
      enabled: workerId !== "",
      refetchInterval: (q) => (q.state.data?.worker?.info ? false : pollMs),
    },
  );
  const gone = hasCode(token.error, Code.NotFound);
  const stage = enrollmentStage(token.data?.token, worker.data?.worker, gone);
  const connected = stage === "connected" ? worker.data?.worker : undefined;

  // Once per connected worker, not on every poll.
  const reportConnected = useEffectEvent(() => {
    if (connected) onConnected?.(connected);
  });
  const connectedId = connected?.id;
  useEffect(() => {
    if (connectedId) reportConnected();
  }, [connectedId]);

  return {
    stage,
    token: token.data?.token,
    worker: worker.data?.worker,
    error: (gone ? undefined : token.error) ?? worker.error ?? undefined,
  };
}

const liveSummary: Record<EnrollmentStage, (name: string) => string> = {
  waiting: () => "Waiting for the command to run.",
  enrolled: (name) => `${name} enrolled. Waiting for it to connect.`,
  connected: (name) => `${name} is connected.`,
  expired: () => "The command expired before a worker used it.",
};

/**
 * EnrollmentProgressView shows how far an enrollment has come: the command
 * running, the worker enrolling, then connecting, with what it reported
 * and a link to its page. workerLink sets how prominent that link is (the
 * setup wizard has its own main button).
 */
export function EnrollmentProgressView({
  progress,
  expiresAt,
  workerLink = "primary",
}: {
  progress: EnrollmentProgress;
  /** When the token expires, from CreateEnrollmentToken. */
  expiresAt: Date | undefined;
  workerLink?: "primary" | "secondary";
}) {
  const { stage, token, worker, error } = progress;
  const name = worker?.name ?? "The worker";

  if (stage === "expired") {
    return (
      <div className="flex items-start gap-2.5 rounded-lg bg-warning-soft px-4 py-3 text-[13px] text-warning">
        <TriangleAlert className="mt-0.5 size-4 shrink-0" aria-hidden />
        <p role="status">
          <strong className="font-semibold">This command expired before a worker used it.</strong>{" "}
          Generate a new command to add a worker.
        </p>
      </div>
    );
  }

  const step = stage === "waiting" ? 1 : stage === "enrolled" ? 3 : 4;
  const marker = (n: number) => (n < step ? "done" : n === step ? "active" : "todo");

  return (
    <section
      aria-label="Adding the worker"
      className="flex flex-col gap-5 rounded-lg border bg-card p-4"
    >
      <p role="status" className="sr-only">
        {liveSummary[stage](name)}
      </p>
      <ol className="flex flex-col gap-4">
        <ProgressStep n={1} state={marker(1)} title="Run the command on the worker's machine">
          {stage === "waiting" ? (
            <>
              Waiting for it to run. The command expires{" "}
              <RelativeTime date={tsDate(token?.expiresAt) ?? expiresAt} />.
            </>
          ) : (
            <>
              Ran <RelativeTime date={tsDate(token?.usedAt)} />.
            </>
          )}
        </ProgressStep>
        <ProgressStep n={2} state={marker(2)} title="Enrolled">
          {stage === "waiting" ? (
            "The worker registers with this controller."
          ) : (
            <>
              As <strong className="font-semibold text-foreground">{name}</strong>.
            </>
          )}
        </ProgressStep>
        <ProgressStep n={3} state={marker(3)} title="Connected">
          {stage === "connected" ? (
            <>
              First report <RelativeTime date={tsDate(worker?.info?.reportedAt)} />.
            </>
          ) : stage === "enrolled" ? (
            "Waiting for it to connect. Setup starts the worker right after it enrolls."
          ) : (
            "The worker reports its versions and the folders it may use."
          )}
        </ProgressStep>
      </ol>
      {error !== undefined && (
        <p className="text-[13px] text-danger">{errorMessage(error)} Still trying.</p>
      )}
      {stage === "connected" && worker && (
        <ConnectedWorker worker={worker} workerLink={workerLink} />
      )}
    </section>
  );
}

type StepState = "done" | "active" | "todo";

/** ProgressStep is one step of EnrollmentProgressView's list. */
function ProgressStep({
  n,
  state,
  title,
  children,
}: {
  n: number;
  state: StepState;
  title: string;
  children: React.ReactNode;
}) {
  return (
    <li
      className="grid grid-cols-[2rem_minmax(0,1fr)] items-start gap-3"
      aria-current={state === "active" ? "step" : undefined}
    >
      <span
        className={cn(
          "grid size-8 place-items-center rounded-full text-[13px] font-semibold",
          state === "done"
            ? "bg-primary text-primary-foreground"
            : "border bg-card text-foreground",
          state === "todo" && "text-muted-foreground",
        )}
      >
        {state === "done" ? (
          <Check className="size-4" aria-label="Done" />
        ) : state === "active" ? (
          <LoaderCircle
            className="size-4 animate-spin text-primary motion-reduce:animate-none"
            aria-label="In progress"
          />
        ) : (
          <span aria-hidden>{n}</span>
        )}
      </span>
      <div className="min-w-0 pt-1">
        <p className={cn("text-[14px] font-medium", state === "todo" && "text-muted-foreground")}>
          {title}
        </p>
        <p className="mt-0.5 text-[13px] text-muted-foreground">{children}</p>
      </div>
    </li>
  );
}

/** ConnectedWorker is what a newly connected worker reported. */
function ConnectedWorker({
  worker,
  workerLink,
}: {
  worker: Worker;
  workerLink: "primary" | "secondary";
}) {
  const info = worker.info;
  const roots = info?.localRoots ?? [];
  return (
    <div className="flex flex-col gap-4 border-t pt-4">
      <KeyValueList
        items={[
          { label: "Hostname", value: info?.hostname || "–" },
          { label: "Platform", value: platformLabel(worker), mono: true },
          { label: "Worker version", value: info?.workerVersion || "–" },
          {
            label: "Tools",
            value: info?.tools.length
              ? info.tools.map((t) => `${t.name} ${t.version}`).join(", ")
              : "–",
          },
          {
            label: "Allowed folders",
            value: roots.length ? (
              <ul className="flex flex-col gap-1">
                {roots.map((r) => (
                  <li key={r} className="font-mono text-[13px]">
                    {r}
                  </li>
                ))}
              </ul>
            ) : (
              <span className="flex w-fit items-start gap-1.5 rounded-md bg-warning-soft px-2 py-1 text-[13px] text-warning">
                <TriangleAlert className="mt-[3px] size-3.5 shrink-0" aria-hidden />
                <span>
                  None, so jobs on this worker cannot read or write its own disks, only move files
                  between remotes. To allow folders, add them to <code>local_roots</code> in the
                  worker&apos;s configuration and restart it.
                </span>
              </span>
            ),
          },
        ]}
      />
      <div>
        <Button variant={workerLink === "primary" ? "default" : "outline"} asChild>
          <Link to="/workers/$workerId" params={{ workerId: worker.id }}>
            Open {worker.name}
          </Link>
        </Button>
      </div>
    </div>
  );
}
