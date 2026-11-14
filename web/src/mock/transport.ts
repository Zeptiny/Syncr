/**
 * Dev-only mock of the controller API (VITE_SYNCR_MOCK=1), so the UI can be
 * explored and screenshotted before the backend is wired. It serves the
 * test fixtures from memory and simulates one live transfer. main.tsx only
 * imports it behind `import.meta.env.DEV`, so production builds drop it.
 *
 * Sign in with admin / syncr-demo-admin or alice / syncr-demo-alice. Set
 * localStorage "syncr.mock.setup" to "1" to start in first-run setup.
 */
import { create } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromMs, timestampMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createRouterTransport, type Transport } from "@connectrpc/connect";

import {
  JobProgressSchema,
  JobSchema,
  JobService,
  JobStatus,
  ValidationErrorSchema,
  WorkerAdminService,
  WorkerSchema,
  WorkerStatus,
  type Job,
  type JobDefinition,
} from "@/gen/syncr/v1/api_pb";
import { APITokenSchema, AuthMethod, AuthService, SetupService } from "@/gen/syncr/v1/auth_pb";
import { WorkerMode } from "@/gen/syncr/v1/link_pb";
import {
  BackendService,
  RemoteSchema,
  RemoteService,
  type Remote,
} from "@/gen/syncr/v1/remotes_pb";
import { UserRole, UserSchema, UserService } from "@/gen/syncr/v1/users_pb";
import * as fx from "@/test/fixtures";

import * as bisyncMock from "./bisync";
import { Enrollments } from "./enrollment";
import * as hooksMock from "./hooks";
import { registerM4 } from "./m4";
import * as m5mock from "./m5";
import * as m6 from "./m6";
import { applyTransfer, applyWorkers as applyM7Workers, registerM7 } from "./m7";

const passwords: Record<string, string> = { admin: "syncr-demo-admin", alice: "syncr-demo-alice" };
const SESSION_KEY = "syncr.mock.session";
const SETUP_KEY = "syncr.mock.setup";

function store(key: string): string | null {
  try {
    return window.sessionStorage.getItem(key) ?? window.localStorage.getItem(key);
  } catch {
    return null;
  }
}

function invalid(violations: { field: string; message: string }[]): ConnectError {
  return new ConnectError("invalid argument", Code.InvalidArgument, undefined, [
    { desc: ValidationErrorSchema, value: { violations } },
  ]);
}

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const id = setTimeout(resolve, ms);
    signal?.addEventListener("abort", () => {
      clearTimeout(id);
      resolve();
    });
  });
}

function uuid(): string {
  return crypto.randomUUID();
}

