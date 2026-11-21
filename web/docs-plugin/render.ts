// Renders a page to HTML with marked: GitHub-compatible heading IDs, links
// rewritten for the app (or to GitHub, pinned by a ref token the viewer
// replaces), images as module imports, copy slots on code blocks.

import path from "node:path";

import { getDefaults, Parser, Renderer, type Token, type Tokens } from "marked";

import { closest, short, type DocErrors } from "./checks.ts";
import {
  plainText,
  slugOf,
  walkTokens,
  type DocsFs,
  type DocsOptions,
  type ParsedFile,
} from "./model.ts";

/** Stands for the Git ref in GitHub links; the viewer replaces it. */
export const GITHUB_REF_TOKEN = "__SYNCR_GITHUB_REF__";

/** What link resolution needs to know about the repository. */
export interface Site {
  options: DocsOptions;
  fs: DocsFs;
  /** The docs folder relative to the repository, normally "docs". */
  docsRel: string;
  /** Every file and folder under docs/ (exact case, as GitHub sees them). */
  docFiles: ReadonlySet<string>;
  docDirs: ReadonlySet<string>;
  /** Shipped markdown file -> slug. */
  shipped: ReadonlyMap<string, string>;
  githubOnly(rel: string): boolean;
  /** The heading IDs of a markdown file, or undefined when it does not exist. */
  headingIds(rel: string): ReadonlySet<string> | undefined;
}

export type LinkTarget =
  | { kind: "page"; file: string; href: string }
  | { kind: "anchor"; href: string }
  | { kind: "github"; href: string }
  | { kind: "external"; href: string }
  | { kind: "error"; message: string };

const SCHEME_RE = /^([a-z][a-z0-9+.-]*):/i;
const ALLOWED_SCHEMES = new Set(["https", "http", "mailto"]);

