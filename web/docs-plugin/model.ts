// Reads docs/, parses the index (docs/README.md) and decides which pages
// ship. Everything after reading works on POSIX paths relative to the
// repository root ("docs/restic.md"), so the output is the same on Windows.

import fs from "node:fs";
import path from "node:path";

import GithubSlugger from "github-slugger";
import { getDefaults, Lexer, type Token, type Tokens } from "marked";

import { checkPage, DocErrors } from "./checks.ts";
import { renderPage, resolveLink, type Site } from "./render.ts";
import { buildSearchIndex, type SearchIndex } from "./search.ts";

export interface DocsOptions {
  /** The docs folder (absolute). */
  docsDir: string;
  /** The repository root (absolute); docsDir must be inside it. */
  repoRoot: string;
  /** For example https://github.com/Zeptiny/Syncr. */
  githubRepo: string;
  /** Globs relative to docsDir of markdown that stays on GitHub. */
  githubOnly: string[];
  /** The generated types file, relative to the web root. */
  genFile: string;
  /** "skip" when the repository outside docs/ is not available (Docker). */
  repoLinks: "check" | "skip";
}

/** Converts a native relative path to POSIX once, at the file system edge. */
export function toPosix(p: string, pathImpl: { sep: string } = path): string {
  return pathImpl.sep === "/" ? p : p.split(pathImpl.sep).join("/");
}

/** The POSIX path of `to` relative to `from`, both native and absolute. */
export function relativePosix(
  from: string,
  to: string,
  pathImpl: Pick<typeof path, "sep" | "relative"> = path,
): string {
  return toPosix(pathImpl.relative(from, to), pathImpl);
}