/** createMockTransport returns a Transport that serves fixture data. */
export function createMockTransport(): Transport {
  const now = Date.now();
  const users = fx.users(now);
  const workers = m5mock.workers(now);
  m6.applyWorkers(workers);
  hooksMock.applyWorkers(workers);
  bisyncMock.applyWorkers(workers);
  applyM7Workers(workers);
  workers.push(m6.atticWorker(now));
  const enrollments = new Enrollments(workers);
  let remotes = [...fx.remotes(now), m6.verifiedRemote(now), bisyncMock.r2Remote(now)].sort(
    (a, b) => a.name.localeCompare(b.name),
  );
  const hist = fx.history(now);
  const m5data = m5mock.data(now);
  hist.schedules.push(...m5data.schedules);
  const twoWay = bisyncMock.history(now);
  hist.schedules.push(twoWay.schedule);
  hist.runs.push(...twoWay.runs);
  hist.runs.sort((a, b) =>
    Number((b.scheduledFor?.seconds ?? 0n) - (a.scheduledFor?.seconds ?? 0n)),
  );
  const limited = m6.timeLimitJob(now);
  const jobs = [
    ...fx.jobs(now),
    ...hist.jobs,
    ...m5data.jobs,
    limited.job,
    ...hooksMock.jobs(now),
    ...twoWay.jobs,
  ].sort(
    (a, b) =>
      timestampMs(b.createdAt ?? timestampFromMs(0)) -
      timestampMs(a.createdAt ?? timestampFromMs(0)),
  );
  const tokens = fx.apiTokens(now);
  let setupRequired = store(SETUP_KEY) === "1";
  let sessionUser = store(SESSION_KEY) ?? undefined;
  const lastTick = new Map<string, number>(); // jobId → time of the last progress

  const running = jobs.find((j) => j.id === fx.ids.runningJob);
  const history = new Map<string, ReturnType<typeof fx.samples>>();
  history.set(limited.job.id, limited.samples);
  if (running?.stats) {
    history.set(running.id, fx.samples(now, 6 * 60_000, Number(running.stats.bytes)));
    lastTick.set(running.id, now);
  }

  const me = () => {
    const u = users.find((x) => x.id === sessionUser);
    if (!u) throw new ConnectError("not signed in", Code.Unauthenticated);
    return u;
  };
  const admin = () => {
    const u = me();
    if (u.role !== UserRole.ADMIN)
      throw new ConnectError("administrators only", Code.PermissionDenied);
    return u;
  };
  const setSession = (id: string | undefined) => {
    sessionUser = id;
    try {
      if (id) window.sessionStorage.setItem(SESSION_KEY, id);
      else window.sessionStorage.removeItem(SESSION_KEY);
    } catch {
      // Storage is optional for the mock.
    }
  };
  const job = (id: string) => {
    const u = me();
    const j = jobs.find((x) => x.id === id);
    if (!j || (j.ownerId !== u.id && u.role !== UserRole.ADMIN))
      throw new ConnectError("job not found", Code.NotFound);
    return j;
  };
  const remote = (id: string) => {
    const r = remotes.find((x) => x.id === id);
    if (!r || me().id !== fx.ids.admin) throw new ConnectError("remote not found", Code.NotFound);
    return r;
  };

  const m5 = m5mock.createM5({
    now,
    data: m5data,
    me,
    jobs,
    workers,
    schedules: hist.schedules,
    remotes: () => remotes,
    invalid,
    sleep: (ms) => sleep(ms),
  });

  // Advance a running job by about one second of transfer.
  const tick = (j: Job) => {
    if (j.status !== JobStatus.RUNNING || !j.stats) return;
    const t = Date.now() / 1000;
    const rate = 54 * 1024 ** 2 * (1 + 0.3 * Math.sin(t / 7) + 0.15 * Math.sin(t / 2.3));
    // Cover the whole time since the last tick, so the rate stays even.
    const since = Math.max(0.5, (Date.now() - (lastTick.get(j.id) ?? Date.now() - 1000)) / 1000);
    lastTick.set(j.id, Date.now());
    const bytes = Number(j.stats.bytes) + rate * since;
    const total = Number(j.stats.totalBytes);
    j.stats.bytes = BigInt(Math.min(total, Math.round(bytes)));
    j.stats.files = BigInt(
      Math.round((Number(j.stats.bytes) / Math.max(1, total)) * Number(j.stats.totalFiles)),
    );
    j.stats.elapsed = durationFromMs(Date.now() - timestampMs(j.startedAt ?? timestampFromMs(now)));
    if (bytes >= total) {
      j.status = JobStatus.SUCCEEDED;
      j.finishedAt = timestampFromMs(Date.now());
      bisyncMock.finish(j);
      applyTransfer(m5.files, j.workerId, j.definition);
      m5.settle();
    }
  };

  // queueJob adds a queued job, which the mock worker picks up after a moment.
  const queueJob = (p: {
    workerId: string;
    definition: JobDefinition | undefined;
    ownerId: string;
    scheduleId?: string;
    scheduleName?: string;
  }): Job => {
    const op = p.definition?.operation;
    const restic = m5.describe(p.definition);
    const twoWay = bisyncMock.describe(p.definition, remotes);
    const refs = restic
      ? restic.remotes
      : twoWay
        ? twoWay.remotes
        : op?.case === "rcloneTransfer"
          ? [op.value.source?.remoteId, op.value.destination?.remoteId]
              .filter((id): id is string => !!id)
              .map((id) => {
                const r = remotes.find((x) => x.id === id);
                return { id, name: r?.name ?? "?", backendType: r?.backendType ?? "" };
              })
          : [];
    const j = create(JobSchema, {
      id: uuid(),
      workerId: p.workerId,
      kind: restic?.kind ?? (twoWay ? bisyncMock.KIND : "rclone_transfer"),
      title: restic?.title ?? twoWay?.title ?? "",
      repository: restic?.repository,
      status: JobStatus.QUEUED,
      ownerId: p.ownerId,
      createdAt: timestampFromMs(Date.now()),
      definition: p.definition,
      remotes: refs,
      scheduleId: p.scheduleId ?? "",
      schedule: p.scheduleId ? { id: p.scheduleId, name: p.scheduleName ?? "" } : undefined,
    });
    jobs.unshift(j);
    if (restic) {
      // Restic jobs wait for older jobs on their repository, then finish.
      m5.start(j);
      return j;
    }
    // A preview or a copy of a few selected items (the file browser) is quick.
    const small =
      op?.case === "rcloneTransfer" &&
      (op.value.options?.dryRun || (op.value.options?.filters.length ?? 0) > 0);
    setTimeout(() => {
      if (j.status !== JobStatus.QUEUED) return;
      j.status = JobStatus.RUNNING;
      j.dispatchedAt = timestampFromMs(Date.now());
      j.startedAt = timestampFromMs(Date.now());
      j.stats = {
        $typeName: "syncr.v1.JobStats",
        bytes: 0n,
        totalBytes: BigInt(Math.round((twoWay ? 1.2 : small ? 0.3 : 6) * 1024 ** 3)),
        files: 0n,
        totalFiles: 1500n,
        checks: 0n,
        deletes: 0n,
        renames: 0n,
        errors: 0n,
      };
    }, 1500);
    return j;
  };

  return createRouterTransport((router) => {
    const { service } = router;
    m5.register(router);
    registerM4(router, {
      m5,
      now,
      history: hist,
      me,
      jobs,
      workers,
      remotes: () => remotes,
      invalid,
      sleep: (ms) => sleep(ms),
      queueJob,
    });
    service(SetupService, {
      getSetupStatus: () => ({ setupRequired }),
      completeSetup: async (req) => {
        await sleep(400);
        if (!setupRequired)
          throw new ConnectError("setup is already complete", Code.FailedPrecondition);
        if (req.setupCode.replaceAll("-", "").length !== 20) {
          throw invalid([
            {
              field: "setup_code",
              message: "That setup code is not valid. Copy it again from the log.",
            },
          ]);
        }
        const u = users[0];
        u.username = req.username;
        u.displayName = req.displayName;
        setupRequired = false;
        try {
          window.localStorage.removeItem(SETUP_KEY);
        } catch {
          // ignore
        }
        setSession(u.id);
        return { user: u };
      },
    });

    service(AuthService, {
      login: async (req) => {
        await sleep(300);
        const u = users.find((x) => x.username === req.username);
        if (!u || passwords[u.username] !== req.password || u.disabled) {
          throw new ConnectError("invalid username or password", Code.Unauthenticated);
        }
        setSession(u.id);
        u.lastLoginAt = timestampFromMs(Date.now());
        return { user: u };
      },
      logout: () => {
        setSession(undefined);
        return {};
      },
      getSession: () => ({
        user: me(),
        method: AuthMethod.SESSION,
        expiresAt: timestampFromMs(Date.now() + 30 * 86_400_000),
      }),
      changePassword: async (req) => {
        me();
        await sleep(300);
        if (req.newPassword.length < 12)
          throw invalid([{ field: "new_password", message: "Use at least 12 characters." }]);
        return {};
      },
      listAPITokens: () => {
        me();
        return { tokens };
      },
      createAPIToken: (req) => {
        me();
        const secret = `syncr_pat_${Array.from(crypto.getRandomValues(new Uint8Array(24)), (b) => (b % 36).toString(36)).join("")}`;
        const t = create(APITokenSchema, {
          id: uuid(),
          name: req.name,
          prefix: secret.slice(0, 14),
          createdAt: timestampFromMs(Date.now()),
          expiresAt: req.ttl
            ? timestampFromMs(Date.now() + Number(req.ttl.seconds) * 1000)
            : undefined,
        });
        tokens.unshift(t);
        return { token: t, secret };
      },
      deleteAPIToken: (req) => {
        me();
        const i = tokens.findIndex((t) => t.id === req.tokenId);
        if (i >= 0) tokens.splice(i, 1);
        return {};
      },
    });

    service(UserService, {
      listUsers: () => {
        admin();
        return { users };
      },
      getUser: (req) => {
        admin();
        return { user: users.find((u) => u.id === req.userId) };
      },
      createUser: (req) => {
        admin();
        if (users.some((u) => u.username === req.username)) {
          throw invalid([{ field: "username", message: "That username is taken." }]);
        }
        const u = create(UserSchema, {
          username: req.username,
          displayName: req.displayName,
          role: req.role,
          id: uuid(),
          createdAt: timestampFromMs(Date.now()),
          version: 1n,
        });
        users.push(u);
        return { user: u };
      },
      updateUser: (req) => {
        admin();
        const u = users.find((x) => x.id === req.userId);
        if (!u) throw new ConnectError("user not found", Code.NotFound);
        if (req.displayName !== undefined) u.displayName = req.displayName;
        if (req.role !== UserRole.UNSPECIFIED) u.role = req.role;
        if (req.disabled !== undefined) u.disabled = req.disabled;
        u.version++;
        return { user: u };
      },
      resetPassword: (req) => {
        admin();
        return { user: users.find((u) => u.id === req.userId) };
      },
      deleteUser: () => {
        admin();
        throw new ConnectError(
          "the user owns remotes or jobs; disable the account instead",
          Code.FailedPrecondition,
        );
      },
    });

    service(BackendService, {
      listBackends: () => {
        me();
        return { backends: fx.backends };
      },
    });

    service(RemoteService, {
      listRemotes: (req) => {
        const u = me();
        if (u.id !== fx.ids.admin) return { remotes: [] };
        return {
          remotes: remotes.filter((r) => !req.backendType || r.backendType === req.backendType),
        };
      },
      getRemote: (req) => ({ remote: remote(req.remoteId) }),
      createRemote: async (req) => {
        me();
        await sleep(300);
        const input = req.input;
        if (!input) throw invalid([{ field: "input", message: "missing" }]);
        if (remotes.some((r) => r.name.toLowerCase() === input.name.toLowerCase())) {
          throw invalid([{ field: "name", message: "You already have a remote with this name." }]);
        }
        if (
          input.backendType === "s3" &&
          input.options.provider !== "AWS" &&
          !input.options.endpoint
        ) {
          throw invalid([
            { field: "options.endpoint", message: "Required unless the provider is AWS." },
          ]);
        }
        const r: Remote = create(RemoteSchema, {
          id: uuid(),
          name: input.name,
          backendType: input.backendType,
          options: input.options,
          secretsSet: Object.keys(input.secrets),
          members: input.members,
          createdAt: timestampFromMs(Date.now()),
          updatedAt: timestampFromMs(Date.now()),
          version: 1n,
        });
        remotes = [...remotes, r].sort((a, b) => a.name.localeCompare(b.name));
        return { remote: r };
      },
      updateRemote: async (req) => {
        const r = remote(req.remoteId);
        await sleep(300);
        const input = req.input;
        if (!input) throw invalid([{ field: "input", message: "missing" }]);
        r.name = input.name;
        r.options = input.options;
        r.secretsSet = [
          ...new Set([
            ...r.secretsSet.filter((k) => !input.clearSecrets.includes(k)),
            ...Object.keys(input.secrets),
          ]),
        ];
        r.members = input.members;
        r.version++;
        r.updatedAt = timestampFromMs(Date.now());
        return { remote: r };
      },
      deleteRemote: (req) => {
        const r = remote(req.remoteId);
        if (r.usedByUnions.length) {
          throw new ConnectError(
            `remote is a member of ${r.usedByUnions.map((id) => remotes.find((x) => x.id === id)?.name ?? id).join(", ")}`,
            Code.FailedPrecondition,
          );
        }
        const held = m5data.repositories.filter((x) => !x.deleted && x.repo.remoteId === r.id);
        if (held.length) {
          throw new ConnectError(
            `the remote holds ${held.length === 1 ? "the repository" : "the repositories"} ${held.map((x) => x.repo.name).join(", ")}`,
            Code.FailedPrecondition,
          );
        }
        remotes = remotes.filter((x) => x.id !== r.id);
        return {};
      },
    });

    service(WorkerAdminService, {
      listWorkers: (req) => {
        me();
        enrollments.advance();
        return { workers: workers.filter((w) => !req.label || w.labels.includes(req.label)) };
      },
      setWorkerLabels: async (req) => {
        admin();
        await sleep(250);
        const w = workers.find((x) => x.id === req.workerId);
        if (!w) throw new ConnectError("worker not found", Code.NotFound);
        const labels = [...new Set(req.labels.map((l) => l.trim().toLowerCase()))].sort();
        const bad = labels.findIndex((l) => !/^[a-z0-9][a-z0-9_.-]{0,31}$/.test(l));
        if (bad >= 0)
          throw invalid([
            {
              field: `labels[${bad}]`,
              message: `"${labels[bad]}" is not a valid label: use up to 32 lower-case letters, digits, dots, dashes and underscores.`,
            },
          ]);
        if (labels.length > 16) throw invalid([{ field: "labels", message: "At most 16 labels." }]);
        w.labels = labels;
        return { worker: w };
      },
      getWorker: (req) => {
        me();
        enrollments.advance();
        const w = workers.find((x) => x.id === req.workerId);
        if (!w) throw new ConnectError("worker not found", Code.NotFound);
        return { worker: w };
      },
      createEnrollmentToken: async (req) => {
        admin();
        await sleep(300);
        const token = "syncr_et_7Hq2xV9pLmT4sR8wK3nB6cY1dF5gJ0aZ";
        const t = enrollments.create(Number(req.ttl?.seconds ?? 3600n) * 1000);
        return {
          token,
          tokenId: t.id,
          expiresAt: timestampFromMs(t.expiresAt),
          installCommands: fx.installCommands(
            token,
            req.controllerUrl || "https://syncr.example.net",
          ),
        };
      },
      getEnrollmentToken: (req) => {
        admin();
        return { token: enrollments.get(req.tokenId) };
      },
      getPassiveInstallCommands: () => {
        admin();
        return { commands: fx.passiveCommands() };
      },
      registerPassiveWorker: (req) => {
        admin();
        const w = create(WorkerSchema, {
          id: uuid(),
          name: req.name,
          mode: WorkerMode.PASSIVE,
          status: WorkerStatus.PENDING,
          url: req.url,
          certFingerprint: req.certFingerprint,
          createdAt: timestampFromMs(Date.now()),
        });
        workers.push(w);
        return { worker: w };
      },
      renameWorker: (req) => {
        admin();
        const w = workers.find((x) => x.id === req.workerId);
        if (!w) throw new ConnectError("worker not found", Code.NotFound);
        w.name = req.name;
        return { worker: w };
      },
      removeWorker: (req) => {
        admin();
        const i = workers.findIndex((x) => x.id === req.workerId);
        if (i >= 0) workers.splice(i, 1);
        return {};
      },
    });

    service(JobService, {
      listJobs: (req) => {
        const u = me();
        const q = req.query.toLowerCase();
        const after = req.createdAfter ? timestampMs(req.createdAfter) : -Infinity;
        const before = req.createdBefore ? timestampMs(req.createdBefore) : Infinity;
        const all = jobs.filter((j) => {
          if (!(req.allUsers && u.role === UserRole.ADMIN) && j.ownerId !== u.id) return false;
          if (req.statuses.length && !req.statuses.includes(j.status)) return false;
          if (req.workerId && j.workerId !== req.workerId) return false;
          if (req.scheduleId && j.scheduleId !== req.scheduleId) return false;
          if (req.remoteId && !j.remotes.some((r) => r.id === req.remoteId)) return false;
          if (req.repositoryId && j.repository?.id !== req.repositoryId) return false;
          if (req.kinds.length && !req.kinds.includes(j.kind)) return false;
          const at = timestampMs(j.createdAt ?? timestampFromMs(0));
          if (at < after || at > before) return false;
          if (q) {
            const op = j.definition?.operation;
            const text = [
              ...j.remotes.map((r) => r.name),
              j.title,
              j.repository?.name ?? "",
              op?.case === "rcloneTransfer"
                ? `${op.value.source?.path} ${op.value.destination?.path}`
                : "",
              op?.case === "rcloneBisync"
                ? `bisync two-way ${op.value.path1?.path} ${op.value.path2?.path}`
                : "",
            ]
              .join(" ")
              .toLowerCase();
            if (!text.includes(q)) return false;
          }
          return true;
        });
        const size = req.pageSize || 50;
        const start = req.pageToken ? Number(req.pageToken) : 0;
        const page = all.slice(start, start + size);
        return { jobs: page, nextPageToken: start + size < all.length ? String(start + size) : "" };
      },
      getJob: (req) => ({ job: job(req.jobId) }),
      listJobSamples: (req) => {
        job(req.jobId);
        return m6.listSamples(history.get(req.jobId) ?? [], req.maxPoints);
      },
      cancelJob: (req) => {
        const j = job(req.jobId);
        if (j.status === JobStatus.QUEUED) {
          j.status = JobStatus.CANCELLED;
          j.finishedAt = timestampFromMs(Date.now());
        } else if (j.status === JobStatus.RUNNING || j.status === JobStatus.DISPATCHED) {
          j.cancelRequested = true;
          // Nothing streams a restic job in the mock, so stop it here.
          if (j.kind !== "rclone_transfer" && j.kind !== bisyncMock.KIND) {
            j.status = JobStatus.CANCELLED;
            j.finishedAt = timestampFromMs(Date.now());
          }
        }
        m5.settle();
        return { job: j };
      },
      runJob: (req) => {
        const u = me();
        const op = req.definition?.operation;
        const problems = [
          ...m5.check(req.definition, false, req.workerId),
          ...hooksMock.check(req.definition, workers, req.workerId),
          ...bisyncMock.check(req.definition, { forSchedule: false }),
        ];
        if (problems.length) throw invalid(problems);
        if (op?.case === undefined)
          throw invalid([{ field: "definition", message: "Choose what to do." }]);
        if (!workers.some((w) => w.id === req.workerId))
          throw invalid([{ field: "worker_id", message: "Choose the worker that runs the job." }]);
        return {
          job: queueJob({ workerId: req.workerId, definition: req.definition, ownerId: u.id }),
        };
      },
      async *watchJob(req, ctx) {
        const j = job(req.jobId);
        let status = j.status;
        yield { update: { case: "job", value: j } };
        while (!ctx.signal.aborted) {
          if (j.status !== status) {
            status = j.status;
            yield { update: { case: "job", value: j } };
          }
          if (
            j.status === JobStatus.SUCCEEDED ||
            j.status === JobStatus.FAILED ||
            j.status === JobStatus.CANCELLED ||
            j.status === JobStatus.INTERRUPTED
          ) {
            return;
          }
          await sleep(1000, ctx.signal);
          if (j.cancelRequested && j.status === JobStatus.RUNNING) {
            j.status = JobStatus.CANCELLED;
            j.finishedAt = timestampFromMs(Date.now());
            continue;
          }
          tick(j);
          if (j.stats && j.status === JobStatus.RUNNING) {
            const sample = create(JobProgressSchema, {
              time: timestampFromMs(Date.now()),
              stats: j.stats,
            });
            const h = history.get(j.id) ?? [];
            if (Date.now() - timestampMs(h[h.length - 1]?.time ?? timestampFromMs(0)) >= 5000)
              history.set(j.id, [...h, sample]);
            yield { update: { case: "progress", value: sample } };
          }
        }
      },
    });

    // Last: its methods replace the unimplemented ones registered above.
    m6.registerM6(router, { me, workers, invalid, sleep: (ms) => sleep(ms) });
    registerM7(router, {
      me,
      workers,
      remotes: () => remotes,
      invalid,
      sleep: (ms) => sleep(ms),
      now,
      repositories: () => m5data.repositories,
      files: m5.files,
    });
  });
}
