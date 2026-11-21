// @vitest-environment node
import path from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

import { buildDocs, globMatcher, parseMarkdown, relativePosix, slugOf, toPosix } from "./model.ts";
import { memFs, TESTDATA_ROOT, testOptions } from "./testing.ts";

const ROOT = "/repo";

/** A small valid tree; each error case changes or adds files. */
const base = {
  "docs/README.md": "# T\n\n## S\n\n- [A](a.md): the first\n- [B](b.md)\n",
  "docs/a.md": "# A\n\nText.\n",
  "docs/b.md": "# B\n\n## Back up a folder of a remote\n\nText.\n",
  "docs/shot.png": Buffer.from(
    "89504e470d0a1a0a0000000d494844520000000400000003080200000000000000",
    "hex",
  ),
  "docs/photo.jpg": "jpg",
};

function errorsOf(files: Record<string, string | Buffer>, extra = {}): string[] {
  return buildDocs(testOptions(ROOT, extra), memFs({ ...base, ...files })).errors.lines();
}

describe("paths", () => {
  it("converts native paths to POSIX once, also on Windows", () => {
    expect(toPosix("docs\\releases\\v2.3.0.md", path.win32)).toBe("docs/releases/v2.3.0.md");
    expect(toPosix("docs/releases/v2.3.0.md", path.posix)).toBe("docs/releases/v2.3.0.md");
    expect(relativePosix("C:\\src\\syncr", "C:\\src\\syncr\\docs\\adr\\0001.md", path.win32)).toBe(
      "docs/adr/0001.md",
    );
  });

  it("matches githubOnly globs", () => {
    const match = globMatcher(["development.md", "adr/**", "drafts/*.md"]);
    expect(match("development.md")).toBe(true);
    expect(match("adr/0001-first.md")).toBe(true);
    expect(match("adr/")).toBe(true);
    expect(match("drafts/x.md")).toBe(true);
    expect(match("drafts/a/x.md")).toBe(false);
    expect(match("developmentXmd")).toBe(false);
    expect(match("restic.md")).toBe(false);
  });

  it("maps files to slugs", () => {
    expect(slugOf("README.md")).toBe("");
    expect(slugOf("restic.md")).toBe("restic");
    expect(slugOf("releases/README.md")).toBe("releases");
    expect(slugOf("releases/v2.3.0.md")).toBe("releases/v2.3.0");
  });
});

describe("heading IDs", () => {
  it("follow GitHub", () => {
    const md = [
      "# Title",
      "## FTP & FTPS",
      "## What's new",
      "## `data_dir`/sources",
      "## 2. Each worker: `syncr-worker upgrade`",
      "## A &amp; B",
      "## Hooks: *a* [link](x.md) ![icon](y.png)",
      "## Done \u{2705}",
      "## Caf\u{e9}",
      "## X",
      "## X",
      "### X",
    ].join("\n\n");
    expect(parseMarkdown("docs/x.md", md).headings.map((h) => h.id)).toEqual([
      "title",
      "ftp--ftps",
      "whats-new",
      "data_dirsources",
      "2-each-worker-syncr-worker-upgrade",
      "a--b",
      "hooks-a-link",
      "done-",
      "caf\u{e9}",
      "x",
      "x-1",
      "x-2",
    ]);
  });

  it("count lines the same with CRLF", () => {
    const parsed = parseMarkdown("docs/x.md", "# T\r\n\r\ntext\r\n\r\n## Here\r\n");
    expect(parsed.headings.at(-1)).toEqual({ depth: 2, id: "here", text: "Here", line: 5 });
  });
});

describe("the index", () => {
  it("builds the navigation, reading order and shipped set from testdata", () => {
    const { model, errors } = buildDocs(testOptions(TESTDATA_ROOT));
    expect(errors.lines()).toEqual([]);
    expect(model.title).toBe("Test docs");
    expect(model.groups).toEqual([
      {
        title: "Start",
        items: [
          { label: "The guide", slug: "guide" },
          { label: "Other page", slug: "other" },
        ],
      },
      {
        title: "Reference",
        items: [
          {
            label: "Release notes",
            slug: "releases",
            children: [{ label: "1.0.0", slug: "releases/v1.0.0" }],
          },
        ],
      },
      {
        title: "On GitHub",
        items: [
          {
            label: "Development",
            external: "https://github.com/o/r/blob/__SYNCR_GITHUB_REF__/docs/development.md",
          },
          {
            label: "Decisions",
            external: "https://github.com/o/r/tree/__SYNCR_GITHUB_REF__/docs/adr",
          },
        ],
      },
    ]);
    expect(model.pages.map((p) => [p.slug, p.file, p.title])).toEqual([
      ["", "docs/README.md", "Test docs"],
      ["guide", "docs/guide.md", "The guide"],
      ["other", "docs/other.md", "Other page"],
      ["releases", "docs/releases/README.md", "Releases"],
      ["releases/v1.0.0", "docs/releases/v1.0.0.md", "Release 1.0.0"],
    ]);
  });

  it("passes on the real docs/", () => {
    const repoRoot = fileURLToPath(new URL("../..", import.meta.url));
    const { model, errors } = buildDocs(testOptions(repoRoot));
    expect(errors.lines()).toEqual([]);
    expect(model.pages.length).toBeGreaterThan(10);
    expect(model.pages.map((p) => p.slug)).toContain("releases/v2.3.0");
  });
});

