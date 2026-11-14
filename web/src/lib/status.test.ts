import { describe, expect, it } from "vitest";

import { JobErrorCode } from "@/gen/syncr/v1/link_pb";

import { errorCodeLabel } from "./status";

describe("errorCodeLabel", () => {
  it("names every error code the contract defines", () => {
    const codes = Object.values(JobErrorCode).filter(
      (c): c is JobErrorCode => typeof c === "number" && c !== JobErrorCode.UNSPECIFIED,
    );
    expect(codes).toContain(JobErrorCode.SOURCE_EMPTY);
    for (const code of codes) expect(errorCodeLabel(code), JobErrorCode[code]).not.toBe("Error");
  });

  it("says an empty remote folder stopped the backup", () => {
    expect(errorCodeLabel(JobErrorCode.SOURCE_EMPTY)).toBe("The remote folder is empty");
  });

  it("falls back for codes it does not know", () => {
    expect(errorCodeLabel(JobErrorCode.UNSPECIFIED)).toBe("Error");
    expect(errorCodeLabel(999 as JobErrorCode)).toBe("Error");
  });
});
