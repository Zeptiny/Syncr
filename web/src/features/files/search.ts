// URL state of the file browser pages, kept apart from them for code
// splitting. Paths and worker IDs only; never a secret.

const str = (v: unknown) => (typeof v === "string" && v ? v : undefined);

/** WorkerFilesSearch is the folder open in a worker's browser. */
export interface WorkerFilesSearch {
  /** path is a native path on the worker; absent for the list of allowed folders. */
  path?: string;
}

/** validateWorkerFilesSearch parses /workers/$workerId/files' search params. */
export function validateWorkerFilesSearch(s: Record<string, unknown>): WorkerFilesSearch {
  const p = str(s.path)?.trim();
  return { path: p || undefined };
}

/** RemoteFilesSearch is the folder open in a remote's browser and its listing worker. */
export interface RemoteFilesSearch {
  /** path is relative to the remote's root; absent for the root. */
  path?: string;
  /** worker pins the worker that lists; absent lets the controller choose. */
  worker?: string;
}

/** validateRemoteFilesSearch parses /remotes/$remoteId/files' search params. */
export function validateRemoteFilesSearch(s: Record<string, unknown>): RemoteFilesSearch {
  const p = str(s.path)?.replace(/^\/+|\/+$/g, "");
  return { path: p || undefined, worker: str(s.worker) };
}