describe("checks", () => {
  it("accepts the base tree", () => {
    expect(errorsOf({})).toEqual([]);
  });

  it("reports a broken anchor with the closest heading, across pages and on the same page", () => {
    expect(
      errorsOf({
        "docs/a.md": "# A\n\nSee [b](b.md#back-up-a-folder).\n\nAnd [here](#nowhere).\n",
      }),
    ).toEqual([
      'docs/a.md:3: link "b.md#back-up-a-folder": docs/b.md has no heading "back-up-a-folder" (closest: "back-up-a-folder-of-a-remote")',
      'docs/a.md:5: link "#nowhere": docs/a.md has no heading "nowhere"',
    ]);
  });

  it("reports a missing file, also outside docs/", () => {
    expect(errorsOf({ "docs/a.md": "# A\n\n[c](c.md) and [x](../deploy/compose.yml)\n" })).toEqual([
      'docs/a.md:3: link "../deploy/compose.yml": no such file in the repository',
      'docs/a.md:3: link "c.md": no such file in the repository',
    ]);
  });

  it("skips files outside docs/ with repoLinks skip, still rewriting them", () => {
    const files = {
      "docs/a.md":
        "# A\n\n[x](../deploy/compose.yml), [y](../README.md#quick-start), [p](../proto/)\n",
    };
    expect(errorsOf(files, { repoLinks: "skip" })).toEqual([]);
    const { model } = buildDocs(
      testOptions(ROOT, { repoLinks: "skip" }),
      memFs({ ...base, ...files }),
    );
    const html = model.pages.find((p) => p.slug === "a")?.html;
    expect(html).toContain(
      'href="https://github.com/o/r/blob/__SYNCR_GITHUB_REF__/deploy/compose.yml"',
    );
    expect(html).toContain('href="https://github.com/o/r/tree/__SYNCR_GITHUB_REF__/proto"');
    // Inside docs/ everything is still checked.
    expect(errorsOf({ "docs/a.md": "# A\n\n[c](c.md)\n" }, { repoLinks: "skip" })).toEqual([
      'docs/a.md:3: link "c.md": no such file in the repository',
    ]);
  });

  it("requires a trailing slash on folder links outside docs/", () => {
    // With repoLinks skip the build cannot tell the folder from a file: it
    // would link /blob/ where the checked build links /tree/.
    const files = {
      "proto/v1/api.proto": "",
      "docs/a.md": "# A\n\n[p](../proto/v1) [q](../proto/v1/)\n",
    };
    expect(errorsOf(files)).toEqual([
      'docs/a.md:3: link "../proto/v1" is a folder: end the link with "/"',
    ]);
  });

  it("rejects an anchor on a folder link", () => {
    expect(
      errorsOf({
        "proto/v1/api.proto": "",
        "docs/adr/0001.md": "# ADR\n\n## Context\n",
        "docs/development.md": "# Dev\n",
        "docs/a.md": "# A\n\n[d](adr/#context) [p](../proto/#v1)\n",
      }),
    ).toEqual([
      'docs/a.md:3: link "../proto/#v1": a folder has no headings; link to a file in it',
      'docs/a.md:3: link "adr/#context": a folder has no headings; link to a file in it',
    ]);
  });

  it("rejects a markdown file whose extension is not .md", () => {
    expect(
      errorsOf({
        "docs/README.md": "# T\n\n## S\n\n- [A](a.md)\n- [B](b.md)\n- [U](Upper.MD)\n",
        "docs/Upper.MD": "# Upper\n",
        "docs/adr/0002.Md": "# ADR\n",
      }),
    ).toEqual([
      'docs/Upper.MD:1: write the extension as ".md": other spellings are neither shipped nor checked',
      'docs/adr/0002.Md:1: write the extension as ".md": other spellings are neither shipped nor checked',
    ]);
  });

  it("rejects raw HTML, block and inline", () => {
    expect(errorsOf({ "docs/a.md": "# A\n\n<div>\nx\n</div>\n\nin <folder> here\n" })).toEqual([
      'docs/a.md:3: raw HTML "<div>..." is not allowed: put it in backticks or escape it (\\<div\\>...)',
      'docs/a.md:7: raw HTML "<folder>" is not allowed: put it in backticks or escape it (\\<folder\\>)',
    ]);
  });

  it("rejects other schemes and paths that leave the repository", () => {
    expect(
      errorsOf({
        "docs/a.md":
          "# A\n\n[x](javascript:alert(1))\n\n[y](../../x.md)\n\n[z](/docs/a.md) [w](//host/x)\n",
      }),
    ).toEqual([
      'docs/a.md:3: link "javascript:alert(1)" is not allowed (only https, http, mailto, or paths inside the repository)',
      'docs/a.md:5: link "../../x.md" leaves the repository',
      'docs/a.md:7: link "//host/x" is not allowed (only https, http, mailto, or paths inside the repository)',
      'docs/a.md:7: link "/docs/a.md" is not allowed (only https, http, mailto, or paths inside the repository)',
    ]);
  });

  it("checks image types, places and alt text", () => {
    expect(
      errorsOf({
        "docs/a.md":
          "# A\n\n![a photo](photo.jpg)\n\n![](shot.png)\n\n![remote](https://example.com/x.png)\n\n![gone](gone.png)\n",
      }),
    ).toEqual([
      'docs/a.md:3: image "photo.jpg": only .png, .webp and .svg files inside docs/ (served by the controller)',
      'docs/a.md:5: image "shot.png" has no alt text',
      'docs/a.md:7: image "https://example.com/x.png": only .png, .webp and .svg files inside docs/ (served by the controller)',
      'docs/a.md:9: image "gone.png": no such file in docs/',
    ]);
  });

  it("requires every page in the index, once", () => {
    expect(
      errorsOf({
        "docs/c.md": "# C\n",
        "docs/adr/0001.md": "# ADR\n",
        "docs/development.md": "# Dev\n",
      }),
    ).toEqual([
      "docs/c.md:1: not listed in docs/README.md (or in githubOnly in web/vite.config.ts)",
    ]);
    expect(
      errorsOf({
        "docs/README.md": "# T\n\n## S\n\n- [A](a.md)\n- [B](b.md)\n- [A again](a.md)\n",
      }),
    ).toEqual(["docs/README.md:7: docs/a.md is listed twice (first on line 5)"]);
  });

  it("checks the index structure", () => {
    expect(
      errorsOf({
        "docs/README.md":
          "# T\n\nIntro.\n\n## S\n\nA paragraph.\n\n- [A](a.md)\n\n## R\n\n- Not a link\n- [B](b.md#back-up-a-folder-of-a-remote)\n- [C](https://example.com)\n- [B](b.md) and more\n",
      }),
    ).toEqual([
      "docs/README.md:7: each section holds one bullet list of links",
      "docs/README.md:13: each item starts with a link to a page",
      'docs/README.md:14: index items link to a page or folder in docs/, without "#anchor"',
      'docs/README.md:15: index items link to a page or folder in docs/, without "#anchor"',
      'docs/README.md:16: write the description after the link and ": "',
    ]);
  });

  it("requires exactly one H1 at the start of a page", () => {
    expect(errorsOf({ "docs/a.md": "Text first.\n\n# A\n" })).toEqual([
      'docs/a.md:1: a page starts with exactly one "# Title"',
    ]);
    expect(errorsOf({ "docs/a.md": "# A\n\n# Again\n" })).toEqual([
      'docs/a.md:3: a page starts with exactly one "# Title"',
    ]);
  });

  it("rejects reserved heading IDs and GitHub alerts", () => {
    expect(
      errorsOf({ "docs/a.md": "# A\n\n## Root\n\n> [!NOTE]\n> Careful.\n\n## Main content\n" }),
    ).toEqual([
      'docs/a.md:3: heading ID "root" is reserved by the app',
      'docs/a.md:5: "> [!NOTE]" alerts are not supported in the app; write a plain blockquote',
      'docs/a.md:8: heading ID "main-content" is reserved by the app',
    ]);
  });

  it("collects every error of every file, sorted by file and line", () => {
    expect(
      errorsOf({
        "docs/b.md": "# B\n\n[x](x.md)\n",
        "docs/a.md": "# A\n" + "\n".repeat(10) + "[y](y.md)\n\n[z](z.md)\n",
      }),
    ).toEqual([
      'docs/a.md:12: link "y.md": no such file in the repository',
      'docs/a.md:14: link "z.md": no such file in the repository',
      'docs/b.md:3: link "x.md": no such file in the repository',
    ]);
  });
});
