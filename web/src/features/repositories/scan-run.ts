// Whether a scan page (search, compare) runs its heavy probe on opening.
// A URL alone never does: a link someone sent, or a reload, shows the
// filled form and its button. The app's own ways in (the snapshot
// browser, the repository's snapshot list, the page's own form) ask for
// the run in the router's history state, which a link cannot carry.
import { useLocation } from "@tanstack/react-router";
import type { HistoryState } from "@tanstack/react-router";

declare module "@tanstack/history" {
  interface HistoryState {
    /** runScan asks a scan page to run at once (see scan-run.ts). */
    runScan?: string;
  }
}

// The browser keeps history state across a reload; tagging the request
// with this page load makes a reload count as opening the URL anew.
const thisLoad = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;

/** runScan is the history state (for navigate or Link) that asks a scan page to run at once. */
export function runScan(prev: HistoryState): HistoryState {
  return { ...prev, runScan: thisLoad };
}

/** useRunRequested reports whether the app opened this page asking it to run. */
export function useRunRequested(): boolean {
  return useLocation({ select: (l) => l.state.runScan === thisLoad });
}
