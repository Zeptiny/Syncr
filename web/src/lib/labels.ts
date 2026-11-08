import type { Worker } from "@/gen/syncr/v1/api_pb";

/** LABEL_RE is the worker label rule (domain.ValidLabel). */
export const LABEL_RE = /^[a-z0-9][a-z0-9_.-]{0,31}$/;

/** MAX_LABELS is how many labels a worker may have. */
export const MAX_LABELS = 16;

/** labelProblem explains why a label is not valid; undefined when it is. */
export function labelProblem(label: string): string | undefined {
  if (!label) return "Enter a label.";
  if (label.length > 32) return "Use at most 32 characters.";
  if (!LABEL_RE.test(label))
    return "Start with a letter or digit; then use letters, digits, dots, dashes and underscores.";
  return undefined;
}

/** normalizeLabels lower-cases, dedupes and sorts labels, like the controller. */
export function normalizeLabels(labels: string[]): string[] {
  return [...new Set(labels.map((l) => l.trim().toLowerCase()).filter(Boolean))].sort();
}

/**
 * addLabel adds the typed text raw to labels as one label (trimmed and
 * lower-cased), returning the normalized labels, or the labels unchanged
 * and the problem when the text is not a valid label or one too many.
 * Empty text adds nothing.
 */
export function addLabel(labels: string[], raw: string): { labels: string[]; problem?: string } {
  const label = raw.trim().toLowerCase();
  if (!label) return { labels };
  const bad = labelProblem(label);
  if (bad) return { labels, problem: `“${label}”: ${bad}` };
  if (!labels.includes(label) && labels.length >= MAX_LABELS)
    return { labels, problem: `A worker can have at most ${MAX_LABELS} labels.` };
  return { labels: normalizeLabels([...labels, label]) };
}

/** labelledWorkers returns the workers that carry a label. */
export function labelledWorkers(workers: Worker[], label: string): Worker[] {
  const l = label.trim().toLowerCase();
  return l ? workers.filter((w) => w.labels.includes(l)) : [];
}
