// A check of the built bundle: the docs stay out of what the app loads at
// start, the entry chunks and every chunk they import statically. Only the
// /docs routes may load them, through dynamic import().

/** The parts of an output chunk the check reads (Rolldown's OutputChunk). */
export interface BundleChunk {
  type: "chunk";
  fileName: string;
  isEntry: boolean;
  /** The chunks this one imports statically. */
  imports: readonly string[];
  /** The modules rendered into this chunk. */
  moduleIds: readonly string[];
}

/** An output file as the check sees it: a chunk, or an asset it skips. */
export type BundleFile = BundleChunk | { type: "asset" };

/**
 * eagerModules lists, as "<chunk>: <module>", every module matching isDocs
 * in an entry chunk or in a chunk an entry reaches through static imports.
 */
export function eagerModules(
  bundle: Readonly<Record<string, BundleFile>>,
  isDocs: (id: string) => boolean,
): string[] {
  const chunk = (name: string): BundleChunk | undefined => {
    const file = bundle[name];
    return file?.type === "chunk" ? file : undefined;
  };
  const queue = Object.keys(bundle).filter((name) => chunk(name)?.isEntry);
  const seen = new Set(queue);
  const found: string[] = [];
  for (let name = queue.shift(); name !== undefined; name = queue.shift()) {
    const c = chunk(name);
    if (!c) continue;
    for (const id of c.moduleIds)
      if (isDocs(id)) found.push(`${c.fileName}: ${id.replace(/^\0/, "")}`);
    for (const next of c.imports) {
      if (seen.has(next)) continue;
      seen.add(next);
      queue.push(next);
    }
  }
  return found.sort();
}
