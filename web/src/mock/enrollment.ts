/**
 * Mock enrollments for the dev mock: a new token is used ENROLL_AFTER_MS
 * after it was created, as if someone ran the command, and its worker
 * connects CONNECT_AFTER_MS later, so the Add a worker page and the setup
 * wizard go through every stage.
 */
import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";

import {
  EnrollmentTokenSchema,
  EnrollmentTokenState,
  WorkerInfoSchema,
  WorkerSchema,
  WorkerStatus,
  type EnrollmentToken,
  type Worker,
} from "@/gen/syncr/v1/api_pb";
import { WorkerMode } from "@/gen/syncr/v1/link_pb";

/** ENROLL_AFTER_MS is when the mock "runs" a new command. */
export const ENROLL_AFTER_MS = 6_000;
/** CONNECT_AFTER_MS is how long the mock worker takes to connect. */
export const CONNECT_AFTER_MS = 6_000;

interface MockToken {
  id: string;
  createdAt: number;
  expiresAt: number;
  workerId?: string;
}

/** Enrollments tracks the mock's enrollment tokens. */
export class Enrollments {
  private readonly tokens = new Map<string, MockToken>();
  private serial = 0;

  /** workers is the mock's worker list, which enrollments add to. */
  constructor(private readonly workers: Worker[]) {}

  /** create records a new token valid for ttlMs and returns its ID. */
  create(ttlMs: number, now = Date.now()): MockToken {
    const t = { id: crypto.randomUUID(), createdAt: now, expiresAt: now + ttlMs };
    this.tokens.set(t.id, t);
    return t;
  }

  /** get returns the token as of now, enrolling and connecting its worker on time. */
  get(id: string, now = Date.now()): EnrollmentToken {
    const t = this.tokens.get(id);
    if (!t) throw new ConnectError("enrollment token not found", Code.NotFound);
    const usedAt = t.createdAt + ENROLL_AFTER_MS;
    if (!t.workerId && now >= usedAt && usedAt < t.expiresAt) this.enroll(t, usedAt);
    const w = this.workers.find((x) => x.id === t.workerId);
    if (w && !w.info && now >= usedAt + CONNECT_AFTER_MS)
      this.connect(w, usedAt + CONNECT_AFTER_MS);
    const state = t.workerId
      ? EnrollmentTokenState.USED
      : now < t.expiresAt
        ? EnrollmentTokenState.WAITING
        : EnrollmentTokenState.EXPIRED;
    return create(EnrollmentTokenSchema, {
      id: t.id,
      state,
      createdAt: timestampFromMs(t.createdAt),
      expiresAt: timestampFromMs(t.expiresAt),
      usedAt: t.workerId ? timestampFromMs(usedAt) : undefined,
      workerId: t.workerId ?? "",
    });
  }

  private enroll(t: MockToken, at: number) {
    this.serial++;
    const name = this.serial === 1 ? "studio-pc" : `studio-pc-${this.serial}`;
    const w = create(WorkerSchema, {
      id: crypto.randomUUID(),
      name,
      mode: WorkerMode.ACTIVE,
      status: WorkerStatus.PENDING,
      createdAt: timestampFromMs(at),
      labels: [],
    });
    this.workers.push(w);
    this.workers.sort((a, b) => a.name.localeCompare(b.name));
    t.workerId = w.id;
  }

  private connect(w: Worker, at: number) {
    // Report like the mock's other Linux workers do.
    const like = this.workers.find((x) => x.info?.os === "linux" && x.info.features.length)?.info;
    w.status = WorkerStatus.ONLINE;
    w.lastSeen = timestampFromMs(Date.now());
    w.info = create(WorkerInfoSchema, {
      workerVersion: like?.workerVersion ?? "v2.1.0",
      os: "linux",
      arch: "amd64",
      hostname: w.name,
      tools: like?.tools.map((t) => ({ name: t.name, version: t.version })) ?? [],
      jobKinds: [...(like?.jobKinds ?? [])],
      maxConcurrentJobs: like?.maxConcurrentJobs ?? 2,
      localRoots: ["/srv/media", "/home/ada/Projects"],
      features: [...(like?.features ?? [])],
      probeOps: [...(like?.probeOps ?? [])],
      reportedAt: timestampFromMs(at),
    });
  }
}
