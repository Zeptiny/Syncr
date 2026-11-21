import manifest from "virtual:syncr-docs/manifest";
import { describe, expect, it } from "vitest";

import { DOC_TITLES, type DocPage } from "./docs.gen";

// A smoke test of the docs plugin's modules as the app sees them; the plugin
// itself is tested in web/docs-plugin.
describe("virtual:syncr-docs", () => {
  it("lists the index first and every page in the generated titles", () => {
    expect(manifest.pages[0]).toMatchObject({ slug: "", file: "docs/README.md" });
    for (const page of manifest.pages) expect(DOC_TITLES[page.slug]).toBe(page.title);
    expect(Object.keys(manifest.loaders).sort()).toEqual(Object.keys(DOC_TITLES).sort());
  });

  it("loads a page with its headings and rewritten links", async () => {
    const slug: DocPage = "restic";
    const page = (await manifest.loaders[slug]()).default;
    expect(page.title).toBe(DOC_TITLES.restic);
    expect(page.headings).toContainEqual({
      depth: 2,
      id: "back-up-a-folder-of-a-remote",
      text: "Back up a folder of a remote",
    });
    expect(page.html).toContain('id="back-up-a-folder-of-a-remote"');
    // Links to other pages are app routes; only GitHub links keep ".md".
    expect(page.html).not.toMatch(/href="(?!https:)[^"]*\.md[#"]/);
  });

  it("loads the search index", async () => {
    const { default: index } = await import("virtual:syncr-docs/search");
    expect(index.pages.length).toBe(manifest.pages.length - 1);
    expect(index.sections.some(([, anchor]) => anchor === "back-up-a-folder-of-a-remote")).toBe(
      true,
    );
  });
});
