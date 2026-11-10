// The retention policy of a "forget" job as the form edits it.

/** RetentionPolicyState is a retention policy with numbers kept as typed. */
export interface RetentionPolicyState {
  keepLast: string;
  keepHourly: string;
  keepDaily: string;
  keepWeekly: string;
  keepMonthly: string;
  keepYearly: string;
  /** keepWithin is a restic duration such as 1y6m or 30d. */
  keepWithin: string;
  keepTags: string[];
}

/** retentionCounts are the keep-N rules in the order the form shows them. */
export const retentionCounts: {
  key: Exclude<keyof RetentionPolicyState, "keepWithin" | "keepTags">;
  label: string;
  noun: string;
}[] = [
  { key: "keepLast", label: "Last", noun: "last" },
  { key: "keepHourly", label: "Hourly", noun: "hourly" },
  { key: "keepDaily", label: "Daily", noun: "daily" },
  { key: "keepWeekly", label: "Weekly", noun: "weekly" },
  { key: "keepMonthly", label: "Monthly", noun: "monthly" },
  { key: "keepYearly", label: "Yearly", noun: "yearly" },
];

/** emptyRetention keeps nothing yet: at least one rule must be set. */
export function emptyRetention(): RetentionPolicyState {
  return {
    keepLast: "",
    keepHourly: "",
    keepDaily: "",
    keepWeekly: "",
    keepMonthly: "",
    keepYearly: "",
    keepWithin: "",
    keepTags: [],
  };
}

/** RetentionPreset is a ready-made policy. */
export interface RetentionPreset {
  id: string;
  label: string;
  policy: RetentionPolicyState;
}

/** retentionPresets are the policies the form offers with one click. */
export const retentionPresets: RetentionPreset[] = [
  {
    id: "daily-weekly-monthly",
    label: "7 daily, 4 weekly, 12 monthly",
    policy: { ...emptyRetention(), keepDaily: "7", keepWeekly: "4", keepMonthly: "12" },
  },
  { id: "last-30", label: "Last 30", policy: { ...emptyRetention(), keepLast: "30" } },
  {
    id: "hourly-daily-weekly",
    label: "24 hourly, 7 daily, 4 weekly",
    policy: { ...emptyRetention(), keepHourly: "24", keepDaily: "7", keepWeekly: "4" },
  },
];

/** defaultRetention is the policy a new retention job starts with. */
export function defaultRetention(): RetentionPolicyState {
  return { ...retentionPresets[0].policy, keepTags: [] };
}

const same = (a: RetentionPolicyState, b: RetentionPolicyState) =>
  retentionCounts.every((c) => (a[c.key] || "0") === (b[c.key] || "0")) &&
  a.keepWithin.trim() === b.keepWithin.trim() &&
  a.keepTags.join(",") === b.keepTags.join(",");

/** matchingPreset returns the ID of the preset equal to policy, if any. */
export function matchingPreset(policy: RetentionPolicyState): string | undefined {
  return retentionPresets.find((p) => same(p.policy, policy))?.id;
}

// The limits below are the controller's (internal/jobkind, ResticForget
// and its tag and host checks); the form checks them so their problems show
// next to the fields instead of coming back from the server.

/** MAX_KEEP is the largest keep-N count. */
export const MAX_KEEP = 100000;
/** MAX_RESTIC_TAGS is how many tags a list may have, MAX_TAG_BYTES how long one may be. */
export const MAX_RESTIC_TAGS = 32;
export const MAX_TAG_BYTES = 128;
/** MAX_HOST_BYTES is how long a host name may be. */
export const MAX_HOST_BYTES = 255;

/**
 * KEEP_WITHIN_RE is the --keep-within syntax the controller accepts: one or
 * more numbers from 1 (no leading zero), each followed by y, m, d or h, in
 * any order (1y6m, 2y5m7d3h, 1d1y).
 */
const KEEP_WITHIN_RE = /^([1-9][0-9]*[ymdh])+$/;

/**
 * keepCount reads a keep-N field: 0 when it is empty, NaN when it is not a
 * whole number.
 */
export function keepCount(text: string): number {
  const t = text.trim();
  if (t === "") return 0;
  return /^\d+$/.test(t) ? Number(t) : NaN;
}

/** cleanTags is a tag list as it is sent: trimmed, without empty tags or repeats. */
export function cleanTags(tags: string[]): string[] {
  return [...new Set(tags.map((t) => t.trim()).filter(Boolean))];
}

