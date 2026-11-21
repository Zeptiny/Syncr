import { describe, expect, it } from "vitest";

import {
  compareReleases,
  githubRef,
  isSafeRef,
  notesSlug,
  parseRelease,
  type Release,
} from "./version";

const r = (v: string): Release => {
  const release = parseRelease(v);
  if (!release) throw new Error(`not a release: ${v}`);
  return release;
};

describe("parseRelease", () => {
  it("accepts release tags and release candidates", () => {
    expect(parseRelease("v2.3.0")).toEqual({ major: 2, minor: 3, patch: 0, base: "v2.3.0" });
    expect(parseRelease("v2.3.0-rc.1")).toEqual({
      major: 2,
      minor: 3,
      patch: 0,
      rc: 1,
      base: "v2.3.0",
    });
    expect(parseRelease("v10.20.30-rc.12")?.base).toBe("v10.20.30");
  });

  it.each([
    "dev",
    "dev-abc1234",
    "v2.3.0-rc.1-4-gabc1234",
    "v2.3.0-dirty",
    "v2.3.0-4-gabc1234-dirty",
    "v0.0.0-20261121043852-abcdefabcdef",
    "2.3.0",
    "v2.3",
    "v2.3.0-beta.1",
    " v2.3.0",
    "",
  ])("rejects %j", (v) => {
    expect(parseRelease(v)).toBeUndefined();
  });

  it("rejects missing values", () => {
    expect(parseRelease(undefined)).toBeUndefined();
    expect(parseRelease(null)).toBeUndefined();
  });
});

describe("compareReleases", () => {
  it("orders by version, rc.N before the final release", () => {
    const sorted = ["v2.3.0", "v2.2.10", "v2.3.0-rc.2", "v10.0.0", "v2.3.0-rc.10", "v2.3.1-rc.1"]
      .map(r)
      .sort(compareReleases)
      .map((x) => (x.rc === undefined ? x.base : `${x.base}-rc.${x.rc}`));
    expect(sorted).toEqual([
      "v2.2.10",
      "v2.3.0-rc.2",
      "v2.3.0-rc.10",
      "v2.3.0",
      "v2.3.1-rc.1",
      "v10.0.0",
    ]);
    expect(compareReleases(r("v2.3.0"), r("v2.3.0"))).toBe(0);
  });
});

describe("notesSlug", () => {
  it("maps a release and its candidates to the final version's notes", () => {
    expect(notesSlug("v2.3.0")).toBe("releases/v2.3.0");
    expect(notesSlug("v2.3.0-rc.1")).toBe("releases/v2.3.0");
    expect(notesSlug("dev")).toBeUndefined();
    expect(notesSlug("v2.3.0-rc.1-4-gabc1234")).toBeUndefined();
    expect(notesSlug(undefined)).toBeUndefined();
  });
});

describe("githubRef", () => {
  it("points releases at their tag and everything else at main", () => {
    expect(githubRef("v2.3.0")).toBe("v2.3.0");
    expect(githubRef("v2.3.0-rc.1")).toBe("v2.3.0-rc.1");
    expect(githubRef("dev")).toBe("main");
    expect(githubRef("dev-abc1234")).toBe("main");
    expect(githubRef("v2.3.0-rc.1-4-gabc1234-dirty")).toBe("main");
    expect(githubRef("v0.0.0-20261121043852-abcdefabcdef")).toBe("main");
    expect(githubRef(undefined)).toBe("main");
  });

  it("only ever produces refs that isSafeRef accepts", () => {
    expect(isSafeRef("main")).toBe(true);
    expect(isSafeRef("v2.3.0-rc.1")).toBe(true);
    expect(isSafeRef('main"><script>')).toBe(false);
    expect(isSafeRef("../../x")).toBe(false);
  });
});
