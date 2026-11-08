/**
 * Fixtures shaped like the controller's built-in descriptors and the dev
 * seed. Tests use them with createRouterTransport; the dev-only mock
 * transport (VITE_SYNCR_MOCK=1) serves them to the running app. Times are
 * relative to `now` so tests can pin it.
 */
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromMs } from "@bufbuild/protobuf/wkt";

import {
  InstallCommandSchema,
  InstallPlatform,
  InstallScope,
  JobProgressSchema,
  JobSchema,
  JobStatus,
  WorkerSchema,
  WorkerStatus,
  type InstallCommand,
  type Job,
  type JobProgress,
  type Worker,
} from "@/gen/syncr/v1/api_pb";
import { APITokenSchema, type APIToken } from "@/gen/syncr/v1/auth_pb";
import {
  JobErrorCode,
  RcloneTransferMode,
  UnionUpstreamMode,
  WorkerMode,
} from "@/gen/syncr/v1/link_pb";
import {
  BackendFieldSchema,
  BackendSchema,
  FieldKind,
  RemoteSchema,
  type Backend,
  type BackendField,
  type Remote,
} from "@/gen/syncr/v1/remotes_pb";
import {
  ChannelTypeSchema,
  ContactSchema,
  DeliverySchema,
  DeliveryStatus,
  NotificationEvent,
  type ChannelType,
  type Contact,
  type Delivery,
} from "@/gen/syncr/v1/notify_pb";
import {
  MissedRunAction,
  OverlapPolicy,
  ScheduleRunSchema,
  ScheduleRunStatus,
  ScheduleRunTrigger,
  ScheduleSchema,
  type Schedule,
  type ScheduleRun,
} from "@/gen/syncr/v1/schedules_pb";
import { UserRole, UserSchema, type User } from "@/gen/syncr/v1/users_pb";
import { next, previousRuns } from "@/mock/cron";

type FieldInit = Omit<MessageInitShape<typeof BackendFieldSchema>, "$typeName">;

const MIN = 60_000;
const HOUR = 60 * MIN;
const DAY = 24 * HOUR;
const GiB = 1024 ** 3;
const MiB = 1024 ** 2;

function f(name: string, label: string, kind: FieldKind, rest: Partial<FieldInit> = {}): FieldInit {
  return { name, label, kind, ...rest };
}

function choices(...values: string[]): { value: string; label: string }[] {
  return values.map((v) => ({ value: v, label: v }));
}

/** Backend descriptors like internal/backend/builtin. */
export const backends: Backend[] = [
  create(BackendSchema, {
    id: "b2",
    label: "Backblaze B2",
    description: "Backblaze B2 cloud storage buckets.",
    capabilities: { unionMember: true, resticRepository: true },
    fields: [
      f("account", "Account ID", FieldKind.STRING, {
        required: true,
        help: "The application key ID.",
      }),
      f("key", "Application key", FieldKind.STRING, { required: true, secret: true }),
      f("bucket", "Bucket", FieldKind.STRING, {
        help: "Optional bucket (and path) every location starts in.",
      }),
      f("endpoint", "Endpoint", FieldKind.STRING, { advanced: true }),
      f("download_url", "Download URL", FieldKind.STRING, { advanced: true }),
      f("hard_delete", "Delete permanently", FieldKind.BOOL, {
        advanced: true,
        help: "Delete files instead of hiding them.",
      }),
      f("versions", "Include old versions", FieldKind.BOOL, { advanced: true }),
    ],
  }),
  create(BackendSchema, {
    id: "local",
    label: "Local folder",
    description: "A folder on the worker's own disks, inside its allowed folders.",
    capabilities: { local: true, unionMember: true, resticRepository: true },
    fields: [
      f("path", "Folder", FieldKind.STRING, {
        required: true,
        placeholder: "/srv/data",
        help: "An absolute path on the worker.",
      }),
    ],
  }),
  create(BackendSchema, {
    id: "s3",
    label: "Amazon S3 and compatible",
    description: "AWS S3, MinIO, Cloudflare R2, Wasabi and other S3 APIs.",
    capabilities: { unionMember: true, resticRepository: true },
    fields: [
      f("provider", "Provider", FieldKind.ENUM, {
        defaultValue: "AWS",
        choices: choices("AWS", "Minio", "Cloudflare", "Wasabi", "DigitalOcean", "Ceph", "Other"),
      }),
      f("access_key_id", "Access key ID", FieldKind.STRING, { required: true }),
      f("secret_access_key", "Secret access key", FieldKind.STRING, {
        required: true,
        secret: true,
      }),
      f("region", "Region", FieldKind.STRING, { placeholder: "us-east-1" }),
      f("endpoint", "Endpoint", FieldKind.STRING, {
        placeholder: "https://minio.example.com",
        help: "Required unless the provider is AWS.",
      }),
      f("bucket", "Bucket", FieldKind.STRING, {
        help: "Optional bucket (and path) every location starts in.",
      }),
      f("location_constraint", "Location constraint", FieldKind.STRING, { advanced: true }),
      f("acl", "Object ACL", FieldKind.ENUM, {
        advanced: true,
        choices: choices("private", "public-read", "bucket-owner-full-control"),
      }),
      f("storage_class", "Storage class", FieldKind.STRING, {
        advanced: true,
        placeholder: "STANDARD",
      }),
      f("server_side_encryption", "Server-side encryption", FieldKind.ENUM, {
        advanced: true,
        choices: choices("AES256", "aws:kms"),
      }),
      f("sse_kms_key_id", "KMS key ID", FieldKind.STRING, { advanced: true }),
      f("force_path_style", "Path-style URLs", FieldKind.BOOL, {
        advanced: true,
        defaultValue: "true",
      }),
      f("no_check_bucket", "Don't check the bucket exists", FieldKind.BOOL, { advanced: true }),
    ],
  }),
  create(BackendSchema, {
    id: "sftp",
    label: "SFTP",
    description: "Any server reachable over SSH.",
    capabilities: { unionMember: true, resticRepository: true },
    fields: [
      f("host", "Host", FieldKind.STRING, { required: true, placeholder: "backup.example.com" }),
      f("port", "Port", FieldKind.INT, { defaultValue: "22" }),
      f("user", "User", FieldKind.STRING, { required: true }),
      f("pass", "Password", FieldKind.STRING, {
        secret: true,
        help: "A password or a private key is required.",
      }),
      f("key_pem", "Private key", FieldKind.TEXT, {
        secret: true,
        placeholder: "-----BEGIN OPENSSH PRIVATE KEY-----",
        help: "PEM-encoded private key.",
      }),
      f("path", "Path", FieldKind.STRING, { help: "Optional folder every location starts in." }),
      f("key_file_pass", "Key passphrase", FieldKind.STRING, { secret: true, advanced: true }),
      f("shell_type", "Remote shell", FieldKind.ENUM, {
        advanced: true,
        choices: choices("unix", "powershell", "cmd", "none"),
      }),
      f("disable_hashcheck", "Skip hash checks", FieldKind.BOOL, { advanced: true }),
      f("md5sum_command", "MD5 command", FieldKind.STRING, { advanced: true }),
      f("sha1sum_command", "SHA-1 command", FieldKind.STRING, { advanced: true }),
      f("use_insecure_cipher", "Allow insecure ciphers", FieldKind.BOOL, { advanced: true }),
    ],
  }),
  create(BackendSchema, {
    id: "union",
    label: "Union",
    description: "Combine several remotes into one pool.",
    capabilities: { union: true },
    fields: [
      f("action_policy", "Action policy", FieldKind.ENUM, {
        defaultValue: "epall",
        choices: choices("epall", "epff", "eplfs", "eplus", "epmfs", "all", "ff"),
      }),
      f("create_policy", "Create policy", FieldKind.ENUM, {
        defaultValue: "epmfs",
        choices: choices("epmfs", "epff", "eplfs", "eplus", "mfs", "ff", "lfs", "rand"),
      }),
      f("search_policy", "Search policy", FieldKind.ENUM, {
        defaultValue: "ff",
        choices: choices("ff", "all", "epff", "newest"),
      }),
      f("cache_time", "Cache time (seconds)", FieldKind.INT, {
        defaultValue: "120",
        advanced: true,
      }),
      f("members", "Members", FieldKind.MEMBERS, { required: true, help: "At least two remotes." }),
    ],
  }),
  create(BackendSchema, {
    id: "webdav",
    label: "WebDAV",
    description: "Nextcloud, ownCloud, SharePoint and other WebDAV servers.",
    capabilities: { unionMember: true, resticRepository: true },
    fields: [
      f("url", "URL", FieldKind.STRING, {
        required: true,
        placeholder: "https://cloud.example.com/remote.php/dav/files/me",
      }),
      f("vendor", "Vendor", FieldKind.ENUM, {
        choices: choices(
          "other",
          "nextcloud",
          "owncloud",
          "sharepoint",
          "sharepoint-ntlm",
          "rclone",
        ),
      }),
      f("user", "User", FieldKind.STRING),
      f("pass", "Password", FieldKind.STRING, { secret: true }),
      f("bearer_token", "Bearer token", FieldKind.STRING, { secret: true, advanced: true }),
    ],
  }),
];

