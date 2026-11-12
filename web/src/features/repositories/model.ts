// Pure logic of the repository pages: IDs, restore defaults and requests,
// and the delete request. No React here, so it is easy to test.
import type { MessageInitShape } from "@bufbuild/protobuf";

import type { RunJobRequestSchema, Worker } from "@/gen/syncr/v1/api_pb";
import { ResticRestoreOverwrite, type ResticSnapshot } from "@/gen/syncr/v1/link_pb";
import type { DeleteRepositoryRequestSchema, Repository } from "@/gen/syncr/v1/restic_pb";
import { isWindowsOS } from "@/lib/restic-paths";
import { FEATURE_RESTIC, hasFeature, isOnline, sortWorkers } from "@/lib/workers";

import { emptyHooks, toHooks, type HooksState } from "../jobs/hooks";
import { overwriteLabel } from "../jobs/model";

/** shortId is the first eight characters of a snapshot or repository ID. */
export function shortId(id: string): string {
  return id.slice(0, 8);
}

/** isLocalRepository reports whether the repository lives on a worker's own disk. */
export function isLocalRepository(repo: Repository | undefined): boolean {
  return repo?.remote?.backendType === "local";
}

/** locationText writes where a repository is: "B2 offsite: restic/photos". */
export function locationText(repo: Repository): string {
  const remote = repo.remote?.name || "Unknown remote";
  return repo.path ? `${remote}: ${repo.path}` : `${remote} (its root)`;
}

/**
 * defaultRestoreTarget proposes a new folder for a restore:
 * <first allowed folder>/restore-<short id>-<yyyymmdd>, in the worker's
 * path style. Without an allowed folder there is no proposal.
 */
export function defaultRestoreTarget(
  roots: string[],
  snapshotId: string,
  date: Date,
  os: string | undefined,
): string {
  const root = roots[0];
  if (!root) return "";
  const ymd = `${date.getFullYear()}${String(date.getMonth() + 1).padStart(2, "0")}${String(date.getDate()).padStart(2, "0")}`;
  const sep = isWindowsOS(os) ? "\\" : "/";
  const base = root.endsWith(sep) ? root : root + sep;
  return `${base}restore-${shortId(snapshotId)}-${ymd}`;
}

/**
 * pickRestoreWorker chooses the worker a restore starts with: the online
 * worker whose host name is the snapshot's (ignoring case), else the first
 * online worker that supports restic, by name.
 */
export function pickRestoreWorker(workers: Worker[], hostname: string): Worker | undefined {
  const usable = sortWorkers(workers).filter((w) => isOnline(w) && hasFeature(w, FEATURE_RESTIC));
  const host = hostname.trim().toLowerCase();
  return (
    (host ? usable.find((w) => (w.info?.hostname ?? "").toLowerCase() === host) : undefined) ??
    usable[0]
  );
}

/** RestoreWhat is what a restore takes from the snapshot. */
export type RestoreWhat =
  /** Everything, laid out under the target by its original path. */
  | { mode: "snapshot" }
  /** Chosen files and folders, laid out by their original paths. */
  | { mode: "items"; paths: string[] }
  /** One folder's contents, placed directly in the target. */
  | { mode: "folder"; folder: string };

/** RestoreState is the restore sheet's form. */
export interface RestoreState {
  workerId: string;
  target: string;
  overwrite: ResticRestoreOverwrite;
  verify: boolean;
  excludes: string[];
  /**
   * hooks are the worker's hooks to run around the restore: stop what uses
   * the files first, load a restored database dump afterwards.
   */
  hooks: HooksState;
}

/** emptyRestore keeps existing files: a restore never overwrites unless asked. */
export function emptyRestore(): RestoreState {
  return {
    workerId: "",
    target: "",
    overwrite: ResticRestoreOverwrite.NEVER,
    verify: false,
    excludes: [],
    hooks: emptyHooks(),
  };
}

/** whatLabel names what is restored: "3 items", "the folder /srv/x", "the whole snapshot". */
export function whatLabel(what: RestoreWhat): string {
  switch (what.mode) {
    case "items":
      return what.paths.length === 1 ? "1 item" : `${what.paths.length} items`;
    case "folder":
      return `the contents of ${what.folder}`;
    default:
      return "the whole snapshot";
  }
}

/**
 * restoreSummary says in one line what the restore will do, e.g. "Restores
 * 3 items into /srv/restore on nas-01 and keeps existing files".
 */
