import { Code, ConnectError } from "@connectrpc/connect";

import { ValidationErrorSchema, type FieldViolation } from "@/gen/syncr/v1/api_pb";

/** fieldViolations returns the ValidationError details of an API error. */
export function fieldViolations(err: unknown): FieldViolation[] {
  if (!(err instanceof ConnectError)) return [];
  return err.findDetails(ValidationErrorSchema).flatMap((d) => d.violations);
}

/** hasCode reports whether err is a ConnectError with the given code. */
export function hasCode(err: unknown, code: Code): boolean {
  return err instanceof ConnectError && err.code === code;
}

/** isUnauthenticated reports whether the session is missing or expired. */
export function isUnauthenticated(err: unknown): boolean {
  return hasCode(err, Code.Unauthenticated);
}

const fallback: Partial<Record<Code, string>> = {
  [Code.Unauthenticated]: "Your session has ended. Sign in again.",
  [Code.PermissionDenied]: "Only administrators can do this.",
  [Code.NotFound]: "It no longer exists, or it isn't yours.",
  [Code.ResourceExhausted]: "Too many attempts. Wait a minute and try again.",
  [Code.Unavailable]: "The controller is unreachable. Check your connection and try again.",
  [Code.DeadlineExceeded]: "The controller took too long to answer. Try again.",
  [Code.Aborted]: "Someone else changed this in the meantime. Reload and try again.",
  [Code.Internal]: "The controller hit an internal error. Its log has the details.",
};

/** errorMessage turns any error into a sentence for display. */
export function errorMessage(err: unknown): string {
  if (err instanceof ConnectError) {
    const raw = err.rawMessage.trim();
    if (raw && err.code !== Code.Internal && err.code !== Code.Unknown) {
      return raw.charAt(0).toUpperCase() + raw.slice(1);
    }
    return fallback[err.code] ?? (raw || "Something went wrong.");
  }
  if (err instanceof Error && err.message) return err.message;
  return "Something went wrong.";
}

/**
 * retryable reports whether a failed query is worth retrying: transient
 * transport failures are, client errors are not.
 */
export function retryable(err: unknown): boolean {
  if (!(err instanceof ConnectError)) return true;
  return [Code.Unavailable, Code.DeadlineExceeded, Code.Unknown, Code.Aborted].includes(err.code);
}
