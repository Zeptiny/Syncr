import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import {
  Code,
  ConnectError,
  createRouterTransport,
  type Interceptor,
  type Transport,
} from "@connectrpc/connect";

import {
  JobSchema,
  JobService,
  JobStatus,
  ValidationErrorSchema,
  WorkerAdminService,
  type Job,
} from "@/gen/syncr/v1/api_pb";
import type { JobErrorCode } from "@/gen/syncr/v1/link_pb";
import { ContactService } from "@/gen/syncr/v1/notify_pb";
import { BackendService, RemoteService } from "@/gen/syncr/v1/remotes_pb";
import { ResticService } from "@/gen/syncr/v1/restic_pb";
import { ScheduleSchema, ScheduleService, type Schedule } from "@/gen/syncr/v1/schedules_pb";
import { UserService } from "@/gen/syncr/v1/users_pb";
import { csrfHeader } from "@/lib/transport";
import * as bisyncMock from "@/mock/bisync";
import * as m5 from "@/mock/m5";

import { signedIn } from "./app-api";
import * as fx from "./fixtures";

/** BisyncApi is a mock API over the two-way sync demo, for tests. */
export interface BisyncApi {
  transport: Transport;
  /** of(method) lists the requests of one method, in order. */
  of: <T = unknown>(method: string) => T[];
  jobs: Job[];
  schedule: Schedule;
  /** job finds the demo job that failed with the given error code, newest first. */
  failed: (code: JobErrorCode) => Job;
}

/**
 * bisyncApi serves the two-way sync demo of the mock (the R2 remote, the
 * schedule "Shared folder ⇄ R2" with its runs and jobs, workers that run
 * two-way syncs) with the services its pages need, answers at once and
 * records every request. RunJob and the schedule methods check two-way
 * syncs like the controller, unless reject says what they refuse.
 */
export function bisyncApi(
  now: number,
  user: "admin" | "alice" = "admin",
  /** reject, when given, is what CreateSchedule and RunJob refuse every request with. */
  reject?: { field: string; message: string }[],
): BisyncApi {
  const h = bisyncMock.history(now);
  const workers = m5.workers(now);
  bisyncMock.applyWorkers(workers);
  const remotes = [...fx.remotes(now), bisyncMock.r2Remote(now)];
  const jobs = [...h.jobs];
  const me = fx.users(now).find((u) => u.username === user)!;
  const calls: { method: string; message: unknown }[] = [];
  const invalid = (violations: { field: string; message: string }[]) =>
    new ConnectError("invalid argument", Code.InvalidArgument, undefined, [
      { desc: ValidationErrorSchema, value: { violations } },
    ]);
  const record: Interceptor = (next) => (req) => {
    calls.push({ method: req.method.name, message: req.stream ? undefined : req.message });
    return next(req);
  };
  const job = (id: string) => {
    const j = jobs.find((x) => x.id === id);
    if (!j) throw new ConnectError("job not found", Code.NotFound);
    return j;
  };
  const transport = createRouterTransport(
    (router) => {
      signedIn(router, now, user);
      router.service(UserService, { listUsers: () => ({ users: fx.users(now) }) });
      router.service(WorkerAdminService, { listWorkers: () => ({ workers }) });
      router.service(RemoteService, {
        listRemotes: () => ({ remotes: user === "admin" ? remotes : [] }),
      });
      router.service(BackendService, { listBackends: () => ({ backends: fx.backends }) });
      router.service(ResticService, { listRepositories: () => ({ repositories: [] }) });
      router.service(ContactService, { listContacts: () => ({ contacts: fx.contacts(now) }) });
      router.service(ScheduleService, {
        getSchedule: () => ({ schedule: h.schedule }),
        listScheduleRuns: () => ({ runs: h.runs.slice(0, 15) }),
        previewSchedule: () => ({ nextRuns: [timestampFromMs(now + 3_600_000)] }),
        createSchedule: (req) => {
          const t = req.input?.target?.target;
          if (reject) throw invalid(reject);
          const problems = bisyncMock.check(req.input?.definition, {
            forSchedule: true,
            label: t?.case === "workerLabel",
          });
          if (problems.length) throw invalid(problems);
          return { schedule: create(ScheduleSchema, { id: "s-new", name: req.input?.name }) };
        },
        updateSchedule: () => ({ schedule: h.schedule }),
      });
      router.service(JobService, {
        listJobs: () => ({ jobs }),
        getJob: (req) => ({ job: job(req.jobId) }),
        listJobSamples: () => ({ samples: [] }),
        runJob: (req) => {
          if (reject) throw invalid(reject);
          const problems = bisyncMock.check(req.definition, { forSchedule: false });
          if (problems.length) throw invalid(problems);
          const d = bisyncMock.describe(req.definition, remotes);
          const j = create(JobSchema, {
            id: `job-${jobs.length + 1}`,
            workerId: req.workerId,
            kind: d ? bisyncMock.KIND : "rclone_transfer",
            title: d?.title ?? "",
            remotes: d?.remotes ?? [],
            status: JobStatus.QUEUED,
            ownerId: me.id,
            createdAt: timestampFromMs(now),
            definition: req.definition,
          });
          jobs.unshift(j);
          return { job: j };
        },
        // One message, then the stream ends: enough for a status badge.
        async *watchJob(req) {
          await Promise.resolve();
          yield { update: { case: "job" as const, value: job(req.jobId) } };
        },
      });
    },
    { transport: { interceptors: [csrfHeader, record] } },
  );
  return {
    transport,
    of: <T>(method: string) => calls.filter((c) => c.method === method).map((c) => c.message as T),
    jobs,
    schedule: h.schedule,
    failed: (code) => {
      const j = h.jobs.find((x) => x.error?.code === code);
      if (!j) throw new Error(`no demo job failed with code ${code}`);
      return j;
    },
  };
}