/** backend returns a fixture descriptor by ID. */
export function backend(id: string): Backend {
  const b = backends.find((x) => x.id === id);
  if (!b) throw new Error(`no fixture backend ${id}`);
  return b;
}

/** field returns a descriptor field by backend and name. */
export function field(backendId: string, name: string): BackendField {
  const fl = backend(backendId).fields.find((x) => x.name === name);
  if (!fl) throw new Error(`no field ${backendId}.${name}`);
  return fl;
}

export const ids = {
  admin: "0b9c8f1e-4a7d-4c55-9a43-6f1d1a1e0a01",
  alice: "0b9c8f1e-4a7d-4c55-9a43-6f1d1a1e0a02",
  nas: "5a1e2b7c-1d3f-4e8a-9b0c-2d4f6a8b0c01",
  office: "5a1e2b7c-1d3f-4e8a-9b0c-2d4f6a8b0c02",
  backupHost: "5a1e2b7c-1d3f-4e8a-9b0c-2d4f6a8b0c03",
  laptop: "5a1e2b7c-1d3f-4e8a-9b0c-2d4f6a8b0c04",
  minio: "9e7d6c5b-2a1f-4b3c-8d9e-0f1a2b3c4d01",
  storageBox: "9e7d6c5b-2a1f-4b3c-8d9e-0f1a2b3c4d02",
  photos: "9e7d6c5b-2a1f-4b3c-8d9e-0f1a2b3c4d03",
  b2: "9e7d6c5b-2a1f-4b3c-8d9e-0f1a2b3c4d04",
  nextcloud: "9e7d6c5b-2a1f-4b3c-8d9e-0f1a2b3c4d05",
  pooled: "9e7d6c5b-2a1f-4b3c-8d9e-0f1a2b3c4d06",
  media: "9e7d6c5b-2a1f-4b3c-8d9e-0f1a2b3c4d07",
  runningJob: "c3d2e1f0-7a6b-4c5d-8e9f-a0b1c2d3e401",
  removedLaptop: "5a1e2b7c-1d3f-4e8a-9b0c-2d4f6a8b0c09",
  nightly: "4b8f0d2a-6c1e-4f3a-9d5b-7e2c1a0f3b01",
  officeHourly: "4b8f0d2a-6c1e-4f3a-9d5b-7e2c1a0f3b02",
  weekly: "4b8f0d2a-6c1e-4f3a-9d5b-7e2c1a0f3b03",
  offsite: "4b8f0d2a-6c1e-4f3a-9d5b-7e2c1a0f3b04",
  laptopSync: "4b8f0d2a-6c1e-4f3a-9d5b-7e2c1a0f3b05",
  opsDiscord: "8c1d3e5f-7a9b-4c2d-8e0f-1a3b5c7d9e01",
  adminMail: "8c1d3e5f-7a9b-4c2d-8e0f-1a3b5c7d9e02",
  homeAssistant: "8c1d3e5f-7a9b-4c2d-8e0f-1a3b5c7d9e03",
};

function ts(ms: number) {
  return timestampFromMs(ms);
}

/** users returns the seeded users. */
export function users(now: number): User[] {
  return [
    create(UserSchema, {
      id: ids.admin,
      username: "admin",
      displayName: "Dana Kowalski",
      role: UserRole.ADMIN,
      createdAt: ts(now - 40 * DAY),
      lastLoginAt: ts(now - 2 * MIN),
      version: 3n,
    }),
    create(UserSchema, {
      id: ids.alice,
      username: "alice",
      displayName: "Alice Moreau",
      role: UserRole.USER,
      createdAt: ts(now - 21 * DAY),
      lastLoginAt: ts(now - 2 * DAY),
      version: 1n,
    }),
    create(UserSchema, {
      id: "0b9c8f1e-4a7d-4c55-9a43-6f1d1a1e0a03",
      username: "intern",
      displayName: "",
      role: UserRole.USER,
      disabled: true,
      createdAt: ts(now - 12 * DAY),
      version: 2n,
    }),
  ];
}

