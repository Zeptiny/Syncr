// What the heavy probes (folder sizes, snapshot search and compare) have in
// common in the UI: the busy worker, and how their errors read.
import { Code, ConnectError } from "@connectrpc/connect";

import { errorMessage } from "./errors";

/** BUSY_TEXT is what the UI says when the worker runs another scan. */
export const BUSY_TEXT = "This worker is already measuring or searching. Try again in a moment.";

/**
 * isBusy reports whether a scan was refused because the worker runs
 * another one: the API answers Unavailable and says "already measuring or
 * searching" (one heavy probe per worker at a time).
 */
export function isBusy(err: unknown): boolean {
  return (
    err instanceof ConnectError &&
    err.code === Code.Unavailable &&
    /already measuring or searching/.test(err.rawMessage)
  );
}

/** SLOTS_BUSY_TEXT is what the UI says when the worker has too many requests at once. */
export const SLOTS_BUSY_TEXT = "This worker is busy with other requests. Try again in a moment.";

/**
 * isSlotsBusy reports whether a scan was refused because the worker's
 * probe slots are all taken (by listings and the like), not by a scan.
 */
export function isSlotsBusy(err: unknown): boolean {
  return (
    err instanceof ConnectError &&
    err.code === Code.Unavailable &&
    /is busy with other requests/.test(err.rawMessage)
  );
}

/** scanErrorText turns a scan's error into a sentence, a busy worker in the UI's words. */
export function scanErrorText(err: unknown): string {
  if (isBusy(err)) return BUSY_TEXT;
  if (isSlotsBusy(err)) return SLOTS_BUSY_TEXT;
  return errorMessage(err);
}
