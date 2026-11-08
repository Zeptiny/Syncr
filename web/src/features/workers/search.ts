// The workers list's URL state, kept apart from the page for code splitting.

/** WorkersSearch is the workers list's URL state. */
export interface WorkersSearch {
  label?: string;
}

/** validateWorkersSearch parses /workers' search params. */
export function validateWorkersSearch(s: Record<string, unknown>): WorkersSearch {
  return { label: typeof s.label === "string" && s.label ? s.label : undefined };
}