/** workers returns the seeded workers. */
export function workers(now: number): Worker[] {
  const tools = [
    { name: "rclone", version: "v1.71.2" },
    { name: "restic", version: "0.18.1" },
  ];
  return [
    create(WorkerSchema, {
      id: ids.backupHost,
      name: "backup-host",
      labels: ["offsite"],
      mode: WorkerMode.PASSIVE,
      status: WorkerStatus.ONLINE,
      lastSeen: ts(now - 4_000),
      createdAt: ts(now - 30 * DAY),
      url: "https://backup-host.lan:7443",
      certFingerprint: "sha256:8f:2c:91:d4:6a:0e:b7:13:5c:fa:22:9d:01:7e:c4:aa",
      info: {
        workerVersion: "v2.0.0",
        os: "linux",
        arch: "arm64",
        hostname: "backup-host",
        tools,
        jobKinds: ["rclone_transfer"],
        maxConcurrentJobs: 2,
        localRoots: ["/srv/backup"],
        reportedAt: ts(now - 4_000),
      },
    }),
    create(WorkerSchema, {
      id: ids.laptop,
      name: "laptop",
      mode: WorkerMode.ACTIVE,
      status: WorkerStatus.PENDING,
      createdAt: ts(now - 20 * MIN),
    }),
    create(WorkerSchema, {
      id: ids.nas,
      name: "nas-01",
      labels: ["linux", "nas"],
      mode: WorkerMode.ACTIVE,
      status: WorkerStatus.ONLINE,
      lastSeen: ts(now - 2_000),
      createdAt: ts(now - 38 * DAY),
      info: {
        workerVersion: "v2.0.0",
        os: "linux",
        arch: "amd64",
        hostname: "nas-01",
        tools,
        jobKinds: ["rclone_transfer", "restic_backup"],
        maxConcurrentJobs: 4,
        localRoots: ["/srv/photos", "/mnt/media", "/srv/backup"],
        reportedAt: ts(now - 2_000),
      },
    }),
    create(WorkerSchema, {
      id: ids.office,
      name: "office-pc",
      labels: ["desktop", "windows"],
      mode: WorkerMode.ACTIVE,
      status: WorkerStatus.OFFLINE,
      lastSeen: ts(now - 3 * HOUR),
      createdAt: ts(now - 25 * DAY),
      info: {
        workerVersion: "v2.0.0",
        os: "windows",
        arch: "amd64",
        hostname: "OFFICE-PC",
        tools: [{ name: "rclone", version: "v1.71.2" }],
        jobKinds: ["rclone_transfer"],
        maxConcurrentJobs: 2,
        localRoots: ["D:\\Shared", "C:\\Users\\dana\\Documents"],
        reportedAt: ts(now - 3 * HOUR),
      },
    }),
  ];
}

/** remotes returns the admin's seeded remotes, by name. */
export function remotes(now: number): Remote[] {
  const base = { createdAt: ts(now - 30 * DAY), updatedAt: ts(now - 3 * DAY), version: 2n };
  return [
    create(RemoteSchema, {
      ...base,
      id: ids.b2,
      name: "B2 offsite",
      backendType: "b2",
      options: { account: "0031f2e9a4b7c50000000003", bucket: "syncr-offsite" },
      secretsSet: ["key"],
    }),
    create(RemoteSchema, {
      ...base,
      id: ids.media,
      name: "Media disk",
      backendType: "local",
      options: { path: "/mnt/media" },
      usedByUnions: [ids.pooled],
    }),
    create(RemoteSchema, {
      ...base,
      id: ids.minio,
      name: "MinIO archive",
      backendType: "s3",
      options: {
        provider: "Minio",
        access_key_id: "syncr-archive",
        endpoint: "https://minio.lan:9000",
        bucket: "archive",
        region: "eu-west-1",
      },
      secretsSet: ["secret_access_key"],
    }),
    create(RemoteSchema, {
      ...base,
      id: ids.nextcloud,
      name: "Nextcloud",
      backendType: "webdav",
      options: {
        url: "https://cloud.example.net/remote.php/dav/files/dana",
        vendor: "nextcloud",
        user: "dana",
      },
      secretsSet: ["pass"],
    }),
    create(RemoteSchema, {
      ...base,
      id: ids.photos,
      name: "Photos",
      backendType: "local",
      options: { path: "/srv/photos" },
      usedByUnions: [ids.pooled],
    }),
    create(RemoteSchema, {
      ...base,
      id: ids.pooled,
      name: "Pooled disks",
      backendType: "union",
      options: { action_policy: "epall", create_policy: "epmfs", search_policy: "ff" },
      members: [
        { remoteId: ids.photos, path: "", mode: UnionUpstreamMode.READ_WRITE },
        { remoteId: ids.media, path: "library", mode: UnionUpstreamMode.READ_ONLY },
      ],
    }),
    create(RemoteSchema, {
      ...base,
      id: ids.storageBox,
      name: "Storage box",
      backendType: "sftp",
      options: { host: "u123456.your-storagebox.de", port: "23", user: "u123456", path: "backups" },
      secretsSet: ["pass"],
    }),
  ];
}

interface JobSeed {
  status: JobStatus;
  ago: number;
  duration?: number;
  worker: string;
  src: [string, string];
  dst: [string, string];
  mode?: RcloneTransferMode;
  bytes?: number;
  total?: number;
  files?: number;
  errors?: number;
  error?: [JobErrorCode, string];
  warnings?: string[];
  owner?: string;
}

const remoteMeta: Record<string, [string, string]> = {
  [ids.minio]: ["MinIO archive", "s3"],
  [ids.storageBox]: ["Storage box", "sftp"],
  [ids.photos]: ["Photos", "local"],
  [ids.b2]: ["B2 offsite", "b2"],
  [ids.nextcloud]: ["Nextcloud", "webdav"],
  [ids.pooled]: ["Pooled disks", "union"],
  [ids.media]: ["Media disk", "local"],
};

function jobId(i: number): string {
  return `c3d2e1f0-7a6b-4c5d-8e9f-${(0xa0b1c2d3e500 + i).toString(16)}`;
}

