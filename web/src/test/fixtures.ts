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
import { UserRole, UserSchema, type User } from "@/gen/syncr/v1/users_pb";

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
