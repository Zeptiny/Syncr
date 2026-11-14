/**
 * Dev-mock services added in milestone 4: schedules, notification channels
 * and contacts, deliveries and the dashboard. createMockTransport registers
 * them next to the M3 services, sharing its in-memory jobs and workers.
 */
import { create } from "@bufbuild/protobuf";
import {
  durationFromMs,
  timestampFromMs,
  timestampMs,
  type Timestamp,
} from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, type ConnectRouter } from "@connectrpc/connect";

import {
  JobStatus,
  WorkerStatus,
  type Job,
  type JobDefinition,
  type Worker,
} from "@/gen/syncr/v1/api_pb";
import {
  ActivityBucketSchema,
  DashboardRange,
  DashboardService,
  UpcomingRunSchema,
} from "@/gen/syncr/v1/dashboard_pb";
import {
  ChannelService,
  ContactSchema,
  ContactScheduleRefSchema,
  ContactService,
  DeliveryStatus,
  type Contact,
  type ContactInput,
  type Delivery,
} from "@/gen/syncr/v1/notify_pb";
import type { Remote } from "@/gen/syncr/v1/remotes_pb";
import {
  OverlapPolicy,
  ScheduleRunSchema,
  ScheduleRunStatus,
  ScheduleRunTrigger,
  ScheduleSchema,
  ScheduleService,
  type Schedule,
  type ScheduleInput,
  type ScheduleRun,
} from "@/gen/syncr/v1/schedules_pb";
import { UserRole, type User } from "@/gen/syncr/v1/users_pb";
import { FieldKind } from "@/gen/syncr/v1/remotes_pb";
import * as fx from "@/test/fixtures";

import { CronError, nextRuns, parseCron } from "./cron";
import { check as checkBisync } from "./bisync";
import { check as checkHooks } from "./hooks";
import type { M5 } from "./m5";
import { api as remoteBackupApi, sourceOf as remoteBackupSource } from "./remote-backup";

type Violation = { field: string; message: string };

/** M4Deps is what the M4 services share with the rest of the mock. */
export interface M4Deps {
  now: number;
  /** m5 checks and names the restic definitions of schedules. */
  m5?: Pick<M5, "check" | "describe">;
  /** history is the seeded schedules, runs and jobs (its jobs are in jobs). */
  history: fx.History;
  me: () => User;
  jobs: Job[];
  workers: Worker[];
  remotes: () => Remote[];
  invalid: (v: Violation[]) => ConnectError;
  sleep: (ms: number) => Promise<void>;
  /** queueJob adds a queued job that the mock worker starts shortly. */
  queueJob: (p: {
    workerId: string;
    definition: JobDefinition | undefined;
    ownerId: string;
    scheduleId?: string;
    scheduleName?: string;
  }) => Job;
}

const LABEL = /^[a-z0-9][a-z0-9_.-]{0,31}$/;
const ms = (t: Timestamp | undefined) => (t ? timestampMs(t) : 0);
const ts = timestampFromMs;

function isActive(s: JobStatus): boolean {
  return s === JobStatus.QUEUED || s === JobStatus.DISPATCHED || s === JobStatus.RUNNING;
}

const statusKey: Partial<Record<JobStatus, "succeeded" | "failed" | "interrupted" | "cancelled">> =
  {
    [JobStatus.SUCCEEDED]: "succeeded",
    [JobStatus.FAILED]: "failed",
    [JobStatus.INTERRUPTED]: "interrupted",
    [JobStatus.CANCELLED]: "cancelled",
  };