const utf8Length = (s: string) => new TextEncoder().encode(s).length;

/** hasControl reports a control character (Unicode Cc), as the controller does. */
const hasControl = (s: string) =>
  [...s].some((ch) => {
    const c = ch.codePointAt(0) ?? 0;
    return c < 0x20 || (c >= 0x7f && c < 0xa0);
  });

/** tagsProblem explains why restic tags (cleaned) are refused; undefined when they are fine. */
export function tagsProblem(tags: string[]): string | undefined {
  if (tags.length > MAX_RESTIC_TAGS) return `Use at most ${MAX_RESTIC_TAGS} tags.`;
  for (const t of tags) {
    if (utf8Length(t) > MAX_TAG_BYTES)
      return `The tag “${t}” is too long: a tag may have at most ${MAX_TAG_BYTES} bytes.`;
    if (t.includes(",")) return `The tag “${t}” contains a comma, which restic cannot store.`;
    if (hasControl(t)) return "Tags cannot contain control characters.";
  }
  return undefined;
}

/** hostProblem explains why a host name (trimmed) is refused; undefined when it is fine. */
export function hostProblem(host: string): string | undefined {
  if (utf8Length(host) > MAX_HOST_BYTES)
    return `The host name may have at most ${MAX_HOST_BYTES} bytes.`;
  if (hasControl(host)) return "The host name cannot contain control characters.";
  return undefined;
}

/**
 * hasRetentionRule reports whether the policy keeps anything at all, as the
 * controller counts rules: a keep-N count above zero, a keep-within period
 * or a keep tag (blank tags are dropped before sending, so they do not
 * count).
 */
export function hasRetentionRule(p: RetentionPolicyState): boolean {
  return (
    retentionCounts.some((c) => keepCount(p[c.key]) > 0) ||
    p.keepWithin.trim() !== "" ||
    cleanTags(p.keepTags).length > 0
  );
}

/** validRetentionDuration reports whether text is a keep-within period the controller accepts. */
export function validRetentionDuration(text: string): boolean {
  return KEEP_WITHIN_RE.test(text);
}

/**
 * retentionPolicyProblem explains what the controller would refuse in a
 * policy, in the order it checks: the counts, the keep tags, the period,
 * then whether any rule is left. Undefined when it would accept it.
 */
export function retentionPolicyProblem(p: RetentionPolicyState): string | undefined {
  for (const c of retentionCounts) {
    const n = keepCount(p[c.key]);
    if (!Number.isInteger(n) || n > MAX_KEEP)
      return `Keep ${c.label.toLowerCase()} must be a whole number from 0 to ${MAX_KEEP}.`;
  }
  const tags = tagsProblem(cleanTags(p.keepTags));
  if (tags) return tags;
  const within = p.keepWithin.trim();
  if (within && !validRetentionDuration(within))
    return "Write the period as years, months, days and hours, such as 1y6m, 30d or 12h; each number is at least 1, without leading zeros.";
  if (!hasRetentionRule(p))
    return "Set at least one rule. Without one, every snapshot would be removed.";
  return undefined;
}

/**
 * describeRetention says in words what a policy keeps, e.g. "Keeps 7 daily,
 * 4 weekly and 12 monthly snapshots". Every rule adds snapshots to keep; the
 * rest is removed.
 */
export function describeRetention(p: RetentionPolicyState): string {
  const parts = retentionCounts
    .filter((c) => keepCount(p[c.key]) > 0)
    .map((c) =>
      c.key === "keepLast" ? `the last ${keepCount(p[c.key])}` : `${keepCount(p[c.key])} ${c.noun}`,
    );
  const extra: string[] = [];
  const tags = cleanTags(p.keepTags);
  if (p.keepWithin.trim()) extra.push(`everything from the last ${p.keepWithin.trim()}`);
  if (tags.length) extra.push(`snapshots tagged ${tags.join(", ")}`);
  if (!parts.length && !extra.length) return "Keeps nothing yet: set at least one rule.";
  const list = (items: string[]) =>
    items.length <= 1 ? items.join("") : `${items.slice(0, -1).join(", ")} and ${items.at(-1)}`;
  const counted = parts.length ? [`${list(parts)} snapshots`] : [];
  return `Keeps ${list([...counted, ...extra])}.`;
}
