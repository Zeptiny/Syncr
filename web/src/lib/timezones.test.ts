import { describe, expect, it } from "vitest";

import { filterZones, hasOffsetChange, timeZones, tzOffset, type ZoneOption } from "./timezones";

const zones: ZoneOption[] = [
  { zone: "UTC", offset: "+00:00" },
  { zone: "America/New_York", offset: "-05:00" },
  { zone: "America/Argentina/Buenos_Aires", offset: "-03:00" },
  { zone: "Asia/Kolkata", offset: "+05:30" },
  { zone: "Europe/Berlin", offset: "+01:00" },
];

describe("filterZones", () => {
  it.each([
    ["", 5],
    ["new york", 1],
    ["NEW_YORK", 1],
    ["america/", 2],
    ["buenos aires", 1],
    ["+05:30", 1],
    ["utc+1", 1],
    ["-5", 1],
    ["nowhere", 0],
  ])("%j matches %i", (q, n) => {
    expect(filterZones(q, zones)).toHaveLength(n);
  });

  it("matches offsets exactly", () => {
    expect(filterZones("+0530", zones).map((z) => z.zone)).toEqual(["Asia/Kolkata"]);
  });
});

describe("zones", () => {
  it("lists UTC first and knows real zones", () => {
    const all = timeZones();
    expect(all[0]).toBe("UTC");
    expect(all).toContain("Europe/Berlin");
  });

  it("reads offsets and notices DST changes", () => {
    const winter = new Date("2027-01-15T12:00:00Z");
    const summer = new Date("2027-07-15T12:00:00Z");
    expect(tzOffset("Europe/Berlin", winter)).toBe("+01:00");
    expect(tzOffset("Europe/Berlin", summer)).toBe("+02:00");
    expect(tzOffset("Asia/Kolkata", summer)).toBe("+05:30");
    expect(hasOffsetChange([winter, summer], "Europe/Berlin")).toBe(true);
    expect(hasOffsetChange([winter, summer], "Asia/Kolkata")).toBe(false);
  });
});
