import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, it } from "vitest";

import { ValidationErrorSchema } from "@/gen/syncr/v1/api_pb";

import { errorMessage, fieldViolations, retryable } from "./errors";

describe("errors", () => {
  it("reads ValidationError details", () => {
    const err = new ConnectError("invalid", Code.InvalidArgument, undefined, [
      { desc: ValidationErrorSchema, value: { violations: [{ field: "name", message: "taken" }] } },
    ]);
    expect(fieldViolations(err).map((v) => [v.field, v.message])).toEqual([["name", "taken"]]);
    expect(fieldViolations(new Error("x"))).toEqual([]);
  });

  it("turns errors into sentences", () => {
    expect(errorMessage(new ConnectError("remote is in use", Code.FailedPrecondition))).toBe(
      "Remote is in use",
    );
    expect(errorMessage(new ConnectError("", Code.PermissionDenied))).toBe(
      "Only administrators can do this.",
    );
    expect(errorMessage(new ConnectError("stack trace", Code.Internal))).toMatch(/internal error/);
  });

  it("retries only transient failures", () => {
    expect(retryable(new ConnectError("x", Code.Unavailable))).toBe(true);
    expect(retryable(new ConnectError("x", Code.Unauthenticated))).toBe(false);
    expect(retryable(new ConnectError("x", Code.InvalidArgument))).toBe(false);
  });
});
