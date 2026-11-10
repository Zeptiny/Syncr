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

/** hasRetentionRule reports whether the policy keeps anything at all. */
export function hasRetentionRule(p: RetentionPolicyState): boolean {
  return (
    retentionCounts.some((c) => Number(p[c.key]) > 0) ||
    p.keepWithin.trim() !== "" ||
    p.keepTags.length > 0
  );
}

/** validRetentionDuration reports whether text is a restic duration (1y6m2d3h). */
export function validRetentionDuration(text: string): boolean {
  return /^(\d+y)?(\d+m)?(\d+d)?(\d+h)?$/.test(text) && text !== "";
}

/**
 * describeRetention says in words what a policy keeps, e.g. "Keeps 7 daily,
 * 4 weekly and 12 monthly snapshots". Every rule adds snapshots to keep; the
 * rest is removed.
 */
export function describeRetention(p: RetentionPolicyState): string {
  const parts = retentionCounts
    .filter((c) => Number(p[c.key]) > 0)
    .map((c) =>
      c.key === "keepLast" ? `the last ${Number(p[c.key])}` : `${Number(p[c.key])} ${c.noun}`,
    );
  const extra: string[] = [];
  if (p.keepWithin.trim()) extra.push(`everything from the last ${p.keepWithin.trim()}`);
  if (p.keepTags.length) extra.push(`snapshots tagged ${p.keepTags.join(", ")}`);
  if (!parts.length && !extra.length) return "Keeps nothing yet: set at least one rule.";
  const list = (items: string[]) =>
    items.length <= 1 ? items.join("") : `${items.slice(0, -1).join(", ")} and ${items.at(-1)}`;
  const counted = parts.length ? [`${list(parts)} snapshots`] : [];
  return `Keeps ${list([...counted, ...extra])}.`;
}