export function escapeHtml(text: string): string {
  return text
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

const encodePath = (rel: string): string => rel.split("/").map(encodeURIComponent).join("/");

/**
 * Resolves a link written in the markdown file `from` (repository-relative)
 * and checks its target. `forIndex` resolves an item of docs/README.md:
 * only files and folders in docs/, without anchors.
 */
export function resolveLink(
  site: Site,
  from: string,
  href: string,
  opts: { forIndex?: boolean } = {},
): LinkTarget {
  const quoted = `link "${short(href)}"`;
  const error = (message: string): LinkTarget => ({ kind: "error", message });
  const indexOnly = 'index items link to a page or folder in docs/, without "#anchor"';
  const badScheme = `${quoted} is not allowed (only https, http, mailto, or paths inside the repository)`;

  const checkAnchor = (file: string, fragment: string): string | undefined => {
    let id: string;
    try {
      id = decodeURIComponent(fragment);
    } catch {
      return `${quoted} has a malformed anchor`;
    }
    const ids = site.headingIds(file);
    if (!ids || ids.has(id)) return undefined;
    const near = closest(id, ids);
    return `${quoted}: ${file} has no heading "${id}"${near ? ` (closest: "${near}")` : ""}`;
  };

  if (href.startsWith("#")) {
    if (opts.forIndex) return error(indexOnly);
    const problem = checkAnchor(from, href.slice(1));
    return problem ? error(problem) : { kind: "anchor", href };
  }
  const scheme = SCHEME_RE.exec(href);
  if (scheme) {
    if (!ALLOWED_SCHEMES.has(scheme[1].toLowerCase())) return error(badScheme);
    if (opts.forIndex) return error(indexOnly);
    return { kind: "external", href: encodeURI(href).replace(/%25/g, "%") };
  }
  if (href === "" || href.startsWith("/") || href.startsWith("\\")) return error(badScheme);

  const hashAt = href.indexOf("#");
  const fragment = hashAt >= 0 ? href.slice(hashAt + 1) : undefined;
  let pathPart: string;
  try {
    pathPart = decodeURIComponent(hashAt >= 0 ? href.slice(0, hashAt) : href);
  } catch {
    return error(`${quoted} is not a valid path`);
  }
  if (pathPart.includes("\\")) return error(badScheme);
  const dirSyntax = pathPart.endsWith("/");
  let target = path.posix.normalize(path.posix.join(path.posix.dirname(from), pathPart));
  if (target.endsWith("/")) target = target.slice(0, -1);
  if (target === ".") target = "";
  if (target === ".." || target.startsWith("../")) return error(`${quoted} leaves the repository`);

  const { docsRel } = site;
  const inDocs = target === docsRel || target.startsWith(`${docsRel}/`);
  const checked = inDocs || site.options.repoLinks === "check";
  let kind: "file" | "dir" | undefined;
  if (inDocs)
    kind = site.docFiles.has(target) ? "file" : site.docDirs.has(target) ? "dir" : undefined;
  else if (checked) kind = target === "" ? "dir" : site.fs.kind(target);
  else kind = dirSyntax ? "dir" : "file";
  if (!kind || (dirSyntax && kind === "file"))
    return error(`${quoted}: no such file in the repository`);
  // With repoLinks "skip" a folder outside docs/ is known only by its "/":
  // requiring it keeps that build's links the same as a checked build's.
  if (kind === "dir" && !inDocs && !dirSyntax)
    return error(`${quoted} is a folder: end the link with "/"`);
  if (opts.forIndex && (!inDocs || fragment !== undefined)) return error(indexOnly);

  // A shipped page, or a folder whose README is one.
  const pageFile = kind === "dir" ? `${target}/README.md` : target;
  const isPage =
    inDocs &&
    pageFile.endsWith(".md") &&
    site.docFiles.has(pageFile) &&
    !site.githubOnly(pageFile) &&
    (opts.forIndex || site.shipped.has(pageFile));
  if (isPage) {
    if (fragment !== undefined) {
      const problem = checkAnchor(pageFile, fragment);
      if (problem) return error(problem);
    }
    const slug = site.shipped.get(pageFile) ?? slugOf(pageFile.slice(docsRel.length + 1));
    const hash = fragment !== undefined ? `#${fragment}` : "";
    return { kind: "page", file: pageFile, href: `/docs${slug ? `/${slug}` : ""}${hash}` };
  }

  if (fragment !== undefined && kind === "dir")
    return error(`${quoted}: a folder has no headings; link to a file in it`);
  if (fragment !== undefined && kind === "file" && target.endsWith(".md") && checked) {
    const problem = checkAnchor(target, fragment);
    if (problem) return error(problem);
  }
  const where = target ? `/${encodePath(target)}` : "";
  const hash = fragment !== undefined ? `#${fragment}` : "";
  return {
    kind: "github",
    href: `${site.options.githubRepo}/${kind === "dir" ? "tree" : "blob"}/${GITHUB_REF_TOKEN}${where}${hash}`,
  };
}

// ---------------------------------------------------------------------------
// Images

const IMAGE_TYPES = new Set([".png", ".webp", ".svg"]);

/** Width and height from a PNG, WebP or SVG header; undefined when unreadable. */
export function imageSize(
  data: Buffer,
  ext: string,
): { width: number; height: number } | undefined {
  if (ext === ".png") {
    if (data.length < 24 || data.toString("latin1", 12, 16) !== "IHDR") return undefined;
    return { width: data.readUInt32BE(16), height: data.readUInt32BE(20) };
  }
  if (ext === ".webp") {
    if (
      data.length < 30 ||
      data.toString("latin1", 0, 4) !== "RIFF" ||
      data.toString("latin1", 8, 12) !== "WEBP"
    ) {
      return undefined;
    }
    switch (data.toString("latin1", 12, 16)) {
      case "VP8X":
        return { width: 1 + data.readUIntLE(24, 3), height: 1 + data.readUIntLE(27, 3) };
      case "VP8 ":
        return { width: data.readUInt16LE(26) & 0x3fff, height: data.readUInt16LE(28) & 0x3fff };
      case "VP8L": {
        const bits = data.readUInt32LE(21);
        return { width: (bits & 0x3fff) + 1, height: ((bits >> 14) & 0x3fff) + 1 };
      }
      default:
        return undefined;
    }
  }
  const svg = /<svg\b[^>]*>/i.exec(data.toString("utf8"))?.[0];
  if (!svg) return undefined;
  const attr = (name: string): number | undefined => {
    const m = new RegExp(`\\s${name}\\s*=\\s*["']([0-9.]+)(px)?["']`).exec(svg);
    return m ? Math.round(Number(m[1])) : undefined;
  };
  const [width, height] = [attr("width"), attr("height")];
  if (width && height) return { width, height };
  const box =
    /\sviewBox\s*=\s*["']\s*[-0-9.]+[\s,]+[-0-9.]+[\s,]+([0-9.]+)[\s,]+([0-9.]+)\s*["']/.exec(svg);
  return box
    ? { width: Math.round(Number(box[1])), height: Math.round(Number(box[2])) }
    : undefined;
}

interface ImageRef {
  file: string;
  width?: number;
  height?: number;
}

export function resolveImage(
  site: Site,
  from: string,
  href: string,
  alt: string,
): { image?: ImageRef; errors: string[] } {
  const quoted = `image "${short(href)}"`;
  const wrongType = `${quoted}: only .png, .webp and .svg files inside docs/ (served by the controller)`;
  const errors: string[] = [];
  if (alt.trim() === "") errors.push(`${quoted} has no alt text`);
  let target: string | undefined;
  if (!SCHEME_RE.test(href) && !href.startsWith("/") && !/[\\?#]/.test(href)) {
    try {
      target = path.posix.normalize(
        path.posix.join(path.posix.dirname(from), decodeURIComponent(href)),
      );
    } catch {
      target = undefined;
    }
  }
  if (!target?.startsWith(`${site.docsRel}/`) || !IMAGE_TYPES.has(path.posix.extname(target))) {
    return { errors: [wrongType, ...errors] };
  }
  if (!site.docFiles.has(target))
    return { errors: [`${quoted}: no such file in docs/`, ...errors] };
  const ext = path.posix.extname(target);
  const size = imageSize(site.fs.read(target), ext);
  if (!size && ext !== ".svg") errors.push(`${quoted}: cannot read the image size`);
  return { image: { file: target, ...size }, errors };
}

/** Marks where an image URL goes in the HTML; codegen turns it into an import. */
export function imagePlaceholder(index: number): string {
  return `\x00img${index}\x00`;
}

// NUL never survives in marked output (it becomes U+FFFD), so it cannot clash.
// eslint-disable-next-line no-control-regex
export const IMAGE_PLACEHOLDER_RE = /\x00img(\d+)\x00/g;

// ---------------------------------------------------------------------------
// The renderer

const CODE_LABELS: Record<string, string> = {
  sh: "Shell",
  bash: "Shell",
  shell: "Shell",
  console: "Shell",
  powershell: "PowerShell",
  ps1: "PowerShell",
  pwsh: "PowerShell",
  yaml: "YAML",
  yml: "YAML",
  json: "JSON",
  http: "HTTP",
  ini: "INI",
  toml: "TOML",
  go: "Go",
  python: "Python",
  py: "Python",
  text: "",
  txt: "",
  plain: "",
};

interface RenderContext {
  page: ParsedFile;
  links: WeakMap<Tokens.Link, LinkTarget>;
  images: WeakMap<Tokens.Image, ImageRef>;
  imageFiles: string[];
}

function containsLink(tokens: Token[]): boolean {
  let found = false;
  walkTokens(tokens, (t) => {
    if (t.type === "link") found = true;
  });
  return found;
}

class DocRenderer extends Renderer {
  readonly ctx: RenderContext;

  constructor(ctx: RenderContext) {
    super();
    this.ctx = ctx;
  }

  heading(token: Tokens.Heading): string {
    // The H1 is the page title; the viewer renders it.
    if (token.depth === 1) return "";
    const id = escapeHtml(this.ctx.page.headingIds.get(token) ?? "");
    const inner = this.parser.parseInline(token.tokens);
    const body = containsLink(token.tokens)
      ? inner
      : `<a class="doc-heading-link" href="#${id}">${inner}</a>`;
    return `<h${token.depth} id="${id}">${body}</h${token.depth}>\n`;
  }

  code(token: Tokens.Code): string {
    const lang = (token.lang ?? "").trim().split(/\s+/)[0].toLowerCase();
    const label = CODE_LABELS[lang] ?? lang;
    const code = token.escaped ? token.text : escapeHtml(token.text);
    return (
      `<div class="doc-code"><div class="doc-code-bar"><span class="doc-code-lang">${escapeHtml(label)}</span>` +
      `<span data-copy-slot></span></div><pre><code>${code}</code></pre></div>\n`
    );
  }

  table(token: Tokens.Table): string {
    return `<div class="doc-table" role="region" tabindex="0" aria-label="Table">${super.table(token)}</div>\n`;
  }

  link(token: Tokens.Link): string {
    const inner = this.parser.parseInline(token.tokens);
    const target = this.ctx.links.get(token);
    if (!target || target.kind === "error") return inner;
    const title = token.title ? ` title="${escapeHtml(token.title)}"` : "";
    const href = escapeHtml(target.href);
    const newTab =
      target.kind === "github" || (target.kind === "external" && /^https?:/i.test(target.href));
    if (!newTab) return `<a href="${href}"${title}>${inner}</a>`;
    return (
      `<a href="${href}"${title} target="_blank" rel="noopener noreferrer">${inner}` +
      `<span class="sr-only"> (opens in a new tab)</span></a>`
    );
  }

  image(token: Tokens.Image): string {
    const image = this.ctx.images.get(token);
    if (!image) return "";
    let index = this.ctx.imageFiles.indexOf(image.file);
    if (index < 0) index = this.ctx.imageFiles.push(image.file) - 1;
    const alt = escapeHtml(plainText(token.tokens).trim() || token.text);
    const title = token.title ? ` title="${escapeHtml(token.title)}"` : "";
    const size =
      image.width && image.height ? ` width="${image.width}" height="${image.height}"` : "";
    return `<img src="${imagePlaceholder(index)}" alt="${alt}"${title}${size} loading="lazy" decoding="async">`;
  }

  html(token: Tokens.HTML | Tokens.Tag): string {
    // Raw HTML is a build error; while the dev server shows it, it is text.
    return escapeHtml(token.raw);
  }
}

/** Resolves every link and image of a page (collecting errors) and renders it. */
export function renderPage(
  page: ParsedFile,
  site: Site,
  errors: DocErrors,
): { html: string; images: string[] } {
  const ctx: RenderContext = { page, links: new WeakMap(), images: new WeakMap(), imageFiles: [] };
  const line = (token: object): number => page.lines.get(token) ?? 1;
  walkTokens(page.tokens, (token) => {
    if (token.type === "link") {
      const link = token as Tokens.Link;
      const target = resolveLink(site, page.file, link.href);
      if (target.kind === "error") errors.add(page.file, line(link), target.message);
      ctx.links.set(link, target);
    } else if (token.type === "image") {
      const image = token as Tokens.Image;
      const resolved = resolveImage(
        site,
        page.file,
        image.href,
        plainText(image.tokens) || image.text,
      );
      for (const message of resolved.errors) errors.add(page.file, line(image), message);
      if (resolved.image) ctx.images.set(image, resolved.image);
    }
  });
  const parser = new Parser({ ...getDefaults(), gfm: true, renderer: new DocRenderer(ctx) });
  const html = parser.parse(page.tokens);
  return { html, images: ctx.imageFiles };
}