/** jobs returns about 25 jobs across every status, newest first. */
export function jobs(now: number): Job[] {
  const seeds: JobSeed[] = [
    {
      status: JobStatus.RUNNING,
      ago: 6 * MIN,
      worker: ids.nas,
      src: [ids.photos, "2026"],
      dst: [ids.b2, "photos/2026"],
      mode: RcloneTransferMode.SYNC,
      bytes: 18.4 * GiB,
      total: 41.2 * GiB,
      files: 2841,
      errors: 0,
    },
    {
      status: JobStatus.QUEUED,
      ago: 1 * MIN,
      worker: ids.office,
      src: ["", "D:\\Shared\\Projects"],
      dst: [ids.nextcloud, "Projects"],
    },
    {
      status: JobStatus.DISPATCHED,
      ago: 40_000,
      worker: ids.backupHost,
      src: [ids.storageBox, "db-dumps"],
      dst: ["", "/srv/backup/db"],
      mode: RcloneTransferMode.COPY,
    },
    {
      status: JobStatus.SUCCEEDED,
      ago: 52 * MIN,
      duration: 14 * MIN,
      worker: ids.nas,
      src: [ids.minio, "archive/2025"],
      dst: [ids.storageBox, "archive"],
      bytes: 6.2 * GiB,
      total: 6.2 * GiB,
      files: 1204,
    },
    {
      status: JobStatus.FAILED,
      ago: 2 * HOUR,
      duration: 40_000,
      worker: ids.nas,
      src: [ids.nextcloud, "Documents"],
      dst: [ids.minio, "docs"],
      bytes: 12 * MiB,
      files: 31,
      errors: 3,
      error: [
        JobErrorCode.EXECUTION_FAILED,
        "rclone exited with status 1: 3 errors: failed to open source object: 401 Unauthorized",
      ],
      warnings: ["Retrying after a low-level error (attempt 2 of 10)."],
    },
    {
      status: JobStatus.SUCCEEDED,
      ago: 3 * HOUR,
      duration: 3 * MIN,
      worker: ids.backupHost,
      src: [ids.storageBox, "db-dumps"],
      dst: ["", "/srv/backup/db"],
      bytes: 820 * MiB,
      total: 820 * MiB,
      files: 14,
    },
    {
      status: JobStatus.INTERRUPTED,
      ago: 4 * HOUR,
      duration: 22 * MIN,
      worker: ids.office,
      src: ["", "C:\\Users\\dana\\Documents"],
      dst: [ids.b2, "office/documents"],
      bytes: 1.1 * GiB,
      total: 3.4 * GiB,
      files: 402,
      error: [
        JobErrorCode.WORKER_LOST,
        "The worker stopped reporting and no longer holds the job.",
      ],
    },
    {
      status: JobStatus.CANCELLED,
      ago: 5 * HOUR,
      duration: 2 * MIN,
      worker: ids.nas,
      src: [ids.pooled, ""],
      dst: [ids.b2, "pool"],
      mode: RcloneTransferMode.SYNC,
      bytes: 340 * MiB,
      total: 96 * GiB,
      files: 22,
      error: [JobErrorCode.CANCELLED, "Cancelled by Dana Kowalski."],
    },
    {
      status: JobStatus.SUCCEEDED,
      ago: 9 * HOUR,
      duration: 47 * MIN,
      worker: ids.nas,
      src: [ids.photos, "2025"],
      dst: [ids.b2, "photos/2025"],
      mode: RcloneTransferMode.SYNC,
      bytes: 28.9 * GiB,
      total: 28.9 * GiB,
      files: 6120,
      warnings: ["2 files changed during upload and were retried."],
    },
    {
      status: JobStatus.SUCCEEDED,
      ago: 13 * HOUR,
      duration: 6 * MIN,
      worker: ids.backupHost,
      src: [ids.minio, "configs"],
      dst: ["", "/srv/backup/configs"],
      bytes: 74 * MiB,
      total: 74 * MiB,
      files: 913,
    },
    {
      status: JobStatus.FAILED,
      ago: 20 * HOUR,
      duration: 5_000,
      worker: ids.office,
      src: ["", "E:\\Scans"],
      dst: [ids.nextcloud, "Scans"],
      error: [JobErrorCode.PATH_NOT_ALLOWED, "E:\\Scans is outside the worker's allowed folders."],
    },
    {
      status: JobStatus.SUCCEEDED,
      ago: 1 * DAY + 2 * HOUR,
      duration: 12 * MIN,
      worker: ids.nas,
      src: [ids.media, "music"],
      dst: [ids.storageBox, "music"],
      mode: RcloneTransferMode.COPY,
      bytes: 4.6 * GiB,
      total: 4.6 * GiB,
      files: 3388,
    },
    {
      status: JobStatus.SUCCEEDED,
      ago: 1 * DAY + 9 * HOUR,
      duration: 2 * MIN,
      worker: ids.backupHost,
      src: [ids.storageBox, "db-dumps"],
      dst: ["", "/srv/backup/db"],
      bytes: 801 * MiB,
      total: 801 * MiB,
      files: 14,
      owner: ids.alice,
    },
    {
      status: JobStatus.FAILED,
      ago: 2 * DAY,
      duration: 0,
      worker: ids.nas,
      src: [ids.b2, "photos"],
      dst: ["", "/srv/photos/restore"],
      error: [JobErrorCode.DISPATCH_TIMEOUT, "The worker did not accept the job in time."],
    },
    {
      status: JobStatus.SUCCEEDED,
      ago: 2 * DAY + 4 * HOUR,
      duration: 51 * MIN,
      worker: ids.nas,
      src: [ids.photos, "2024"],
      dst: [ids.b2, "photos/2024"],
      mode: RcloneTransferMode.SYNC,
      bytes: 31.7 * GiB,
      total: 31.7 * GiB,
      files: 7002,
    },
    {
      status: JobStatus.SUCCEEDED,
      ago: 3 * DAY,
      duration: 9 * MIN,
      worker: ids.office,
      src: ["", "D:\\Shared\\Invoices"],
      dst: [ids.nextcloud, "Invoices"],
      bytes: 512 * MiB,
      total: 512 * MiB,
      files: 860,
    },
  ];
  // A long tail of routine nightly copies fills the first page.
  for (let d = 3; d < 13; d++) {
    seeds.push({
      status: d === 7 ? JobStatus.FAILED : JobStatus.SUCCEEDED,
      ago: d * DAY + 2 * HOUR,
      duration: (8 + (d % 5) * 3) * MIN,
      worker: ids.nas,
      src: [ids.minio, "archive"],
      dst: [ids.storageBox, "archive"],
      bytes: (1.2 + (d % 4) * 0.4) * GiB,
      total: (1.2 + (d % 4) * 0.4) * GiB,
      files: 200 + d * 17,
      ...(d === 7
        ? {
            error: [
              JobErrorCode.TEMPORARY,
              "Connection reset by peer while uploading archive/2019/raw-0412.tar.",
            ] as [JobErrorCode, string],
          }
        : {}),
    });
  }

  return seeds.map((s, i) => {
    const created = now - s.ago;
    const started = s.status === JobStatus.QUEUED ? undefined : created + 1500;
    const running = s.status === JobStatus.RUNNING;
    const finished = s.duration !== undefined && started ? started + s.duration : undefined;
    const refs = [s.src[0], s.dst[0]].filter(Boolean);
    const elapsed = running ? s.ago : (s.duration ?? 0);
    return create(JobSchema, {
      id: running ? ids.runningJob : jobId(i),
      workerId: s.worker,
      kind: "rclone_transfer",
      status: s.status,
      ownerId: s.owner ?? ids.admin,
      createdAt: ts(created),
      dispatchedAt: started ? ts(started - 500) : undefined,
      startedAt: started && s.status !== JobStatus.DISPATCHED ? ts(started) : undefined,
      finishedAt: finished ? ts(finished) : undefined,
      stats:
        s.bytes !== undefined || s.files !== undefined
          ? {
              bytes: BigInt(Math.round(s.bytes ?? 0)),
              totalBytes: BigInt(Math.round(s.total ?? 0)),
              files: BigInt(s.files ?? 0),
              totalFiles: BigInt(
                Math.round((s.files ?? 0) * ((s.total ?? 0) / Math.max(1, s.bytes ?? 1))),
              ),
              checks: BigInt(Math.round((s.files ?? 0) * 0.4)),
              errors: BigInt(s.errors ?? 0),
              elapsed: durationFromMs(elapsed),
            }
          : undefined,
      error: s.error ? { code: s.error[0], message: s.error[1] } : undefined,
      warnings: s.warnings ?? [],
      exitCode:
        s.status === JobStatus.FAILED && s.error?.[0] === JobErrorCode.EXECUTION_FAILED
          ? 1
          : undefined,
      definition: {
        operation: {
          case: "rcloneTransfer",
          value: {
            mode: s.mode ?? RcloneTransferMode.COPY,
            source: { remoteId: s.src[0], path: s.src[1] },
            destination: { remoteId: s.dst[0], path: s.dst[1] },
            options: { transfers: 4, checksum: s.mode === RcloneTransferMode.SYNC, filters: [] },
          },
        },
      },
      remotes: refs.map((id) => ({ id, name: remoteMeta[id][0], backendType: remoteMeta[id][1] })),
    });
  });
}

/**
 * samples returns progress samples every 5 s for a job that started
 * `elapsedMs` ago and reached `bytes`, with a believable, uneven rate.
 */
