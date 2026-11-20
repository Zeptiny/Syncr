import type { Backend, Remote } from "@/gen/syncr/v1/remotes_pb";

/**
 * NoChecksumLookup returns the name of a remote whose type keeps no
 * checksums (BackendCapabilities.no_checksum), or undefined for any other
 * remote, a worker's folder ("") or a remote that is not known.
 */
export type NoChecksumLookup = (remoteId: string) => string | undefined;

/**
 * noChecksumLookup builds a NoChecksumLookup from the user's remotes and
 * the backend descriptors. It reads only the capability, never a type ID:
 * any type may declare it.
 */
export function noChecksumLookup(
  remotes: Remote[],
  backends: Map<string, Backend>,
): NoChecksumLookup {
  const byId = new Map(remotes.map((r) => [r.id, r]));
  return (id) => {
    const r = id ? byId.get(id) : undefined;
    return r && backends.get(r.backendType)?.capabilities?.noChecksum ? r.name : undefined;
  };
}

/** noChecksumNames lists, once each, the names of the remotes among ids that keep no checksums. */
export function noChecksumNames(lookup: NoChecksumLookup | undefined, ids: string[]): string[] {
  if (!lookup) return [];
  const names: string[] = [];
  for (const id of ids) {
    const name = lookup(id);
    if (name !== undefined && !names.includes(name)) names.push(name);
  }
  return names;
}

function list(names: string[]): string {
  return names.length < 2
    ? (names[0] ?? "")
    : `${names.slice(0, -1).join(", ")} and ${names.at(-1)}`;
}

/** noChecksumNote says why checksum comparison is not offered. */
export function noChecksumNote(names: string[]): string {
  return names.length > 1
    ? `These remotes keep no checksums (${list(names)}), so checksum comparison is not offered.`
    : `This remote keeps no checksums (${list(names)}), so checksum comparison is not offered.`;
}

/** noChecksumError is the error of a definition that compares checksums with such a remote. */
export function noChecksumError(names: string[]): string {
  return `Checksum comparison is not available: ${list(names)} ${names.length > 1 ? "keep" : "keeps"} no checksums. Choose another way to compare.`;
}

/**
 * checksumServerMessage words the API's refusal of checksums
 * ("is not available: a remote that keeps no checksums") for the control
 * it lands on.
 */
export function checksumServerMessage(message: string): string {
  return message.startsWith("is not available") ? `Checksum comparison ${message}` : message;
}
