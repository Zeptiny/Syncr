import { describe, expect, it } from "vitest";

import { canonicalSlug, githubFileUrl, isPage, manifest, pageInfo, pinRef } from "./manifest";

describe("canonicalSlug", () => {
  it.each([
    ["restic", "restic"],
    ["restic.md", "restic"],
    ["restic/", "restic"],
    ["releases/README.md", "releases"],
    ["releases/README", "releases"],
    ["releases/v2.3.0", "releases/v2.3.0"],
    ["releases/v2.3.0.md", "releases/v2.3.0"],
    ["README.md", ""],
    ["README", ""],
  ])("%j is %j", (splat, slug) => {
    expect(canonicalSlug(splat)).toBe(slug);
  });
});

describe("the manifest helpers", () => {
  it("accepts only shipped pages", () => {
    expect(isPage("restic")).toBe(true);
    expect(isPage("development")).toBe(false);
    expect(isPage("constructor")).toBe(false);
  });

  it("pins GitHub refs and refuses anything but a release tag or main", () => {
    const url = `${manifest.githubRepo}/blob/${manifest.githubRefToken}/docs/development.md`;
    expect(pinRef(url, "v2.3.0-rc.1")).toBe(
      `${manifest.githubRepo}/blob/v2.3.0-rc.1/docs/development.md`,
    );
    expect(pinRef(url, '"><img src=x>')).toBe(
      `${manifest.githubRepo}/blob/main/docs/development.md`,
    );
    expect(githubFileUrl("docs/restic.md", "v2.3.0")).toBe(
      `${manifest.githubRepo}/blob/v2.3.0/docs/restic.md`,
    );
  });

  it("orders pages as docs/README.md lists them", () => {
    expect(pageInfo("")).toMatchObject({ previous: undefined, next: { slug: "deployment" } });
    const last = manifest.pages[manifest.pages.length - 1];
    expect(pageInfo(last.slug).next).toBeUndefined();
    expect(pageInfo("configuration").previous?.slug).toBe("deployment");
  });
});
