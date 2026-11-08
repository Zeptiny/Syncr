import { WorkerStatus, type Worker } from "@/gen/syncr/v1/api_pb";

const order: Record<WorkerStatus, number> = {
  [WorkerStatus.ONLINE]: 0,
  [WorkerStatus.OFFLINE]: 1,
  [WorkerStatus.PENDING]: 2,
  [WorkerStatus.UNSPECIFIED]: 3,
};

/** sortWorkers puts online workers first, then by name. */
export function sortWorkers(workers: Worker[]): Worker[] {
  return [...workers].sort(
    (a, b) => order[a.status] - order[b.status] || a.name.localeCompare(b.name),
  );
}