/** Code unit order, the same everywhere (no locale). */
export function compareCodeUnits(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

/** File access by repository-relative POSIX path. */
export interface DocsFs {
  /** Every file under dir, recursively, sorted. */
  files(dir: string): string[];
  kind(rel: string): "file" | "dir" | undefined;
  read(rel: string): Buffer;
}

export function nodeFs(repoRoot: string): DocsFs {
  const abs = (rel: string): string => path.join(repoRoot, ...rel.split("/"));
  const kind = (rel: string): "file" | "dir" | undefined => {
    try {
      const stat = fs.statSync(abs(rel));
      return stat.isDirectory() ? "dir" : stat.isFile() ? "file" : undefined;
    } catch {
      return undefined;
    }
  };
  return {
    files(dir) {
      const out: string[] = [];
      const walk = (rel: string): void => {
        for (const entry of fs.readdirSync(abs(rel), { withFileTypes: true })) {
          const child = `${rel}/${entry.name}`;
          if (entry.isDirectory()) walk(child);
          else if (entry.isFile()) out.push(child);
        }
      };
      if (kind(dir) === "dir") walk(dir);
      return out.sort(compareCodeUnits);
    },
    kind,
    read(rel) {
      return fs.readFileSync(abs(rel));
    },
  };
}

/** A matcher for globs with *, ** and ?; "adr/**" also matches "adr/". */
export function globMatcher(patterns: string[]): (rel: string) => boolean {
  const regexps = patterns.map((glob) => {
    let re = "";
    for (let i = 0; i < glob.length; i++) {
      const c = glob[i];
      if (c === "*" && glob[i + 1] === "*") {
        re += ".*";
        i++;
      } else if (c === "*") re += "[^/]*";
      else if (c === "?") re += "[^/]";
      else re += c.replace(/[.+^${}()|[\]\\]/g, "\\$&");
    }
    return new RegExp(`^${re}$`);
  });
  return (rel) => regexps.some((re) => re.test(rel));
}

// ---------------------------------------------------------------------------
// Parsing

export interface Heading {
  depth: number;
  id: string;
  text: string;
  line: number;
}

export interface ParsedFile {
  /** Repository-relative, e.g. "docs/restic.md". */
  file: string;
  src: string;
  tokens: Token[];
  /** Every heading in document order, H1 included. */
  headings: Heading[];
  /** Heading IDs of H2 and deeper: what a link may point to. */
  ids: Set<string>;
  headingIds: WeakMap<Tokens.Heading, string>;
  /** The 1-based source line of each token. */
  lines: WeakMap<object, number>;
}

/** The child token lists of a token, in document order. */
export function childLists(token: Token): Token[][] {
  switch (token.type) {
    case "list":
      return [(token as Tokens.List).items];
    case "table": {
      const table = token as Tokens.Table;
      return [...table.header, ...table.rows.flat()].map((cell) => cell.tokens);
    }
    default:
      return "tokens" in token && Array.isArray(token.tokens) ? [token.tokens] : [];
  }
}

export function walkTokens(tokens: Token[], visit: (token: Token) => void): void {
  for (const token of tokens) {
    visit(token);
    for (const list of childLists(token)) walkTokens(list, visit);
  }
}

const namedEntities: Record<string, string> = {
  amp: "&",
  lt: "<",
  gt: ">",
  quot: '"',
  apos: "'",
  nbsp: "\u{a0}",
  copy: "\u{a9}",
  reg: "\u{ae}",
  trade: "\u{2122}",
  hellip: "\u{2026}",
  mdash: "\u{2014}",
  ndash: "\u{2013}",
  rarr: "\u{2192}",
  larr: "\u{2190}",
  times: "\u{d7}",
};

/** Decodes the character references marked leaves in text tokens. */
export function decodeEntities(text: string): string {
  return text.replace(/&(#x[0-9a-f]+|#[0-9]+|[a-z][a-z0-9]*);/gi, (all, ref: string) => {
    if (ref.startsWith("#")) {
      const hex = ref[1] === "x" || ref[1] === "X";
      const code = hex ? parseInt(ref.slice(2), 16) : parseInt(ref.slice(1), 10);
      return code > 0 && code <= 0x10ffff ? String.fromCodePoint(code) : all;
    }
    return namedEntities[ref] ?? all;
  });
}

/** The text a browser shows for inline tokens (images left out). */
export function plainText(tokens: Token[] | undefined): string {
  let out = "";
  for (const token of tokens ?? []) {
    switch (token.type) {
      case "text": {
        const text = token as Tokens.Text;
        out += text.tokens ? plainText(text.tokens) : decodeEntities(text.text);
        break;
      }
      case "escape":
      case "codespan":
        out += (token as Tokens.Escape | Tokens.Codespan).text;
        break;
      case "br":
        out += " ";
        break;
      case "image":
      case "html":
        break;
      default:
        if ("tokens" in token && Array.isArray(token.tokens)) out += plainText(token.tokens);
        else if ("text" in token && typeof token.text === "string") out += token.text;
    }
  }
  return out;
}

/** Finds each token's raw text forward in the source to know its line. */
class Locator {
  private readonly src: string;
  private readonly newlines: number[] = [];
  private cursor = 0;

  constructor(src: string) {
    this.src = src;
    for (let i = src.indexOf("\n"); i >= 0; i = src.indexOf("\n", i + 1)) this.newlines.push(i);
  }

  private lineAt(offset: number): number {
    let lo = 0;
    let hi = this.newlines.length;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (this.newlines[mid] < offset) lo = mid + 1;
      else hi = mid;
    }
    return lo + 1;
  }

  locate(tokens: Token[], lines: WeakMap<object, number>): void {
    for (const token of tokens) {
      const raw = token.raw;
      // A token nested in a blockquote or list may have lost its prefix;
      // then it keeps the line of the last token found.
      const start = raw && token.type !== "space" ? this.src.indexOf(raw, this.cursor) : -1;
      if (start >= 0) this.cursor = start;
      lines.set(token, this.lineAt(start >= 0 ? start : this.cursor));
      for (const list of childLists(token)) this.locate(list, lines);
      if (start >= 0) this.cursor = Math.max(this.cursor, start + raw.length);
    }
  }
}

function lex(src: string): Token[] {
  return new Lexer({ ...getDefaults(), gfm: true, breaks: false, pedantic: false }).lex(src);
}

export function parseMarkdown(file: string, raw: string): ParsedFile {
  const src = raw.replace(/\r\n?/g, "\n");
  const tokens = lex(src);
  const lines = new WeakMap<object, number>();
  new Locator(src).locate(tokens, lines);
  const slugger = new GithubSlugger();
  const headings: Heading[] = [];
  const headingIds = new WeakMap<Tokens.Heading, string>();
  const ids = new Set<string>();
  walkTokens(tokens, (token) => {
    if (token.type !== "heading") return;
    const heading = token as Tokens.Heading;
    const text = plainText(heading.tokens).trim();
    const id = slugger.slug(text);
    headingIds.set(heading, id);
    headings.push({ depth: heading.depth, id, text, line: lines.get(heading) ?? 1 });
    if (heading.depth > 1) ids.add(id);
  });
  return { file, src, tokens, headings, ids, headingIds, lines };
}

// ---------------------------------------------------------------------------
// The index and the shipped set

export interface NavItem {
  label: string;
  /** A shipped page; absent for external entries. */
  slug?: string;
  /** A GitHub URL (with the ref token) for entries that are not shipped. */
  external?: string;
  children?: NavItem[];
}

export interface NavGroup {
  title: string;
  items: NavItem[];
}

