import { docTarget, type DocTarget } from "@/features/docs/doc-link";

/**
 * remoteTypeHelp maps a backend type ID (Backend.id) to the section of the
 * docs that explains its fields. The targets are checked by the typecheck;
 * the keys are the API's IDs, so a type the controller adds later simply
 * falls back to the page (remoteHelp).
 */
export const remoteTypeHelp: Readonly<Record<string, DocTarget>> = {
  s3: docTarget("remotes", "amazon-s3-and-compatible"),
  b2: docTarget("remotes", "backblaze-b2"),
  sftp: docTarget("remotes", "sftp"),
  webdav: docTarget("remotes", "webdav"),
  local: docTarget("remotes", "local-directory"),
  union: docTarget("remotes", "union"),
  crypt: docTarget("encrypted-remotes"),
  smb: docTarget("remotes", "smb-windows-share"),
  ftp: docTarget("remotes", "ftp--ftps"),
  azureblob: docTarget("remotes", "azure-blob-storage"),
  gcs: docTarget("remotes", "google-cloud-storage"),
};

/** remoteHelp is the docs section for a backend type, or the remotes page for any other. */
export function remoteHelp(type: string | undefined): DocTarget {
  return type !== undefined && Object.hasOwn(remoteTypeHelp, type)
    ? remoteTypeHelp[type]
    : docTarget("remotes");
}
