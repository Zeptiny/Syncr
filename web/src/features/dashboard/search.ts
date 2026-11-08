// The dashboard's URL state, kept apart from the page for code splitting.

/** DashboardRangeKey is the dashboard period in the URL. */
export type DashboardRangeKey = "24h" | "7d" | "30d";

/** DashboardSearch is the dashboard's URL state. */
export interface DashboardSearch {
  range?: DashboardRangeKey;
  all?: boolean;
}

/** validateDashboardSearch parses the dashboard's search params. */
export function validateDashboardSearch(s: Record<string, unknown>): DashboardSearch {
  const r = s.range;
  return {
    range: r === "24h" || r === "7d" || r === "30d" ? r : undefined,
    all: s.all === true || s.all === "true" ? true : undefined,
  };
}
