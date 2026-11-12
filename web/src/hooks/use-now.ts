import { useEffect, useState } from "react";

/**
 * useNow returns the current time, refreshed every intervalMs while active.
 * An inactive clock keeps its last value, so a page that no longer changes
 * (a finished job) stops re-rendering.
 */
export function useNow(intervalMs = 15_000, active = true): Date {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    if (!active) return;
    const id = window.setInterval(() => setNow(new Date()), intervalMs);
    return () => window.clearInterval(id);
  }, [intervalMs, active]);
  return now;
}