export function samples(now: number, elapsedMs: number, bytes: number): JobProgress[] {
  const n = Math.max(2, Math.floor(elapsedMs / 5000));
  const start = now - elapsedMs;
  const weights: number[] = [];
  for (let i = 0; i < n; i++) {
    const wave = 1 + 0.35 * Math.sin(i / 4) + 0.2 * Math.sin(i / 1.7);
    const dip = i > n * 0.55 && i < n * 0.62 ? 0.25 : 1;
    weights.push(Math.max(0.05, wave * dip));
  }
  const sum = weights.reduce((a, b) => a + b, 0);
  let acc = 0;
  return weights.map((w, i) => {
    acc += (w / sum) * bytes;
    return create(JobProgressSchema, {
      time: ts(start + (i + 1) * 5000),
      stats: { bytes: BigInt(Math.round(acc)), files: BigInt(Math.round((acc / bytes) * 2841)) },
    });
  });
}

/** installCommands renders commands like the controller's install.go. */
export function installCommands(
  token: string,
  url = "https://syncr.example.net",
): InstallCommand[] {
  const base = "https://github.com/Zeptiny/Syncr/releases/latest/download";
  const linux = (user: boolean) =>
    [
      `curl -fsSL "${base}/syncr-worker_linux_$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/').tar.gz" | tar -xz syncr-worker \\`,
      user
        ? `  && install -D -m 0755 syncr-worker ~/.local/bin/syncr-worker \\`
        : `  && sudo install -m 0755 syncr-worker /usr/local/bin/syncr-worker \\`,
      user
        ? `  && ~/.local/bin/syncr-worker setup --user --controller '${url}' --token '${token}'`
        : `  && sudo syncr-worker setup --controller '${url}' --token '${token}'`,
    ].join("\n");
  const windows = (user: boolean) =>
    [
      `$d = ${user ? '"$env:LOCALAPPDATA\\Programs\\Syncr"' : '"$env:ProgramFiles\\Syncr"'}`,
      `Invoke-WebRequest "${base}/syncr-worker_windows_amd64.zip" -OutFile "$env:TEMP\\syncr-worker.zip"`,
      `Expand-Archive "$env:TEMP\\syncr-worker.zip" -DestinationPath $d -Force`,
      `& "$d\\syncr-worker.exe" setup${user ? " --user" : ""} --controller '${url}' --token '${token}'`,
    ].join("\n");
  const note = "Binary already installed? Run only the last line.";
  return [
    create(InstallCommandSchema, {
      platform: InstallPlatform.LINUX,
      scope: InstallScope.SYSTEM,
      shell: "sh",
      title: "Install as a system service",
      command: linux(false),
      note,
    }),
    create(InstallCommandSchema, {
      platform: InstallPlatform.LINUX,
      scope: InstallScope.USER,
      shell: "sh",
      title: "Install for the current user",
      command: linux(true),
      note,
    }),
    create(InstallCommandSchema, {
      platform: InstallPlatform.WINDOWS,
      scope: InstallScope.SYSTEM,
      shell: "powershell",
      title: "In an elevated PowerShell",
      command: windows(false),
      note,
    }),
    create(InstallCommandSchema, {
      platform: InstallPlatform.WINDOWS,
      scope: InstallScope.USER,
      shell: "powershell",
      title: "In PowerShell, for the current user",
      command: windows(true),
      note,
    }),
  ];
}

/** passiveCommands renders the passive-worker install commands. */
export function passiveCommands(): InstallCommand[] {
  const base = "https://github.com/Zeptiny/Syncr/releases/latest/download";
  return [
    create(InstallCommandSchema, {
      platform: InstallPlatform.LINUX,
      scope: InstallScope.SYSTEM,
      shell: "sh",
      title: "Install and start in passive mode",
      command: [
        `curl -fsSL "${base}/syncr-worker_linux_$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/').tar.gz" | tar -xz syncr-worker \\`,
        `  && sudo install -m 0755 syncr-worker /usr/local/bin/syncr-worker \\`,
        `  && sudo syncr-worker setup --passive --listen :7443 --advertise-url 'https://<this-host>:7443'`,
      ].join("\n"),
      note: "It prints the URL, fingerprint and pairing secret to enter below.",
    }),
    create(InstallCommandSchema, {
      platform: InstallPlatform.WINDOWS,
      scope: InstallScope.SYSTEM,
      shell: "powershell",
      title: "In an elevated PowerShell",
      command: [
        `$d = "$env:ProgramFiles\\Syncr"`,
        `Invoke-WebRequest "${base}/syncr-worker_windows_amd64.zip" -OutFile "$env:TEMP\\syncr-worker.zip"`,
        `Expand-Archive "$env:TEMP\\syncr-worker.zip" -DestinationPath $d -Force`,
        `New-NetFirewallRule -DisplayName "Syncr worker" -Direction Inbound -Protocol TCP -LocalPort 7443 -Action Allow`,
        `& "$d\\syncr-worker.exe" setup --passive --listen :7443 --advertise-url 'https://<this-host>:7443'`,
      ].join("\n"),
      note: "It prints the URL, fingerprint and pairing secret to enter below.",
    }),
  ];
}

/** apiTokens returns the admin's API tokens. */
export function apiTokens(now: number): APIToken[] {
  return [
    create(APITokenSchema, {
      id: "7f6e5d4c-3b2a-4190-8f7e-6d5c4b3a2901",
      name: "Home Assistant",
      prefix: "syncr_pat_Q3vX",
      createdAt: ts(now - 18 * DAY),
      lastUsedAt: ts(now - 25 * MIN),
    }),
    create(APITokenSchema, {
      id: "7f6e5d4c-3b2a-4190-8f7e-6d5c4b3a2902",
      name: "Nightly script",
      prefix: "syncr_pat_m81K",
      createdAt: ts(now - 6 * DAY),
      expiresAt: ts(now + 84 * DAY),
    }),
  ];
}

// ---------------------------------------------------------------------------
// Milestone 4: notification channels, contacts, schedules, their runs and
// jobs, and deliveries. Names match what the integration pass seeds
// (internal/devseed), so screenshot scripts work against both.

