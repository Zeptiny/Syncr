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

/** scanErrorText turns a scan's error into a sentence, the busy worker in the UI's words. */
export function scanErrorText(err: unknown): string {
  return isBusy(err) ? BUSY_TEXT : errorMessage(err);
}
