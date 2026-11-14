import { describe, expect, it } from "vitest";

import { EnrollmentTokenState, WorkerStatus, type Worker } from "@/gen/syncr/v1/api_pb";
import * as m5 from "@/mock/m5";

import { CONNECT_AFTER_MS, ENROLL_AFTER_MS, Enrollments } from "./enrollment";

describe("mock enrollments", () => {
  const t0 = Date.UTC(2026, 10, 13, 12);

  it("enrolls a worker after a while, which then connects", () => {
    const workers: Worker[] = m5.workers(t0);
    const before = workers.length;
    const e = new Enrollments(workers);
    const { id } = e.create(3_600_000, t0);

    expect(e.get(id, t0 + 1000).state).toBe(EnrollmentTokenState.WAITING);
    expect(workers).toHaveLength(before);

    const used = e.get(id, t0 + ENROLL_AFTER_MS);
    expect(used.state).toBe(EnrollmentTokenState.USED);
    const w = workers.find((x) => x.id === used.workerId)!;
    expect(w).toMatchObject({ name: "studio-pc", status: WorkerStatus.PENDING });
    expect(w.info).toBeUndefined();
    expect(workers).toHaveLength(before + 1);

    // The page stops asking for a used token and asks for the worker:
    // that is enough to connect it.
    e.advance(t0 + ENROLL_AFTER_MS + CONNECT_AFTER_MS - 1);
    expect(w.info).toBeUndefined();
    e.advance(t0 + ENROLL_AFTER_MS + CONNECT_AFTER_MS);
    expect(w.status).toBe(WorkerStatus.ONLINE);
    expect(w.info).toMatchObject({
      hostname: "studio-pc",
      os: "linux",
      localRoots: ["/srv/media", "/home/ada/Projects"],
    });
    expect(w.info?.tools.length).toBeGreaterThan(0);
    // Asking again changes nothing.
    expect(e.get(id, t0 + 60_000).workerId).toBe(w.id);
    expect(workers).toHaveLength(before + 1);
  });

  it("expires a token nobody used, and does not know other IDs", () => {
    const e = new Enrollments(m5.workers(t0));
    const { id } = e.create(ENROLL_AFTER_MS - 1000, t0);
    expect(e.get(id, t0 + ENROLL_AFTER_MS).state).toBe(EnrollmentTokenState.EXPIRED);
    expect(() => e.get("nope", t0)).toThrow("enrollment token not found");
  });
});