/** channels are the notification channel descriptors (internal/adapters/notify). */
export const channels: ChannelType[] = [
  create(ChannelTypeSchema, {
    id: "discord",
    label: "Discord",
    description: "Post to a Discord channel through a webhook.",
    fields: [
      f("webhook_url", "Webhook URL", FieldKind.STRING, {
        required: true,
        secret: true,
        placeholder: "https://discord.com/api/webhooks/…",
        help: "In Discord: channel settings, Integrations, Webhooks, Copy webhook URL.",
      }),
      f("username", "Post as", FieldKind.STRING, {
        advanced: true,
        placeholder: "Syncr",
        help: "The name the messages appear under.",
      }),
    ],
  }),
  create(ChannelTypeSchema, {
    id: "email",
    label: "Email",
    description: "Send mail through your SMTP server.",
    fields: [
      f("host", "SMTP server", FieldKind.STRING, {
        required: true,
        placeholder: "smtp.example.com",
      }),
      f("port", "Port", FieldKind.INT, { defaultValue: "587" }),
      f("security", "Encryption", FieldKind.ENUM, {
        defaultValue: "starttls",
        choices: [
          { value: "starttls", label: "STARTTLS (required)" },
          { value: "tls", label: "TLS from the start" },
          { value: "none", label: "None" },
        ],
      }),
      f("username", "Username", FieldKind.STRING),
      f("password", "Password", FieldKind.STRING, { secret: true }),
      f("from", "From", FieldKind.STRING, {
        required: true,
        placeholder: "Syncr <syncr@example.com>",
      }),
      f("to", "To", FieldKind.STRING, {
        required: true,
        placeholder: "ops@example.com, me@example.com",
        help: "Up to 20 addresses, separated by commas.",
      }),
      f("helo_name", "HELO name", FieldKind.STRING, {
        advanced: true,
        help: "The host name to greet the server with. Defaults to the controller's.",
      }),
    ],
  }),
  create(ChannelTypeSchema, {
    id: "webhook",
    label: "Webhook",
    description: "POST JSON to any URL: Home Assistant, ntfy, Slack, Mattermost and more.",
    fields: [
      f("url", "URL", FieldKind.STRING, {
        required: true,
        secret: true,
        placeholder: "https://example.com/hooks/syncr",
        help: "Kept secret, because URLs often carry a token.",
      }),
      f("signing_secret", "Signing secret", FieldKind.STRING, {
        secret: true,
        help: "Signs each request (Standard Webhooks), so the receiver can check it came from Syncr.",
      }),
      f("format", "Body format", FieldKind.ENUM, {
        defaultValue: "syncr",
        choices: [
          { value: "syncr", label: "Syncr JSON" },
          { value: "slack", label: "Slack-compatible text" },
        ],
      }),
      f("authorization", "Authorization header", FieldKind.STRING, {
        secret: true,
        advanced: true,
        placeholder: "Bearer …",
      }),
    ],
  }),
];

/** contacts returns the admin's notification contacts, by name. */
export function contacts(now: number): Contact[] {
  const base = { createdAt: ts(now - 20 * DAY), updatedAt: ts(now - 2 * DAY), version: 2n };
  return [
    create(ContactSchema, {
      ...base,
      id: ids.adminMail,
      name: "Admin mail",
      channelType: "email",
      options: {
        host: "smtp.example.net",
        port: "587",
        security: "starttls",
        username: "syncr@example.net",
        from: "Syncr <syncr@example.net>",
        to: "dana@example.net, ops@example.net",
      },
      secretsSet: ["password"],
      summary: "smtp.example.net → 2 recipients",
      enabled: true,
    }),
    create(ContactSchema, {
      ...base,
      id: ids.homeAssistant,
      name: "Home Assistant",
      channelType: "webhook",
      options: { format: "syncr" },
      secretsSet: ["url", "signing_secret"],
      summary: "Signed webhook, Syncr JSON",
      enabled: true,
      jobEvents: [NotificationEvent.FAILED],
    }),
    create(ContactSchema, {
      ...base,
      id: ids.opsDiscord,
      name: "Ops Discord",
      channelType: "discord",
      options: { username: "Syncr" },
      secretsSet: ["webhook_url"],
      summary: "discord.com webhook",
      enabled: true,
    }),
  ];
}

const H = HOUR;

interface ScheduleSeed {
  id: string;
  name: string;
  description: string;
  cron: string;
  timezone: string;
  target: { case: "workerId" | "workerLabel"; value: string };
  missed: [MissedRunAction, number | undefined];
  overlap: OverlapPolicy;
  enabled: boolean;
  pausedReason?: string;
  contacts: [string, NotificationEvent[]][];
  src: [string, string];
  dst: [string, string];
  mode: RcloneTransferMode;
  created: number;
}

function scheduleSeeds(now: number): ScheduleSeed[] {
  return [
    {
      id: ids.nightly,
      name: "Nightly photos → B2",
      description: "Mirror this year's photo library to Backblaze every night.",
      cron: "30 2 * * *",
      timezone: "Europe/Berlin",
      target: { case: "workerLabel", value: "nas" },
      missed: [MissedRunAction.RUN_LATE, 6 * H],
      overlap: OverlapPolicy.SKIP,
      enabled: true,
      contacts: [
        [
          ids.opsDiscord,
          [NotificationEvent.FAILED, NotificationEvent.INTERRUPTED, NotificationEvent.MISSED],
        ],
      ],
      src: [ids.photos, "2026"],
      dst: [ids.b2, "photos/2026"],
      mode: RcloneTransferMode.SYNC,
      created: now - 32 * DAY,
    },
    {
      id: ids.officeHourly,
      name: "Office docs hourly",
      description: "Copy the office documents to B2 while the desktop is on.",
      cron: "0 * * * *",
      timezone: "Europe/Berlin",
      target: { case: "workerId", value: ids.office },
      missed: [MissedRunAction.RUN_LATE, 0],
      overlap: OverlapPolicy.SKIP,
      enabled: true,
      contacts: [[ids.adminMail, [NotificationEvent.FAILED, NotificationEvent.MISSED]]],
      src: ["", "C:\\Users\\dana\\Documents"],
      dst: [ids.b2, "office/documents"],
      mode: RcloneTransferMode.COPY,
      created: now - 2 * DAY,
    },
    {
      id: ids.weekly,
      name: "Weekly archive",
      description: "",
      cron: "0 3 * * 0",
      timezone: "Europe/Berlin",
      target: { case: "workerId", value: ids.nas },
      missed: [MissedRunAction.RUN_LATE, 24 * H],
      overlap: OverlapPolicy.QUEUE,
      enabled: true,
      contacts: [[ids.adminMail, [NotificationEvent.SUCCEEDED, NotificationEvent.FAILED]]],
      src: [ids.minio, "archive"],
      dst: [ids.storageBox, "archive"],
      mode: RcloneTransferMode.COPY,
      created: now - 40 * DAY,
    },
    {
      id: ids.offsite,
      name: "Offsite mirror",
      description: "Pull the database dumps to the offsite box four times a day.",
      cron: "15 */6 * * *",
      timezone: "UTC",
      target: { case: "workerLabel", value: "offsite" },
      missed: [MissedRunAction.SKIP, undefined],
      overlap: OverlapPolicy.SKIP,
      enabled: true,
      contacts: [
        [
          ids.homeAssistant,
          [NotificationEvent.FAILED, NotificationEvent.INTERRUPTED, NotificationEvent.MISSED],
        ],
      ],
      src: [ids.storageBox, "db-dumps"],
      dst: ["", "/srv/backup/db"],
      mode: RcloneTransferMode.COPY,
      created: now - 35 * DAY,
    },
    {
      id: ids.laptopSync,
      name: "Laptop projects",
      description: "",
      cron: "0 20 * * 1-5",
      timezone: "Europe/Berlin",
      target: { case: "workerId", value: ids.removedLaptop },
      missed: [MissedRunAction.RUN_LATE, 12 * H],
      overlap: OverlapPolicy.SKIP,
      enabled: false,
      pausedReason: "worker_removed",
      contacts: [],
      src: ["", "/home/dana/Projects"],
      dst: [ids.nextcloud, "Projects"],
      mode: RcloneTransferMode.SYNC,
      created: now - 26 * DAY,
    },
  ];
}

