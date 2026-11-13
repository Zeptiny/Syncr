import { createConnectQueryKey } from "@connectrpc/connect-query";
import { QueryClient, type QueryKey } from "@tanstack/react-query";
import { describe, expect, it } from "vitest";

import { ProbeService } from "@/gen/syncr/v1/probe_pb";

import { invalidatePlaces } from "./refresh";

// The listings' keys as the browser's queries make them.
const worker = (input: Record<string, string>): QueryKey =>
  createConnectQueryKey({
    schema: ProbeService.method.listWorkerFiles,
    input,
    cardinality: "finite",
  });
const remote = (input: Record<string, string>): QueryKey =>
  createConnectQueryKey({
    schema: ProbeService.method.listRemoteFiles,
    input,
    cardinality: "finite",
  });

describe("invalidatePlaces", () => {
  // A worker's disk is reached as its folders and as local remotes it
  // lists: a job that wrote one refreshes the other.
  it("refreshes a worker's folders and the local remotes listed through it together", async () => {
    const client = new QueryClient();
    const folders = worker({ workerId: "nas", path: "/srv" });
    const local = remote({
      remoteId: "photos",
      workerId: "nas",
      path: "2026",
    });
    const other = remote({ remoteId: "b2", path: "" });
    const elsewhere = worker({ workerId: "backup", path: "/" });
    const all = [folders, local, other, elsewhere];
    const stale = () => all.map((k) => client.getQueryState(k)?.isInvalidated);
    const reset = () => all.forEach((k) => client.setQueryData(k, {}));

    reset();
    await invalidatePlaces(client, [{ kind: "worker", workerId: "nas" }]);
    expect(stale()).toEqual([true, true, false, false]);

    reset();
    await invalidatePlaces(client, [
      { kind: "remote", remoteId: "photos", local: true, workerId: "nas" },
    ]);
    expect(stale()).toEqual([true, true, false, false]);

    reset();
    await invalidatePlaces(client, [{ kind: "remote", remoteId: "b2" }]);
    expect(stale()).toEqual([false, false, true, false]);
  });
});
