// Collects build errors as "docs/<file>:<line>: <message>" and holds the
// checks that look at a page as a whole.

import type { Tokens } from "marked";

import { plainText, walkTokens, type ParsedFile } from "./model.ts";

export interface DocError {
  /** Repository-relative, e.g. "docs/restic.md". */
  file: string;
  line: number;
  message: string;
}

export class DocErrors {
  readonly list: DocError[] = [];

  add(file: string, line: number, message: string): void {
    this.list.push({ file, line, message });
  }

  get size(): number {
    return this.list.length;
  }

  /** Sorted by file, line and message; duplicates dropped. */
  lines(): string[] {
    const sorted = [...this.list].sort(
      (a, b) =>
        (a.file < b.file ? -1 : a.file > b.file ? 1 : 0) ||
        a.line - b.line ||
        (a.message < b.message ? -1 : a.message > b.message ? 1 : 0),
    );
    return [...new Set(sorted.map((e) => `${e.file}:${e.line}: ${e.message}`))];
  }

  format(): string {
    const lines = this.lines();
    const n = lines.length;
    return [`${n} problem${n === 1 ? "" : "s"} in the docs:`, ...lines.map((l) => `  ${l}`)].join(
      "\n",
    );
  }
}

/** IDs the app itself uses; a heading may not take them. */
export const RESERVED_IDS: ReadonlySet<string> = new Set(["root", "main-content", "doc-content"]);

/** Shortens a quoted value for a message. */
export function short(value: string, max = 60): string {
  const first = value.split("\n")[0];
  return first.length > max || first !== value ? `${first.slice(0, max)}...` : first;
}

function distance(a: string, b: string): number {
  let prev = Array.from({ length: b.length + 1 }, (_, i) => i);
  for (let i = 1; i <= a.length; i++) {
    const cur = [i];
    for (let j = 1; j <= b.length; j++) {
      cur[j] = Math.min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + (a[i - 1] === b[j - 1] ? 0 : 1));
    }
    prev = cur;
  }
  return prev[b.length];
}

/** The candidate most like target: one that starts with it, else the nearest by edit distance. */
export function closest(target: string, candidates: Iterable<string>): string | undefined {
  let best: string | undefined;
  let bestScore = Infinity;
  for (const c of candidates) {
    const score = c.startsWith(target) || target.startsWith(c) ? 0 : distance(target, c);
    if (score < bestScore) {
      best = c;
      bestScore = score;
    }
  }
  return bestScore <= Math.max(3, target.length / 2) ? best : undefined;
}

export function rawHtmlMessage(raw: string): string {
  const html = short(raw.trim(), 40);
  const escaped = html.replace(/[<>]/g, "\\$&");
  return `raw HTML "${html}" is not allowed: put it in backticks or escape it (${escaped})`;
}

const ALERT_RE = /^\s*>\s*\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\]/i;

/**
 * The checks on a page as a whole: one H1 at the start (returned as the
 * title), no reserved heading IDs, no GitHub alerts and no raw HTML.
 */
export function checkPage(page: ParsedFile, errors: DocErrors): string {
  const line = (token: object): number => page.lines.get(token) ?? 1;
  const blocks = page.tokens.filter((t) => t.type !== "space");
  const first = blocks.at(0);
  const h1s = page.headings.filter((h) => h.depth === 1);
  let title: string;
  if (first?.type === "heading" && (first as Tokens.Heading).depth === 1 && h1s.length === 1) {
    title = plainText((first as Tokens.Heading).tokens).trim();
  } else {
    errors.add(page.file, h1s.at(1)?.line ?? 1, 'a page starts with exactly one "# Title"');
    title = h1s.at(0)?.text ?? page.file;
  }
  for (const h of page.headings) {
    if (h.depth > 1 && RESERVED_IDS.has(h.id)) {
      errors.add(page.file, h.line, `heading ID "${h.id}" is reserved by the app`);
    }
  }
  walkTokens(page.tokens, (token) => {
    if (token.type === "html") {
      errors.add(page.file, line(token), rawHtmlMessage((token as Tokens.HTML).raw));
    } else if (token.type === "blockquote") {
      const alert = ALERT_RE.exec(token.raw);
      if (alert) {
        errors.add(
          page.file,
          line(token),
          `"> [!${alert[1].toUpperCase()}]" alerts are not supported in the app; write a plain blockquote`,
        );
      }
    }
  });
  return title;
}