function definitionOf(s: {
  src: [string, string];
  dst: [string, string];
  mode: RcloneTransferMode;
}) {
  return {
    operation: {
      case: "rcloneTransfer" as const,
      value: {
        mode: s.mode,
        source: { remoteId: s.src[0], path: s.src[1] },
        destination: { remoteId: s.dst[0], path: s.dst[1] },
        options: { transfers: 4, checksum: s.mode === RcloneTransferMode.SYNC, filters: [] },
      },
    },
  };
}

function remoteRefs(...idList: string[]) {
  return idList
    .filter(Boolean)
    .map((id) => ({ id, name: remoteMeta[id][0], backendType: remoteMeta[id][1] }));
}

/** History is the scheduled side of the dev seed: schedules, runs and jobs. */
export interface History {
  schedules: Schedule[];
  runs: ScheduleRun[];
  jobs: Job[];
}

// rand is a small deterministic generator, so fixtures look the same on
// every load and in tests.
function rand(seed: number): () => number {
  let x = seed >>> 0 || 1;
  return () => {
    x ^= x << 13;
    x ^= x >>> 17;
    x ^= x << 5;
    return (x >>> 0) / 0x1_0000_0000;
  };
}

function historyId(prefix: string, n: number): string {
  // The first segment varies, so short IDs (its first 8 hex digits) differ.
  const head = ((parseInt(prefix, 16) + n * 0x9e3779b1) >>> 0).toString(16).padStart(8, "0");
  return `${head}-7a6b-4c5d-8e9f-${n.toString(16).padStart(12, "0")}`;
}

/**
 * history returns the schedules with about 30 days of runs and the jobs they
 * launched: mostly successes, some missed runs (an offline worker, an
 * overlap), a few failures, an open run on the offline desktop that
 * coalesced earlier occurrences, and a paused schedule whose worker was
 * removed.
 */
export function history(now: number): History {
  const runs: ScheduleRun[] = [];
  const jobList: Job[] = [];
  const schedules: Schedule[] = [];
  const r = rand(4242);
  let runN = 0;
  let jobN = 0;
  const contactNames: Record<string, [string, string]> = {
    [ids.opsDiscord]: ["Ops Discord", "discord"],
    [ids.adminMail]: ["Admin mail", "email"],
    [ids.homeAssistant]: ["Home Assistant", "webhook"],
  };

  const addJob = (
    s: ScheduleSeed,
    at: number,
    worker: string,
    status: JobStatus,
    size: { bytes: number; files: number; minutes: number },
    error?: [JobErrorCode, string],
  ): Job => {
    const started = at + 3_000 + Math.round(r() * 20_000);
    const duration = Math.round(Math.max(20_000, size.minutes * MIN * (0.7 + r() * 0.6)));
    const finished = started + duration;
    const bytes = Math.round(size.bytes * (0.6 + r() * 0.8));
    const files = Math.round(size.files * (0.6 + r() * 0.8));
    const partial = status === JobStatus.SUCCEEDED ? 1 : 0.3 + r() * 0.4;
    const j = create(JobSchema, {
      id: historyId("d4e5f6a7", ++jobN),
      workerId: worker,
      kind: "rclone_transfer",
      status,
      ownerId: ids.admin,
      createdAt: ts(at + 1_000),
      dispatchedAt: ts(started - 500),
      startedAt: ts(started),
      finishedAt: ts(finished),
      stats: {
        bytes: BigInt(Math.round(bytes * partial)),
        totalBytes: BigInt(bytes),
        files: BigInt(Math.round(files * partial)),
        totalFiles: BigInt(files),
        checks: BigInt(files * 3),
        errors: status === JobStatus.FAILED ? 2n : 0n,
        elapsed: durationFromMs(duration),
      },
      error: error ? { code: error[0], message: error[1] } : undefined,
      definition: definitionOf(s),
      remotes: remoteRefs(s.src[0], s.dst[0]),
      scheduleId: s.id,
      schedule: { id: s.id, name: s.name },
    });
    jobList.push(j);
    return j;
  };

  const addRun = (
    s: ScheduleSeed,
    at: number,
    status: ScheduleRunStatus,
    extra: Partial<{
      reason: string;
      message: string;
      worker: string;
      job: Job;
      coalesced: number;
      deadline: number;
      trigger: ScheduleRunTrigger;
    }> = {},
  ) => {
    runs.push(
      create(ScheduleRunSchema, {
        id: historyId("e6f7a8b9", ++runN),
        scheduleId: s.id,
        trigger: extra.trigger ?? ScheduleRunTrigger.CRON,
        scheduledFor: ts(at),
        deadline: extra.deadline ? ts(extra.deadline) : undefined,
        coalesced: extra.coalesced ?? 0,
        status,
        reason: extra.reason ?? "",
        message: extra.message ?? "",
        workerId: extra.job?.workerId ?? extra.worker ?? "",
        jobId: extra.job?.id ?? "",
        jobStatus: extra.job?.status ?? JobStatus.UNSPECIFIED,
        createdAt: ts(at),
        updatedAt: ts(extra.job ? at + 2_000 : at),
      }),
    );
  };

  for (const s of scheduleSeeds(now)) {
    const since = Math.max(s.created, now - 30 * DAY);
    const past = previousRuns(s.cron, s.timezone, now, 400, now - since);
    switch (s.id) {
      case ids.nightly:
        past.forEach((at, i) => {
          if (i === 4) {
            addRun(s, at, ScheduleRunStatus.MISSED, {
              reason: "worker_offline",
              message: "No worker labelled nas came online within 6 hours.",
            });
            return;
          }
          const failed = i === 11;
          const job = addJob(
            s,
            at,
            ids.nas,
            failed ? JobStatus.FAILED : JobStatus.SUCCEEDED,
            { bytes: 3.2 * GiB, files: 420, minutes: 38 },
            failed
              ? [
                  JobErrorCode.EXECUTION_FAILED,
                  "rclone exited with status 1: 2 errors: 503 Service Unavailable from B2",
                ]
              : undefined,
          );
          addRun(s, at, ScheduleRunStatus.LAUNCHED, { job });
        });
        break;
      case ids.officeHourly: {
        // The desktop went offline three hours ago: the latest occurrence
        // waits, with the two before it merged into it.
        const hourly = past.slice(0, 40);
        hourly.forEach((at, i) => {
          if (i === 0) {
            addRun(s, at, ScheduleRunStatus.PENDING, {
              reason: "worker_offline",
              coalesced: 2,
              deadline: at + HOUR,
              worker: ids.office,
            });
            return;
          }
          if (i < 3) return;
          if (i === 15 || i === 16) {
            addRun(s, at, ScheduleRunStatus.MISSED, {
              reason: "worker_offline",
              message: "office-pc stayed offline until the next run was due.",
            });
            return;
          }
          const job = addJob(s, at, ids.office, JobStatus.SUCCEEDED, {
            bytes: 60 * MiB,
            files: 18,
            minutes: 1.5,
          });
          addRun(s, at, ScheduleRunStatus.LAUNCHED, { job });
        });
        break;
      }
      case ids.weekly:
        past.forEach((at, i) => {
          if (i === 2) {
            addRun(s, at, ScheduleRunStatus.MISSED, {
              reason: "overlap",
              message: "The previous archive copy was still running after 24 hours.",
            });
            return;
          }
          const job = addJob(s, at, ids.nas, JobStatus.SUCCEEDED, {
            bytes: 26 * GiB,
            files: 5200,
            minutes: 70,
          });
          addRun(s, at, ScheduleRunStatus.LAUNCHED, { job });
        });
        break;
      case ids.offsite:
        past.forEach((at, i) => {
          if (i === 1 || i === 23 || i === 57 || i === 58 || i === 90) {
            addRun(s, at, ScheduleRunStatus.MISSED, {
              reason: i === 23 ? "no_worker_with_label" : "worker_offline",
              message:
                i === 23
                  ? "No worker had the label offsite."
                  : "backup-host was offline at 5 minutes past the run.",
            });
            return;
          }
          const interrupted = i === 9;
          const job = addJob(
            s,
            at,
            ids.backupHost,
            interrupted ? JobStatus.INTERRUPTED : JobStatus.SUCCEEDED,
            { bytes: 810 * MiB, files: 14, minutes: 3 },
            interrupted
              ? [
                  JobErrorCode.WORKER_LOST,
                  "The worker stopped reporting and no longer holds the job.",
                ]
              : undefined,
          );
          addRun(s, at, ScheduleRunStatus.LAUNCHED, { job });
        });
        break;
      case ids.laptopSync:
        past.forEach((at, i) => {
          if (i === 0) return; // paused after this
          if (i === 1) {
            addRun(s, at, ScheduleRunStatus.FAILED, {
              reason: "worker_removed",
              message: "The worker laptop was removed.",
            });
            return;
          }
          if (i > 9) return;
          const job = addJob(s, at, ids.removedLaptop, JobStatus.SUCCEEDED, {
            bytes: 240 * MiB,
            files: 310,
            minutes: 4,
          });
          addRun(s, at, ScheduleRunStatus.LAUNCHED, { job });
        });
        break;
    }
    const mine = runs.filter((x) => x.scheduleId === s.id);
    const lastRun = mine[0];
    const nextAt = s.enabled ? next(s.cron, s.timezone, now) : undefined;
    schedules.push(
      create(ScheduleSchema, {
        id: s.id,
        ownerId: ids.admin,
        name: s.name,
        description: s.description,
        cron: s.cron,
        timezone: s.timezone,
        target: { target: s.target },
        missedRun: {
          action: s.missed[0],
          maxDelay: s.missed[1] === undefined ? undefined : durationFromMs(s.missed[1]),
        },
        overlap: s.overlap,
        enabled: s.enabled,
        pausedReason: s.pausedReason ?? "",
        definition: definitionOf(s),
        remotes: remoteRefs(s.src[0], s.dst[0]),
        contacts: s.contacts.map(([id, events]) => ({
          contactId: id,
          events,
          contactName: contactNames[id][0],
          channelType: contactNames[id][1],
        })),
        nextRunAt: nextAt ? ts(nextAt) : undefined,
        lastFiredAt: lastRun?.scheduledFor,
        lastRun,
        createdAt: ts(s.created),
        updatedAt: ts(s.created + DAY),
        version: 3n,
      }),
    );
  }
  runs.sort((a, b) => Number((b.scheduledFor?.seconds ?? 0n) - (a.scheduledFor?.seconds ?? 0n)));
  jobList.sort((a, b) => Number((b.createdAt?.seconds ?? 0n) - (a.createdAt?.seconds ?? 0n)));
  return { schedules, runs, jobs: jobList };
}