export interface Page {
  slug: string;
  /** Repository-relative, e.g. "docs/restic.md". */
  file: string;
  title: string;
  /** HTML with image placeholders (see imagePlaceholder in render.ts). */
  html: string;
  /** Repository-relative paths of the images, by placeholder index. */
  images: string[];
  /** H2 and deeper, in document order. */
  headings: Heading[];
}

export interface DocsModel {
  title: string;
  githubRepo: string;
  groups: NavGroup[];
  /** Reading order: the index first, then the index's items depth first. */
  pages: Page[];
  search: SearchIndex;
}

export interface DocsBuild {
  model: DocsModel;
  errors: DocErrors;
}

/** The slug of a shipped page: its path without .md; a README is its folder. */
export function slugOf(docRel: string): string {
  const noExt = docRel.replace(/\.md$/, "");
  if (noExt === "README") return "";
  return noExt.endsWith("/README") ? noExt.slice(0, -"/README".length) : noExt;
}

const SLUG_RE = /^[A-Za-z0-9._-]+(\/[A-Za-z0-9._-]+)*$/;

interface IndexItem {
  label: string;
  href: string;
  line: number;
  children: IndexItem[];
}

interface ParsedIndex {
  title: string;
  groups: { title: string; items: IndexItem[] }[];
}

const isHeading = (token: Token | undefined, maxDepth: number): boolean =>
  token?.type === "heading" && (token as Tokens.Heading).depth <= maxDepth;

function parseIndex(index: ParsedFile, errors: DocErrors): ParsedIndex {
  const file = index.file;
  const line = (token: object): number => index.lines.get(token) ?? 1;
  const blocks = index.tokens.filter((t) => t.type !== "space");
  const first = blocks.at(0);
  let title = "";
  if (first && isHeading(first, 1)) title = plainText((first as Tokens.Heading).tokens).trim();
  else errors.add(file, first ? line(first) : 1, 'the index starts with "# Title"');

  const parseItems = (list: Tokens.List, depth: number): IndexItem[] => {
    if (list.ordered) errors.add(file, line(list), "each section holds one bullet list of links");
    const items: IndexItem[] = [];
    for (const item of list.items) {
      const parts = item.tokens.filter((t) => t.type !== "space");
      const head = parts.at(0);
      const inline =
        head?.type === "text" || head?.type === "paragraph"
          ? ((head as Tokens.Text).tokens ?? [])
          : [];
      const link = inline.at(0);
      if (link?.type !== "link") {
        errors.add(file, line(item), "each item starts with a link to a page");
        continue;
      }
      const rest = plainText(inline.slice(1)).trim();
      if (rest !== "" && !rest.startsWith(":")) {
        errors.add(file, line(item), 'write the description after the link and ": "');
      }
      const entry: IndexItem = {
        label: plainText((link as Tokens.Link).tokens).trim(),
        href: (link as Tokens.Link).href,
        line: line(link),
        children: [],
      };
      for (const extra of parts.slice(1)) {
        if (extra.type === "list" && depth === 0)
          entry.children.push(...parseItems(extra as Tokens.List, 1));
        else
          errors.add(
            file,
            line(extra),
            "an item holds a link, a description and at most one nested list",
          );
      }
      items.push(entry);
    }
    return items;
  };

  const groups: ParsedIndex["groups"] = [];
  let i = 1;
  while (i < blocks.length && !isHeading(blocks[i], 6)) i++; // the intro
  for (; i < blocks.length; i++) {
    const heading = blocks[i];
    if (!isHeading(heading, 2) || (heading as Tokens.Heading).depth !== 2) {
      errors.add(file, line(heading), 'each section starts with a "## Heading"');
      continue;
    }
    const body: Token[] = [];
    while (i + 1 < blocks.length && !isHeading(blocks[i + 1], 6)) body.push(blocks[++i]);
    const group = {
      title: plainText((heading as Tokens.Heading).tokens).trim(),
      items: [] as IndexItem[],
    };
    if (body.length !== 1 || body[0].type !== "list") {
      errors.add(
        file,
        line(body.find((t) => t.type !== "list") ?? heading),
        "each section holds one bullet list of links",
      );
    }
    for (const token of body)
      if (token.type === "list") group.items.push(...parseItems(token as Tokens.List, 0));
    groups.push(group);
  }
  if (groups.length === 0 && first)
    errors.add(file, line(first), 'the index needs a "## Section" with a list of links');
  return { title, groups };
}

/**
 * Builds the whole model and collects every error. With errors the model is
 * best effort (the dev server keeps serving the last good one).
 */
