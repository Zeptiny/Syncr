// @vitest-environment node
import { describe, expect, it } from "vitest";

import { buildDocs, type Page } from "./model.ts";
import { imagePlaceholder, imageSize } from "./render.ts";
import { TESTDATA_ROOT, testOptions } from "./testing.ts";

const { model, errors } = buildDocs(testOptions(TESTDATA_ROOT));
const page = (slug: string): Page => {
  const found = model.pages.find((p) => p.slug === slug);
  if (!found) throw new Error(`no page ${slug}`);
  return found;
};
const guide = page("guide").html;
const REF = "__SYNCR_GITHUB_REF__";
const NEW_TAB = ' target="_blank" rel="noopener noreferrer"';
const SR = '<span class="sr-only"> (opens in a new tab)</span>';

describe("link rewriting", () => {
  it("builds without errors", () => {
    expect(errors.lines()).toEqual([]);
  });

  it.each([
    ["same page", '<a href="#first-steps">same page link</a>'],
    ["shipped page", '<a href="/docs/other">other</a>'],
    ["shipped page plus anchor", '<a href="/docs/other#the-details">details</a>'],
    ["folder with a shipped README", '<a href="/docs/releases">releases</a>'],
    ["page in a folder plus anchor", '<a href="/docs/releases/v1.0.0#known-limitations">notes</a>'],
    [
      "GitHub-only markdown plus anchor",
      `<a href="https://github.com/o/r/blob/${REF}/docs/adr/0001-first.md#context"${NEW_TAB}>ADR${SR}</a>`,
    ],
    [
      "GitHub-only markdown",
      `<a href="https://github.com/o/r/blob/${REF}/docs/development.md"${NEW_TAB}>development${SR}</a>`,
    ],
    [
      "repository file",
      `<a href="https://github.com/o/r/blob/${REF}/deploy/compose.yml"${NEW_TAB}>compose${SR}</a>`,
    ],
    [
      "repository folder",
      `<a href="https://github.com/o/r/tree/${REF}/proto/v1"${NEW_TAB}>protos${SR}</a>`,
    ],
    [
      "GitHub-only folder",
      `<a href="https://github.com/o/r/tree/${REF}/docs/adr"${NEW_TAB}>decisions${SR}</a>`,
    ],
    [
      "repository markdown plus anchor",
      `<a href="https://github.com/o/r/blob/${REF}/README.md#quick-start"${NEW_TAB}>quick start${SR}</a>`,
    ],
    [
      "external",
      `<a href="https://rclone.org/crypt/" title="rclone crypt"${NEW_TAB}>rclone${SR}</a>`,
    ],
    ["mailto", '<a href="mailto:team@example.com">mail</a>'],
  ])("%s", (_, html) => {
    expect(guide).toContain(html);
  });

  it("rewrites links up one level and to the index", () => {
    expect(page("releases/v1.0.0").html).toContain(
      '<a href="/docs/guide#first-steps">the first steps</a>',
    );
    expect(page("releases/v1.0.0").html).toContain('<a href="/docs">the index</a>');
    expect(page("releases").html).toContain(
      '<a href="/docs/releases/v1.0.0#known-limitations">1.0.0</a>',
    );
  });
});

describe("rendering", () => {
  it("leaves the title out and links headings to themselves", () => {
    expect(guide).not.toContain("<h1");
    expect(guide).toContain(
      '<h2 id="first-steps"><a class="doc-heading-link" href="#first-steps">First steps</a></h2>',
    );
    expect(guide).toContain('<h2 id="first-steps-1">');
    expect(guide).toContain(
      '<h2 id="ftp--ftps"><a class="doc-heading-link" href="#ftp--ftps">FTP &amp; FTPS</a></h2>',
    );
    expect(page("other").headings.map((h) => [h.depth, h.id])).toEqual([
      [2, "the-details"],
      [3, "deeper"],
      [4, "deepest"],
    ]);
  });

  it("wraps code blocks with a label and a copy slot, and escapes them", () => {
    expect(guide).toContain(
      '<div class="doc-code"><div class="doc-code-bar"><span class="doc-code-lang">Shell</span>' +
        "<span data-copy-slot></span></div><pre><code>echo &quot;a &lt; b&quot;</code></pre></div>",
    );
    expect(guide).toContain(
      '<span class="doc-code-lang"></span><span data-copy-slot></span></div><pre><code>plain</code>',
    );
  });

  it("wraps tables in a scrollable region", () => {
    expect(guide).toContain(
      '<div class="doc-table" role="region" tabindex="0" aria-label="Table"><table>',
    );
  });

  it("turns images into placeholders with their size", () => {
    expect(page("guide").images).toEqual(["docs/img/shot.png", "docs/img/diagram.svg"]);
    expect(guide).toContain(
      `<img src="${imagePlaceholder(0)}" alt="A screenshot of the form" title="The form" width="4" height="3" loading="lazy" decoding="async">`,
    );
    expect(guide).toContain(
      `<img src="${imagePlaceholder(1)}" alt="A diagram" width="120" height="40"`,
    );
  });

  it("has no inline scripts, handlers or styles", () => {
    for (const p of model.pages) expect(p.html).not.toMatch(/<script|\son[a-z]+=|\sstyle=/i);
  });

  it("reads image sizes from PNG, WebP and SVG headers", () => {
    const png = Buffer.from("89504e470d0a1a0a0000000d49484452000002d0000001e00802000000", "hex");
    expect(imageSize(png, ".png")).toEqual({ width: 720, height: 480 });
    const vp8x = Buffer.alloc(30);
    vp8x.write("RIFF", 0, "latin1");
    vp8x.write("WEBPVP8X", 8, "latin1");
    vp8x.writeUIntLE(799, 24, 3);
    vp8x.writeUIntLE(599, 27, 3);
    expect(imageSize(vp8x, ".webp")).toEqual({ width: 800, height: 600 });
    const svg = Buffer.from('<svg width="64px" height="32" viewBox="0 0 10 10"></svg>');
    expect(imageSize(svg, ".svg")).toEqual({ width: 64, height: 32 });
    expect(imageSize(Buffer.from("<svg></svg>"), ".svg")).toBeUndefined();
    expect(imageSize(Buffer.from("not a png"), ".png")).toBeUndefined();
  });
});