/**
 * deliveries returns the notification log of the scheduled history: one
 * per subscribed outcome, all delivered except a retrying email, a webhook
 * that gave up and one being sent.
 */
export function deliveries(now: number, h: History): Delivery[] {
  const out: Delivery[] = [];
  let n = 0;
  const byId = new Map(h.schedules.map((s) => [s.id, s]));
  const eventOf = (run: ScheduleRun): NotificationEvent | undefined => {
    if (run.status === ScheduleRunStatus.MISSED) return NotificationEvent.MISSED;
    if (run.status === ScheduleRunStatus.FAILED) return NotificationEvent.FAILED;
    switch (run.jobStatus) {
      case JobStatus.SUCCEEDED:
        return NotificationEvent.SUCCEEDED;
      case JobStatus.FAILED:
        return NotificationEvent.FAILED;
      case JobStatus.INTERRUPTED:
        return NotificationEvent.INTERRUPTED;
      default:
        return undefined;
    }
  };
  const verb: Partial<Record<NotificationEvent, string>> = {
    [NotificationEvent.SUCCEEDED]: "succeeded",
    [NotificationEvent.FAILED]: "failed",
    [NotificationEvent.INTERRUPTED]: "was interrupted",
    [NotificationEvent.MISSED]: "missed a run",
  };
  for (const run of h.runs) {
    const s = byId.get(run.scheduleId);
    const ev = eventOf(run);
    if (!s || ev === undefined) continue;
    for (const link of s.contacts) {
      if (!link.events.includes(ev)) continue;
      const at = Number(run.scheduledFor?.seconds ?? 0n) * 1000 + (run.jobId ? 40 * MIN : 5 * MIN);
      if (at > now) continue;
      out.push(
        create(DeliverySchema, {
          id: historyId("f7a8b9c0", ++n),
          contactId: link.contactId,
          event: ev,
          status: DeliveryStatus.DELIVERED,
          attempts: 1,
          maxAttempts: 8,
          lastAttemptAt: ts(at),
          deliveredAt: ts(at + 400),
          createdAt: ts(at),
          title: `${s.name} ${verb[ev]}`,
          jobId: run.jobId,
          scheduleId: s.id,
        }),
      );
    }
  }
  out.sort((a, b) => Number((b.createdAt?.seconds ?? 0n) - (a.createdAt?.seconds ?? 0n)));
  // The latest office miss is still retrying; an older webhook gave up.
  const mail = out.find((d) => d.contactId === ids.adminMail);
  if (mail) {
    mail.status = DeliveryStatus.PENDING;
    mail.attempts = 3;
    mail.deliveredAt = undefined;
    mail.nextAttemptAt = ts(now + 4 * MIN);
    mail.lastError = "SMTP 451: 4.7.0 Temporary server error. Please try again later.";
  }
  const hooks = out.filter((d) => d.contactId === ids.homeAssistant);
  const dead = hooks[0];
  if (dead) {
    dead.status = DeliveryStatus.DEAD;
    dead.attempts = 8;
    dead.deliveredAt = undefined;
    dead.lastError = "HTTP 503: Service Unavailable (homeassistant: integration reloading)";
  }
  out.unshift(
    create(DeliverySchema, {
      id: historyId("f7a8b9c0", n + 1),
      contactId: ids.homeAssistant,
      event: NotificationEvent.FAILED,
      status: DeliveryStatus.SENDING,
      attempts: 1,
      maxAttempts: 8,
      lastAttemptAt: ts(now - 4_000),
      createdAt: ts(now - 2 * HOUR),
      title: "Nextcloud → MinIO archive failed",
      jobId: jobId(4),
    }),
  );
  return out;
}
