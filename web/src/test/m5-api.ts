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
import { BackendService, RemoteService } from "@/gen/syncr/v1/remotes_pb";
import { ContactService } from "@/gen/syncr/v1/notify_pb";
import { ScheduleSchema, ScheduleService } from "@/gen/syncr/v1/schedules_pb";
import { csrfHeader } from "@/lib/transport";
import * as hooksMock from "@/mock/hooks";
import * as m5 from "@/mock/m5";
import * as m6 from "@/mock/m6";
import { registerM7 } from "@/mock/m7";

import { signedIn } from "./app-api";
import * as fx from "./fixtures";

/** Call is one request the UI sent, as the mock API saw it. */
export interface Call {
  method: string;
  message: unknown;
}

/** M5Api is a mock API over the milestone 5 demo data, for tests. */
export interface M5Api {
  transport: Transport;
  /** calls lists every request in order; of(method) picks one method's messages. */
  calls: Call[];
  of: <T = unknown>(method: string) => T[];
  data: m5.M5Data;
  jobs: Job[];
  /** repo returns a demo repository's state by ID, to adjust it before rendering. */
  repo: (id: string) => m5.RepoState;
}

/**
 * m5Api serves the milestone 5 mock (repositories, probes) and the
 * milestone 6 methods (host keys, upgrade commands) with the services its
 * pages also need, answers at once and never starts timers, and records
 * every request. Its workers are m5's, with m6's outdated flags and the
 * hooks mock's hooks (nas-01 defines four), and its jobs include the hooks
 * mock's two backups.
 */
export function m5Api(now: number, user: "admin" | "alice" = "admin"): M5Api {
  const data = m5.data(now);
  const workers = m5.workers(now);
  m6.applyWorkers(workers);
  hooksMock.applyWorkers(workers);
  const jobs = [...data.jobs, ...hooksMock.jobs(now)];
  const schedules = [...data.schedules];
  const remotes = fx.remotes(now);
  const me = fx.users(now).find((u) => u.username === user)!;
  const calls: Call[] = [];
  const invalid = (violations: { field: string; message: string }[]) =>
    new ConnectError("invalid argument", Code.InvalidArgument, undefined, [
      { desc: ValidationErrorSchema, value: { violations } },
    ]);
  const api = m5.createM5({
    now,
    data,
    me: () => me,
    jobs,
    workers,
    schedules,
    remotes: () => remotes,
    invalid,
    sleep: () => Promise.resolve(),
    probeDelay: 0,
    timers: false,
  });
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
      api.register(router);
      router.service(WorkerAdminService, {
        listWorkers: () => ({ workers }),
        getWorker: (req) => ({ worker: workers.find((w) => w.id === req.workerId) }),
      });
      router.service(RemoteService, {
        listRemotes: () => ({ remotes: user === "admin" ? remotes : [] }),
        getRemote: (req) => ({ remote: remotes.find((r) => r.id === req.remoteId) }),
      });
      router.service(BackendService, { listBackends: () => ({ backends: fx.backends }) });
      router.service(ScheduleService, {
        listSchedules: (req) => ({
          schedules: schedules.filter(
            (s) => !req.repositoryId || s.repository?.id === req.repositoryId,
          ),
        }),
        getSchedule: (req) => ({ schedule: schedules.find((s) => s.id === req.scheduleId) }),
        listScheduleRuns: () => ({ runs: [] }),
        previewSchedule: () => ({ nextRuns: [timestampFromMs(now + 3_600_000)] }),
        createSchedule: (req) => {
          const workerId =
            req.input?.target?.target.case === "workerId" ? req.input.target.target.value : "";
          const problems = [
            ...api.check(req.input?.definition, true, workerId),
            ...hooksMock.check(req.input?.definition, workers, workerId),
          ];
          if (problems.length) throw invalid(problems);
          return { schedule: create(ScheduleSchema, { id: "s-new", name: req.input?.name }) };
        },
      });
      router.service(ContactService, { listContacts: () => ({ contacts: [] }) });
      router.service(JobService, {
        listJobs: (req) => ({
          jobs: jobs
            .filter((j) => !req.repositoryId || j.repository?.id === req.repositoryId)
            .filter((j) => !req.statuses.length || req.statuses.includes(j.status))
            .slice(0, req.pageSize || 50),
        }),
        getJob: (req) => ({ job: job(req.jobId) }),
        listJobSamples: () => ({ samples: [] }),
        cancelJob: (req) => {
          const j = job(req.jobId);
          if (j.status === JobStatus.QUEUED) j.status = JobStatus.CANCELLED;
          else j.cancelRequested = true;
          api.settle();
          return { job: j };
        },
        runJob: (req) => {
          const problems = [
            ...api.check(req.definition, false, req.workerId),
            ...hooksMock.check(req.definition, workers, req.workerId),
          ];
          if (problems.length) throw invalid(problems);
          const d = api.describe(req.definition);
          const j = create(JobSchema, {
            id: `job-${jobs.length + 1}`,
            workerId: req.workerId,
            kind: d?.kind ?? "rclone_transfer",
            title: d?.title ?? "",
            repository: d?.repository,
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
      // Milestone 6: host keys and upgrade commands (after WorkerAdminService).
      m6.registerM6(router, {
        me: () => me,
        workers,
        invalid,
        sleep: () => Promise.resolve(),
        probeDelay: 0,
      });
      // Milestone 7: the file browser's remote usage.
      registerM7(router, {
        me: () => me,
        workers,
        remotes: () => remotes,
        invalid,
        sleep: () => Promise.resolve(),
        probeDelay: 0,
      });
    },
    { transport: { interceptors: [csrfHeader, record] } },
  );
  return {
    transport,
    calls,
    of: <T>(method: string) => calls.filter((c) => c.method === method).map((c) => c.message as T),
    data,
    jobs,
    repo: (id) => data.repositories.find((r) => r.repo.id === id)!,
  };
}
