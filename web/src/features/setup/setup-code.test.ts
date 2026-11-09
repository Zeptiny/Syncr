import { describe, expect, it } from "vitest";

import { formatSetupCode, setupCodeLength } from "./setup-code";

describe("formatSetupCode", () => {
  it("groups a generated code as it is typed or pasted", () => {
    expect(formatSetupCode("7qk2mxc4pa")).toBe("7QK2M-XC4PA");
    expect(formatSetupCode("7QK2M-XC4PA-ABCDE-FGHIJ")).toBe("7QK2M-XC4PA-ABCDE-FGHIJ");
    expect(formatSetupCode(" 7qk2m xc4pa\n")).toBe("7QK2M-XC4PA");
    expect(formatSetupCode("")).toBe("");
  });

  it("keeps a preset code exactly as typed", () => {
    const preset = "correct-horse-battery-staple-01";
    expect(formatSetupCode(preset)).toBe(preset);
    expect(formatSetupCode("0189018901890189")).toBe("0189018901890189");
    expect(formatSetupCode("Pa$$w0rd-Pa$$w0rd")).toBe("Pa$$w0rd-Pa$$w0rd");
  });

  it("does not truncate a code longer than a generated one", () => {
    const long = "ABCDEFGHIJKLMNOPQRSTUVWXYZ";
    expect(formatSetupCode(long)).toBe(long);
    expect(formatSetupCode("ABCDE-FGHIJ-KLMNO-PQRST-UV")).toBe("ABCDE-FGHIJ-KLMNO-PQRST-UV");
  });

  it("never loses a character while a preset code is typed", () => {
    const preset = "correct-horse-battery-staple-01";
    let value = "";
    for (const ch of preset) value = formatSetupCode(value + ch);
    expect(value.replace(/[\s-]/g, "").toUpperCase()).toBe(
      preset.replace(/[\s-]/g, "").toUpperCase(),
    );
  });
});

describe("setupCodeLength", () => {
  it("ignores dashes and white space like the server", () => {
    expect(setupCodeLength("abcd efgh-ijkl mnop")).toBe(16);
    expect(setupCodeLength("0189018901890189")).toBe(16);
    expect(setupCodeLength("correct-horse-battery-staple-01")).toBe(27);
    expect(setupCodeLength("")).toBe(0);
  });
});
