import type { MessageInitShape } from "@bufbuild/protobuf";
import { useMutation } from "@connectrpc/connect-query";
import { CircleCheck, CircleX, PlugZap } from "lucide-react";
import { useState } from "react";

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import type { Worker } from "@/gen/syncr/v1/api_pb";
import { ProbeService, type TestRemoteResponse } from "@/gen/syncr/v1/probe_pb";
import type { RemoteInputSchema } from "@/gen/syncr/v1/remotes_pb";
import { errorMessage, fieldViolations } from "@/lib/errors";
import { durationSeconds, formatBytes, toNumber } from "@/lib/format";

import { UsageBar } from "./usage-bar";
import { WorkerPicker } from "./worker-picker";

/** TestRemoteButtonProps configures TestRemoteButton. */
export interface TestRemoteButtonProps {
  /** remoteId is the stored remote; empty for one that is not saved yet. */
  remoteId?: string;
  /**
   * input returns the form's unsaved values. With a remoteId they are
   * tested on top of the stored secrets; without one they are tested alone.
   */
  input?: () => MessageInitShape<typeof RemoteInputSchema> | undefined;
  /** workers lets the user choose who tests (needed for local folders). */
  workers?: Worker[];
  /** needsWorker asks for a worker from the start (a remote on a worker's disk). */
  needsWorker?: boolean;
  label?: string;
}

const codeLabels: Record<string, string> = {
  no_worker: "No worker available",
  unavailable: "Worker unavailable",
};

function latency(res: TestRemoteResponse): string {
  const s = durationSeconds(res.latency);
  if (!s) return "";
  return s < 1 ? ` in ${Math.round(s * 1000)} ms` : ` in ${s.toFixed(1)} s`;
}

/**
 * TestRemoteButton asks a worker to list the remote and report its space,
 * then shows the outcome inline: what it saw, or why it failed. Secrets
 * typed in the form are sent for the test and never shown back.
 */
export function TestRemoteButton({
  remoteId = "",
  input,
  workers,
  needsWorker = false,
  label = "Test remote",
}: TestRemoteButtonProps) {
  const test = useMutation(ProbeService.method.testRemote);
  const [workerId, setWorkerId] = useState("");
  const [askWorker, setAskWorker] = useState(needsWorker);
  const [result, setResult] = useState<TestRemoteResponse | undefined>();
  const [error, setError] = useState<string | undefined>();

  const run = async () => {
    setResult(undefined);
    setError(undefined);
    try {
      const res = await test.mutateAsync({ remoteId, input: input?.(), workerId, path: "" });
      setResult(res);
    } catch (err) {
      const violations = fieldViolations(err);
      if (violations.some((v) => v.field === "worker_id")) setAskWorker(true);
      setError(
        violations.length
          ? violations
              .map((v) => (v.field === "worker_id" ? v.message : `${v.field}: ${v.message}`))
              .join(" ")
          : errorMessage(err),
      );
    }
  };

  const usage = result?.usage;
  const used = toNumber(usage?.used);
  const total = toNumber(usage?.total);
  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-2">
        <Button
          type="button"
          variant="outline"
          disabled={test.isPending}
          onClick={() => void run()}
        >
          <PlugZap aria-hidden />
          {test.isPending ? "Testing…" : label}
        </Button>
        {workers && askWorker && (
          <div className="w-full sm:w-64">
            <WorkerPicker
              workers={workers}
              value={workerId}
              onChange={setWorkerId}
              className="bg-card"
            />
          </div>
        )}
        {!result && !error && !test.isPending && (
          <span className="text-xs text-muted-foreground">
            A worker lists the remote with the values above. Nothing is saved.
          </span>
        )}
      </div>
      {result?.ok && (
        <Alert data-testid="test-remote-ok">
          <CircleCheck className="text-success" aria-hidden />
          <AlertTitle>
            The remote answered{result.worker?.name ? ` through ${result.worker.name}` : ""}
            {latency(result)}
          </AlertTitle>
          <AlertDescription>
            <p>
              {result.entries === 0
                ? "It is empty at the top."
                : `${result.truncated ? "More than " : ""}${result.entries.toLocaleString("en-US")} ${result.entries === 1 ? "item" : "items"} at the top`}
              {result.sample.length > 0 && (
                <>
                  :{" "}
                  <span className="font-mono text-[12.5px]">
                    {result.sample.slice(0, 5).join(", ")}
                    {result.entries > Math.min(5, result.sample.length) ? ", …" : ""}
                  </span>
                </>
              )}
            </p>
            {usage &&
              !usage.unsupported &&
              usage.total !== undefined &&
              usage.used !== undefined && (
                <UsageBar used={used} total={total} className="mt-2 w-full max-w-sm" />
              )}
            {usage &&
              !usage.unsupported &&
              usage.total === undefined &&
              usage.used !== undefined && <p className="mt-1">Space used: {formatBytes(used)}</p>}
            {usage?.unsupported && (
              <p className="mt-1 text-muted-foreground">
                This kind of storage does not report how much space is used.
              </p>
            )}
          </AlertDescription>
        </Alert>
      )}
      {result && !result.ok && (
        <Alert variant="destructive" data-testid="test-remote-failed">
          <CircleX aria-hidden />
          <AlertTitle>
            The test failed
            {result.errorCode ? `: ${codeLabels[result.errorCode] ?? result.errorCode}` : ""}
          </AlertTitle>
          <AlertDescription>
            <p>{result.error || "The worker could not reach the remote."}</p>
            {result.worker?.name && <p>Tested through {result.worker.name}.</p>}
          </AlertDescription>
        </Alert>
      )}
      {error && (
        <Alert variant="destructive" data-testid="test-remote-error">
          <CircleX aria-hidden />
          <AlertTitle>The test could not run</AlertTitle>
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
    </div>
  );
}