/** registerM4 adds the milestone 4 services to a mock router. */
export function registerM4(router: ConnectRouter, d: M4Deps): void {
  const history = d.history;
  const schedules: Schedule[] = history.schedules;
  const runs: ScheduleRun[] = history.runs;
  let contacts: Contact[] = fx.contacts(d.now);
  const deliveries: Delivery[] = fx.deliveries(d.now, history);
  const tests: number[] = []; // send-test times, for the rate limit
  let seq = 0;
  const id = (p: string) =>
    `${p}-${Date.now().toString(16).slice(-8).padStart(8, "0")}-4000-8000-${(++seq).toString(16).padStart(12, "0")}`;

  // Only the admin owns schedules and contacts in the seed, like remotes.
  const owner = () => {
    const u = d.me();
    return u.id === fx.ids.admin ? u : undefined;
  };
  const schedule = (scheduleId: string) => {
    const s = owner() && schedules.find((x) => x.id === scheduleId);
    if (!s) throw new ConnectError("schedule not found", Code.NotFound);
    return s;
  };
  const contact = (contactId: string) => {
    const c = owner() && contacts.find((x) => x.id === contactId);
    if (!c) throw new ConnectError("contact not found", Code.NotFound);
    return c;
  };
  const jobById = (jobId: string) => d.jobs.find((j) => j.id === jobId);

  // Runs carry their job's current status.
  const joined = (r: ScheduleRun): ScheduleRun => {
    const j = r.jobId ? jobById(r.jobId) : undefined;
    if (j) r.jobStatus = j.status;
    return r;
  };
  const withLastRun = (s: Schedule): Schedule => {
    const last = runs.find((r) => r.scheduleId === s.id);
    s.lastRun = last ? joined(last) : undefined;
    s.contacts = s.contacts.map((l) => {
      const c = contacts.find((x) => x.id === l.contactId);
      return { ...l, contactName: c?.name ?? l.contactName, channelType: c?.channelType ?? "" };
    });
    return s;
  };
  const activeRun = (s: Schedule) =>
    runs.some(
      (r) =>
        r.scheduleId === s.id &&
        (r.status === ScheduleRunStatus.LAUNCHING ||
          (r.jobId !== "" && isActive(jobById(r.jobId)?.status ?? JobStatus.UNSPECIFIED))),
    ) || d.jobs.some((j) => j.scheduleId === s.id && isActive(j.status));
  const nextAt = (cron: string, tz: string) => {
    const n = nextRuns(parseCron(cron, tz), Date.now(), 1)[0];
    return n ? ts(n) : undefined;
  };

  const checkSchedule = (input: ScheduleInput | undefined, selfId?: string): ScheduleInput => {
    if (!input) throw d.invalid([{ field: "input", message: "missing" }]);
    const v: Violation[] = [];
    const name = input.name.trim();
    if (!name) v.push({ field: "name", message: "Give the schedule a name." });
    else if (name.length > 64) v.push({ field: "name", message: "Use at most 64 characters." });
    else if (schedules.some((s) => s.id !== selfId && s.name.toLowerCase() === name.toLowerCase()))
      v.push({ field: "name", message: "You already have a schedule with this name." });
    try {
      parseCron(input.cron, input.timezone);
    } catch (e) {
      if (e instanceof CronError) v.push({ field: e.field, message: e.message });
      else throw e;
    }
    const t = input.target?.target;
    if (t?.case === "workerId") {
      if (!d.workers.some((w) => w.id === t.value))
        v.push({ field: "target.worker_id", message: "This worker no longer exists." });
    } else if (t?.case === "workerLabel") {
      if (!LABEL.test(t.value))
        v.push({
          field: "target.worker_label",
          message: "Use lower-case letters, digits, dots, dashes and underscores.",
        });
    } else {
      v.push({ field: "target.worker_id", message: "Choose a worker or a label." });
    }
    const delay = input.missedRun?.maxDelay;
    if (delay && Number(delay.seconds) > 7 * 86_400)
      v.push({ field: "missed_run.max_delay", message: "At most 7 days." });
    input.contacts.forEach((l, i) => {
      if (!contacts.some((c) => c.id === l.contactId))
        v.push({ field: `contacts[${i}].contact_id`, message: "This contact no longer exists." });
      if (!l.events.length)
        v.push({ field: `contacts[${i}].events`, message: "Choose at least one event." });
    });
    v.push(
      ...checkHooks(input.definition, d.workers, t?.case === "workerId" ? t.value : undefined),
    );
    // A backup of a remote runs on one worker (its other checks are m5's).
    if (t?.case === "workerLabel" && remoteBackupSource(input.definition))
      v.push({ field: "target.worker_label", message: remoteBackupApi.singleWorker });
    v.push(
      ...checkBisync(input.definition, {
        forSchedule: true,
        label: t?.case === "workerLabel",
      }),
    );
    const op = input.definition?.operation;
    if (op?.case === "rcloneBisync") {
      // Checked above.
    } else if (op?.case !== "rcloneTransfer") {
      if (op?.case === undefined || !d.m5)
        v.push({ field: "definition", message: "Choose what to transfer." });
      else
        v.push(...d.m5.check(input.definition, true, t?.case === "workerId" ? t.value : undefined));
    } else {
      for (const [side, loc] of [
        ["source", op.value.source],
        ["destination", op.value.destination],
      ] as const) {
        if (!loc?.remoteId && !loc?.path.startsWith("/") && !/^[A-Za-z]:\\/.test(loc?.path ?? ""))
          v.push({
            field: `definition.rclone_transfer.${side}.path`,
            message: "Enter an absolute path on the worker.",
          });
      }
    }
    if (v.length) throw d.invalid(v);
    return input;
  };

  const fromInput = (
    input: ScheduleInput,
    base: { id: string; createdAt?: Timestamp; version: bigint; pausedReason?: string },
  ): Schedule => {
    const op = input.definition?.operation;
    const ids =
      op?.case === "rcloneTransfer"
        ? [op.value.source?.remoteId, op.value.destination?.remoteId].filter(
            (x): x is string => !!x,
          )
        : op?.case === "rcloneBisync"
          ? [op.value.path1?.remoteId, op.value.path2?.remoteId].filter((x): x is string => !!x)
          : [];
    const restic = d.m5?.describe(input.definition);
    return create(ScheduleSchema, {
      ...base,
      ownerId: fx.ids.admin,
      name: input.name.trim(),
      description: input.description.trim(),
      cron: parseCron(input.cron, input.timezone).normalized,
      timezone: input.timezone,
      target: input.target,
      missedRun: input.missedRun,
      overlap: input.overlap || OverlapPolicy.SKIP,
      enabled: input.enabled,
      pausedReason: input.enabled ? "" : (base.pausedReason ?? ""),
      definition: input.definition,
      repository: restic?.repository,
      remotes: restic
        ? restic.remotes
        : [...new Set(ids)].map((rid) => {
            const r = d.remotes().find((x) => x.id === rid);
            return { id: rid, name: r?.name ?? "?", backendType: r?.backendType ?? "" };
          }),
      contacts: input.contacts.map((l) => ({ contactId: l.contactId, events: l.events })),
      nextRunAt: input.enabled ? nextAt(input.cron, input.timezone) : undefined,
      updatedAt: ts(Date.now()),
    });
  };

  router.service(ScheduleService, {
    listSchedules: (req) => {
      if (!owner()) return { schedules: [] };
      return {
        schedules: schedules
          .filter((s) => !req.remoteId || s.remotes.some((r) => r.id === req.remoteId))
          .filter((s) => !req.contactId || s.contacts.some((c) => c.contactId === req.contactId))
          .filter((s) => !req.repositoryId || s.repository?.id === req.repositoryId)
          .map(withLastRun)
          .sort((a, b) => a.name.localeCompare(b.name)),
      };
    },
    getSchedule: (req) => ({ schedule: withLastRun(schedule(req.scheduleId)) }),
    createSchedule: async (req) => {
      d.me();
      await d.sleep(300);
      const input = checkSchedule(req.input);
      const s = fromInput(input, {
        id: id("4b8f0d2a"),
        createdAt: ts(Date.now()),
        version: 1n,
      });
      schedules.push(s);
      return { schedule: withLastRun(s) };
    },
    updateSchedule: async (req) => {
      const s = schedule(req.scheduleId);
      await d.sleep(300);
      if (req.version !== s.version)
        throw new ConnectError("the schedule was changed in the meantime", Code.Aborted);
      const input = checkSchedule(req.input, s.id);
      const next = fromInput(input, {
        id: s.id,
        createdAt: s.createdAt,
        version: s.version + 1n,
        pausedReason: s.pausedReason,
      });
      Object.assign(s, next);
      return { schedule: withLastRun(s) };
    },
    setScheduleEnabled: (req) => {
      const s = schedule(req.scheduleId);
      s.enabled = req.enabled;
      if (req.enabled) {
        s.pausedReason = "";
        s.nextRunAt = nextAt(s.cron, s.timezone);
      } else {
        s.nextRunAt = undefined;
        for (const r of runs) {
          if (r.scheduleId === s.id && r.status === ScheduleRunStatus.PENDING) {
            r.status = ScheduleRunStatus.CANCELLED;
            r.reason = "disabled";
          }
        }
      }
      return { schedule: withLastRun(s) };
    },
    deleteSchedule: (req) => {
      const s = schedule(req.scheduleId);
      schedules.splice(schedules.indexOf(s), 1);
      for (let i = runs.length - 1; i >= 0; i--) if (runs[i].scheduleId === s.id) runs.splice(i, 1);
      return {};
    },
    runScheduleNow: async (req) => {
      const s = schedule(req.scheduleId);
      await d.sleep(250);
      if (s.overlap === OverlapPolicy.SKIP && activeRun(s)) {
        throw new ConnectError(
          "the schedule's last run is still going and its overlap policy skips new runs",
          Code.FailedPrecondition,
        );
      }
      const t = s.target?.target;
      const worker =
        t?.case === "workerId"
          ? d.workers.find((w) => w.id === t.value)
          : d.workers.find(
              (w) => w.status === WorkerStatus.ONLINE && w.labels.includes(t?.value ?? ""),
            );
      const run = create(ScheduleRunSchema, {
        id: id("e6f7a8b9"),
        scheduleId: s.id,
        trigger: ScheduleRunTrigger.MANUAL,
        scheduledFor: ts(Date.now()),
        deadline: ts(Date.now() + 86_400_000),
        status: ScheduleRunStatus.PENDING,
        reason: worker?.status === WorkerStatus.ONLINE ? "" : "worker_offline",
        workerId: worker?.id ?? "",
        createdAt: ts(Date.now()),
        updatedAt: ts(Date.now()),
      });
      runs.unshift(run);
      if (worker?.status === WorkerStatus.ONLINE) {
        const j = d.queueJob({
          workerId: worker.id,
          definition: s.definition,
          ownerId: s.ownerId,
          scheduleId: s.id,
          scheduleName: s.name,
        });
        run.status = ScheduleRunStatus.LAUNCHED;
        run.jobId = j.id;
        run.jobStatus = j.status;
      }
      return { run };
    },
    listScheduleRuns: (req) => {
      schedule(req.scheduleId);
      const all = runs.filter((r) => r.scheduleId === req.scheduleId).map(joined);
      const size = req.pageSize || 50;
      const start = req.pageToken ? Number(req.pageToken) : 0;
      return {
        runs: all.slice(start, start + size),
        nextPageToken: start + size < all.length ? String(start + size) : "",
      };
    },
    previewSchedule: (req) => {
      d.me();
      try {
        const e = parseCron(req.cron, req.timezone);
        const after = req.after ? timestampMs(req.after) : Date.now();
        const n = Math.min(20, req.count || 5);
        return { nextRuns: nextRuns(e, after, n).map(ts), normalized: e.normalized };
      } catch (e) {
        if (e instanceof CronError) throw d.invalid([{ field: e.field, message: e.message }]);
        throw e;
      }
    },
  });

  router.service(ChannelService, {
    listChannels: () => {
      d.me();
      return { channels: fx.channels };
    },
  });

  const usedBy = (c: Contact): Contact => {
    c.usedBy = schedules
      .filter((s) => s.contacts.some((l) => l.contactId === c.id))
      .map((s) => create(ContactScheduleRefSchema, { id: s.id, name: s.name }));
    return c;
  };

  const checkContact = (input: ContactInput | undefined, stored?: Contact): ContactInput => {
    if (!input) throw d.invalid([{ field: "input", message: "missing" }]);
    const ch = fx.channels.find((x) => x.id === input.channelType);
    if (!ch) throw d.invalid([{ field: "channel_type", message: "Unknown channel." }]);
    const v: Violation[] = [];
    const name = input.name.trim();
    if (!name) v.push({ field: "name", message: "Give the contact a name." });
    else if (
      contacts.some((c) => c.id !== stored?.id && c.name.toLowerCase() === name.toLowerCase())
    )
      v.push({ field: "name", message: "You already have a contact with this name." });
    for (const f of ch.fields) {
      if (!f.required) continue;
      const kept =
        f.secret && stored?.secretsSet.includes(f.name) && !input.clearSecrets.includes(f.name);
      const val = f.secret ? input.secrets[f.name] : input.options[f.name];
      if (!val && !kept && !(f.kind === FieldKind.ENUM && f.defaultValue))
        v.push({
          field: `${f.secret ? "secrets" : "options"}.${f.name}`,
          message: `${f.label} is required.`,
        });
    }
    const hook = input.secrets.webhook_url;
    if (
      hook &&
      !/^https:\/\/(ptb\.|canary\.)?discord(app)?\.com\/api\/webhooks\/\d+\/[\w-]+$/.test(hook)
    )
      v.push({
        field: "secrets.webhook_url",
        message: "Paste the webhook URL Discord shows, like https://discord.com/api/webhooks/….",
      });
    if (input.channelType === "email") {
      const hasPassword =
        !!input.secrets.password ||
        (stored?.secretsSet.includes("password") && !input.clearSecrets.includes("password"));
      if (input.options.security === "none" && hasPassword)
        v.push({
          field: "options.security",
          message: "A password is never sent unencrypted. Choose STARTTLS or TLS.",
        });
      const to = (input.options.to ?? "").split(",").filter((x) => x.trim());
      if (to.length > 20) v.push({ field: "options.to", message: "At most 20 recipients." });
    }
    if (v.length) throw d.invalid(v);
    return input;
  };

  const summary = (input: ContactInput, secretsSet: string[]): string => {
    switch (input.channelType) {
      case "discord":
        return "discord.com webhook";
      case "email": {
        const n = (input.options.to ?? "").split(",").filter((x) => x.trim()).length;
        return `${input.options.host} → ${n} ${n === 1 ? "recipient" : "recipients"}`;
      }
      case "webhook":
        return `${secretsSet.includes("signing_secret") ? "Signed webhook" : "Webhook"}, ${input.options.format === "slack" ? "Slack text" : "Syncr JSON"}`;
      default:
        return input.channelType;
    }
  };

  const secretsAfter = (input: ContactInput, stored?: Contact) => [
    ...new Set([
      ...(stored?.secretsSet ?? []).filter((k) => !input.clearSecrets.includes(k)),
      ...Object.entries(input.secrets)
        .filter(([, v]) => v)
        .map(([k]) => k),
    ]),
  ];

  router.service(ContactService, {
    listContacts: (req) => {
      if (!owner()) return { contacts: [] };
      return {
        contacts: contacts
          .filter((c) => !req.channelType || c.channelType === req.channelType)
          .map(usedBy),
      };
    },
    getContact: (req) => ({ contact: usedBy(contact(req.contactId)) }),
    createContact: async (req) => {
      d.me();
      await d.sleep(300);
      const input = checkContact(req.input);
      const secretsSet = secretsAfter(input);
      const c = create(ContactSchema, {
        id: id("8c1d3e5f"),
        name: input.name.trim(),
        channelType: input.channelType,
        options: input.options,
        secretsSet,
        summary: summary(input, secretsSet),
        enabled: input.enabled,
        jobEvents: input.jobEvents,
        createdAt: ts(Date.now()),
        updatedAt: ts(Date.now()),
        version: 1n,
      });
      contacts = [...contacts, c].sort((a, b) => a.name.localeCompare(b.name));
      return { contact: usedBy(c) };
    },
    updateContact: async (req) => {
      const c = contact(req.contactId);
      await d.sleep(300);
      if (req.version !== c.version)
        throw new ConnectError("the contact was changed in the meantime", Code.Aborted);
      const input = checkContact(req.input, c);
      c.secretsSet = secretsAfter(input, c);
      c.name = input.name.trim();
      c.options = input.options;
      c.enabled = input.enabled;
      c.jobEvents = input.jobEvents;
      c.summary = summary(input, c.secretsSet);
      c.version++;
      c.updatedAt = ts(Date.now());
      return { contact: usedBy(c) };
    },
    deleteContact: (req) => {
      const c = contact(req.contactId);
      contacts = contacts.filter((x) => x.id !== c.id);
      for (const s of schedules) s.contacts = s.contacts.filter((l) => l.contactId !== c.id);
      for (let i = deliveries.length - 1; i >= 0; i--)
        if (deliveries[i].contactId === c.id) deliveries.splice(i, 1);
      return {};
    },
    sendTestNotification: async (req) => {
      d.me();
      const stored = req.contactId ? contact(req.contactId) : undefined;
      const t = Date.now();
      while (tests.length && t - tests[0] > 30_000) tests.shift();
      if (tests.length >= 3 && t - tests[tests.length - 1] < 10_000)
        throw new ConnectError(
          "too many test notifications; wait a few seconds",
          Code.ResourceExhausted,
        );
      tests.push(t);
      const type = req.input?.channelType || stored?.channelType || "";
      const opts = { ...stored?.options, ...req.input?.options };
      const url = req.input?.secrets.url ?? "";
      const latency = 280 + Math.round(Math.random() * 500);
      await d.sleep(latency);
      const res = (ok: boolean, status: number, error = "") => ({
        ok,
        status,
        error,
        latency: durationFromMs(latency),
      });
      if (type === "webhook" && /\/\/(localhost|127\.|169\.254\.)/.test(url))
        return res(
          false,
          0,
          "Blocked by the notification network policy: the URL points at a loopback or link-local address.",
        );
      if (type === "email" && /fail|invalid/.test(opts.host ?? ""))
        return res(false, 535, "SMTP 535: 5.7.8 Authentication credentials invalid.");
      if (type === "email") return res(true, 250);
      if (type === "discord") return res(true, 204);
      return res(true, 200);
    },
    listDeliveries: (req) => {
      if (!owner()) return { deliveries: [], nextPageToken: "" };
      const all = deliveries.filter(
        (x) =>
          (!req.contactId || x.contactId === req.contactId) &&
          (!req.statuses.length || req.statuses.includes(x.status)),
      );
      const size = req.pageSize || 50;
      const start = req.pageToken ? Number(req.pageToken) : 0;
      return {
        deliveries: all.slice(start, start + size),
        nextPageToken: start + size < all.length ? String(start + size) : "",
      };
    },
    retryDelivery: (req) => {
      d.me();
      const x = deliveries.find((y) => y.id === req.deliveryId);
      if (!x || !owner()) throw new ConnectError("delivery not found", Code.NotFound);
      if (x.status !== DeliveryStatus.DEAD)
        throw new ConnectError(
          "only deliveries that gave up can be retried",
          Code.FailedPrecondition,
        );
      x.status = DeliveryStatus.PENDING;
      x.attempts = 0;
      x.nextAttemptAt = ts(Date.now());
      setTimeout(() => {
        x.status = DeliveryStatus.DELIVERED;
        x.attempts = 1;
        x.lastAttemptAt = ts(Date.now());
        x.deliveredAt = ts(Date.now());
        x.lastError = "";
      }, 2000);
      return { delivery: x };
    },
  });

  router.service(DashboardService, {
    getDashboard: (req) => dashboard(req),
  });
  const dashboard = (req: { range: DashboardRange; allUsers: boolean }) => {
    const u = d.me();
    const all = req.allUsers && u.role === UserRole.ADMIN;
    const mine = d.jobs.filter((j) => all || j.ownerId === u.id);
    const range = req.range || DashboardRange.DAY;
    const hourly = range === DashboardRange.DAY;
    const n = hourly ? 24 : range === DashboardRange.WEEK ? 7 : 30;
    const end = new Date();
    if (hourly) end.setMinutes(0, 0, 0);
    else end.setHours(0, 0, 0, 0);
    const starts: number[] = [];
    for (let i = n - 1; i >= 0; i--) {
      const s = new Date(end);
      if (hourly) s.setHours(s.getHours() - i);
      else s.setDate(s.getDate() - i);
      starts.push(s.getTime());
    }
    const last = new Date(end);
    if (hourly) last.setHours(last.getHours() + 1);
    else last.setDate(last.getDate() + 1);
    const from = starts[0];
    const until = last.getTime();
    const buckets = starts.map((start) => create(ActivityBucketSchema, { start: ts(start) }));
    const totals = {
      succeeded: 0n,
      failed: 0n,
      interrupted: 0n,
      cancelled: 0n,
      bytes: 0n,
      files: 0n,
      successRate: 0,
      running: 0n,
      queued: 0n,
      missedRuns: 0n,
    };
    for (const j of mine) {
      if (j.status === JobStatus.RUNNING || j.status === JobStatus.DISPATCHED) totals.running++;
      if (j.status === JobStatus.QUEUED) totals.queued++;
      const fin = ms(j.finishedAt);
      if (!fin || fin < from || fin >= until) continue;
      let i = starts.length - 1;
      while (i > 0 && starts[i] > fin) i--;
      const b = buckets[i];
      const bytes = j.stats?.bytes ?? 0n;
      const files = j.stats?.files ?? 0n;
      b.bytes += bytes;
      b.files += files;
      totals.bytes += bytes;
      totals.files += files;
      const key = statusKey[j.status];
      if (key) {
        b[key]++;
        totals[key]++;
      }
    }
    const done = totals.succeeded + totals.failed + totals.interrupted;
    totals.successRate = done ? Number(totals.succeeded) / Number(done) : 0;
    const ownRuns = owner() ? runs : [];
    totals.missedRuns = BigInt(
      ownRuns.filter(
        (r) =>
          r.status === ScheduleRunStatus.MISSED &&
          ms(r.scheduledFor) >= from &&
          ms(r.scheduledFor) < until,
      ).length,
    );
    const upcoming = (owner() ? schedules : [])
      .filter((s) => s.enabled && s.nextRunAt)
      .sort((a, b) => ms(a.nextRunAt) - ms(b.nextRunAt))
      .slice(0, 8)
      .map((s) => {
        const t = s.target?.target;
        const w =
          t?.case === "workerId"
            ? d.workers.find((x) => x.id === t.value)
            : (d.workers.find(
                (x) => x.labels.includes(t?.value ?? "") && x.status === WorkerStatus.ONLINE,
              ) ?? d.workers.find((x) => x.labels.includes(t?.value ?? "")));
        return create(UpcomingRunSchema, {
          scheduleId: s.id,
          scheduleName: s.name,
          at: s.nextRunAt,
          target: s.target,
          workerName: w?.name ?? "",
          workerStatus: w?.status ?? WorkerStatus.UNSPECIFIED,
        });
      });
    const count = (st: WorkerStatus) => d.workers.filter((w) => w.status === st).length;
    return {
      from: ts(from),
      until: ts(until),
      bucketUnit: hourly ? "hour" : "day",
      totals,
      activity: buckets,
      upcoming,
      workers: {
        online: count(WorkerStatus.ONLINE),
        offline: count(WorkerStatus.OFFLINE),
        pending: count(WorkerStatus.PENDING),
        workers: d.workers,
      },
      recentFailures: mine
        .filter((j) => j.status === JobStatus.FAILED || j.status === JobStatus.INTERRUPTED)
        .sort((a, b) => ms(b.finishedAt) - ms(a.finishedAt))
        .slice(0, 5),
      recentMissed: ownRuns
        .filter(
          (r) => r.status === ScheduleRunStatus.MISSED || r.status === ScheduleRunStatus.FAILED,
        )
        .slice(0, 5),
    };
  };
}