export function buildDocs(
  options: DocsOptions,
  docsFs: DocsFs = nodeFs(options.repoRoot),
): DocsBuild {
  const errors = new DocErrors();
  const docsRel = relativePosix(options.repoRoot, options.docsDir);
  if (docsRel === "" || docsRel.startsWith("..") || path.isAbsolute(docsRel)) {
    throw new Error("docsDir must be a folder inside repoRoot");
  }
  const isGithubOnly = globMatcher(options.githubOnly);
  const toDocRel = (rel: string): string => rel.slice(docsRel.length + 1);
  const docFiles = new Set(docsFs.files(docsRel));
  const docDirs = new Set<string>([docsRel]);
  for (const f of docFiles) {
    for (let d = path.posix.dirname(f); d !== docsRel && d !== "."; d = path.posix.dirname(d))
      docDirs.add(d);
  }

  const parsed = new Map<string, ParsedFile>();
  const parse = (rel: string): ParsedFile => {
    let p = parsed.get(rel);
    if (!p) {
      p = parseMarkdown(rel, docsFs.read(rel).toString("utf8"));
      parsed.set(rel, p);
    }
    return p;
  };

  const indexFile = `${docsRel}/README.md`;
  const shipped = new Map<string, string>(); // file -> slug
  const site: Site = {
    options,
    fs: docsFs,
    docsRel,
    docFiles,
    docDirs,
    shipped,
    githubOnly: (rel) => rel.startsWith(`${docsRel}/`) && isGithubOnly(toDocRel(rel)),
    headingIds(rel) {
      if (!rel.endsWith(".md")) return undefined;
      const inDocs = rel.startsWith(`${docsRel}/`);
      if (inDocs ? !docFiles.has(rel) : docsFs.kind(rel) !== "file") return undefined;
      return parse(rel).ids;
    },
  };

  const model: DocsModel = {
    title: "",
    githubRepo: options.githubRepo,
    groups: [],
    pages: [],
    search: { pages: [], sections: [] },
  };
  if (!docFiles.has(indexFile)) {
    errors.add(indexFile, 1, "missing: docs/README.md is the index of the docs");
    return { model, errors };
  }
  const parsedIndex = parseIndex(parse(indexFile), errors);
  model.title = parsedIndex.title;

  const order: string[] = [indexFile];
  const listedOn = new Map<string, number>([[indexFile, 1]]);
  shipped.set(indexFile, "");

  const toNav = (item: IndexItem): NavItem | undefined => {
    const target = resolveLink(site, indexFile, item.href, { forIndex: true });
    if (target.kind === "error") {
      errors.add(indexFile, item.line, target.message);
      return undefined;
    }
    let nav: NavItem;
    if (target.kind === "page") {
      const first = listedOn.get(target.file);
      if (first !== undefined) {
        errors.add(indexFile, item.line, `${target.file} is listed twice (first on line ${first})`);
        return undefined;
      }
      listedOn.set(target.file, item.line);
      const slug = slugOf(toDocRel(target.file));
      if (!SLUG_RE.test(slug)) {
        errors.add(
          target.file,
          1,
          "the file name of a page may only use letters, digits, '.', '_' and '-'",
        );
      }
      const clash = [...shipped].find(([, s]) => s === slug);
      if (clash)
        errors.add(
          indexFile,
          item.line,
          `${target.file} and ${clash[0]} would both be /docs/${slug}`,
        );
      shipped.set(target.file, slug);
      order.push(target.file);
      nav = { label: item.label, slug };
    } else {
      nav = { label: item.label, external: target.href };
    }
    const children = item.children.map(toNav).filter((c): c is NavItem => c !== undefined);
    if (children.length > 0) nav.children = children;
    return nav;
  };
  model.groups = parsedIndex.groups.map((g) => ({
    title: g.title,
    items: g.items.map(toNav).filter((c): c is NavItem => c !== undefined),
  }));

  for (const f of docFiles) {
    if (/\.md$/i.test(f) && !f.endsWith(".md")) {
      errors.add(
        f,
        1,
        'write the extension as ".md": other spellings are neither shipped nor checked',
      );
    } else if (f.endsWith(".md") && !shipped.has(f) && !site.githubOnly(f)) {
      errors.add(f, 1, "not listed in docs/README.md (or in githubOnly in web/vite.config.ts)");
    }
  }

  for (const file of order) {
    const page = parse(file);
    const title = checkPage(page, errors);
    const rendered = renderPage(page, site, errors);
    model.pages.push({
      slug: shipped.get(file) ?? "",
      file,
      title,
      html: rendered.html,
      images: rendered.images,
      headings: page.headings.filter((h) => h.depth > 1),
    });
  }
  model.search = buildSearchIndex(model.pages.map((page) => ({ page, parsed: parse(page.file) })));
  return { model, errors };
}
