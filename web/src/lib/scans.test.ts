import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, it } from "vitest";

import { BUSY_TEXT, SLOTS_BUSY_TEXT, scanErrorText } from "./scans";

describe("scanErrorText", () => {
  it("tells a scan already running from a worker with every slot taken", () => {
    const err = (msg: string, code = Code.Unavailable) => new ConnectError(msg, code);
    expect(
      scanErrorText(
        err(`worker "nas-01" is already measuring or searching; try again in a moment`),
      ),
    ).toBe(BUSY_TEXT);
    expect(
      scanErrorText(err(`worker "nas-01" is busy with other requests; try again in a moment`)),
    ).toBe(SLOTS_BUSY_TEXT);
    expect(scanErrorText(err("is busy with other requests", Code.FailedPrecondition))).not.toBe(
      SLOTS_BUSY_TEXT,
    );
  });
});