export function restoreSummary(
  what: RestoreWhat,
  s: Pick<RestoreState, "target" | "overwrite">,
  workerName: string | undefined,
): string {
  const target = s.target.trim() || "a folder you still have to choose";
  const on = workerName ? ` on ${workerName}` : "";
  return `Restores ${whatLabel(what)} into ${target}${on} and ${overwriteLabel(s.overwrite)}.`;
}

/** RestoreErrors are the restore form's problems. */
export type RestoreErrors = Partial<
  Record<"worker" | "target" | "what" | "hooks" | "form", string>
>;

/** validateRestore checks what the browser can check before sending. */
export function validateRestore(what: RestoreWhat, s: RestoreState): RestoreErrors {
  const e: RestoreErrors = {};
  if (!s.workerId) e.worker = "Choose the worker that receives the files.";
  const t = s.target.trim();
  if (!t) e.target = "Choose the folder to restore into.";
  else if (!(t.startsWith("/") || /^[A-Za-z]:[\\/]/.test(t) || t.startsWith("\\\\")))
    e.target = "Enter an absolute path on the worker.";
  if (what.mode === "items" && what.paths.length === 0) e.what = "Select at least one item.";
  return e;
}

/** toRestoreRequest builds the RunJob request of a restore. */
export function toRestoreRequest(
  repositoryId: string,
  snapshot: Pick<ResticSnapshot, "id">,
  what: RestoreWhat,
  s: RestoreState,
): MessageInitShape<typeof RunJobRequestSchema> {
  const hooks = toHooks(s.hooks);
  return {
    workerId: s.workerId,
    definition: {
      ...(hooks ? { hooks } : {}),
      operation: {
        case: "resticRestore",
        value: {
          repositoryId,
          snapshotId: snapshot.id,
          target: s.target.trim(),
          paths: what.mode === "items" ? what.paths : [],
          subfolder: what.mode === "folder" ? what.folder : "",
          excludes: [...new Set(s.excludes.map((x) => x.trim()).filter(Boolean))],
          verify: s.verify,
          // Unset would also mean "keep existing" to the controller; say it outright.
          overwrite:
            s.overwrite === ResticRestoreOverwrite.UNSPECIFIED
              ? ResticRestoreOverwrite.NEVER
              : s.overwrite,
        },
      },
    },
  };
}

/** DeleteChoice is the delete dialog's form. */
export interface DeleteChoice {
  /** deleteData also removes the backup data from the storage. */
  deleteData: boolean;
  /** typedName is the repository name the user typed to confirm. */
  typedName: string;
  /** workerId is the worker that deletes the data; "" lets the controller choose. */
  workerId: string;
}

/**
 * toDeleteRequest builds the DeleteRepository request. Forgetting sends
 * only the ID; deleting the data adds delete_data, the typed name and the
 * chosen worker, and nothing of those is ever sent for a plain forget.
 */
export function toDeleteRequest(
  repo: Pick<Repository, "id">,
  c: DeleteChoice,
): MessageInitShape<typeof DeleteRepositoryRequestSchema> {
  if (!c.deleteData) return { repositoryId: repo.id };
  return {
    repositoryId: repo.id,
    deleteData: true,
    confirmName: c.typedName,
    workerId: c.workerId,
  };
}

/**
 * canDelete reports whether the delete dialog's button may be pressed:
 * always for a plain forget; for the data, only when the typed name is the
 * repository's exactly and, for a repository on a worker's disk, a worker
 * is chosen.
 */
export function canDelete(repo: Repository, c: DeleteChoice): boolean {
  if (!c.deleteData) return true;
  if (c.typedName !== repo.name) return false;
  return !isLocalRepository(repo) || c.workerId !== "";
}

/** deletionFailed reports whether the last attempt to delete the data failed. */
export function deletionFailed(repo: Repository): boolean {
  return !repo.deleting && repo.deletionError !== "";
}

const PASSWORD_ALPHABET = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789";

/**
 * generatePassword returns length random characters from an alphabet
 * without look-alikes, drawn with the browser's secure generator
 * (rejection sampling, so every character is equally likely).
 */
export function generatePassword(length = 24): string {
  const n = PASSWORD_ALPHABET.length;
  const limit = 256 - (256 % n);
  let out = "";
  while (out.length < length) {
    const bytes = crypto.getRandomValues(new Uint8Array(length * 2));
    for (const b of bytes) {
      if (b < limit && out.length < length) out += PASSWORD_ALPHABET[b % n];
    }
  }
  return out;
}
