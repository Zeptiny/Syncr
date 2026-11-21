// Docs search in the browser over the build-time index (design section 5):
// every query word must match the start of a word in the section's page
// title, heading or text; matches in titles and headings rank higher.

import type { DocSearchIndex } from "virtual:syncr-docs/search";

import type { DocPage } from "./docs.gen";

/** normalize lowercases and drops diacritics, so "Resumé" finds "resume". */
export function normalize(text: string): string {
  return text.toLowerCase().normalize("NFKD").replace(/\p{M}/gu, "");
}

const WORD_SPLIT = /[^\p{L}\p{N}]+/u;
const WORD_CHAR = /[\p{L}\p{N}]/u;

/** words splits text into normalized words. */
export function words(text: string): string[] {
  return normalize(text)
    .split(WORD_SPLIT)
    .filter((w) => w !== "");
}

interface PreparedSection {
  page: number;
  anchor: string;
  heading: string;
  text: string;
  titleWords: string[];
  headingWords: string[];
  bodyWords: string[];
}

/** PreparedIndex is the index with its normalized words, computed once. */
export interface PreparedIndex {
  pages: { slug: DocPage; title: string }[];
  sections: PreparedSection[];
}

/** prepareIndex normalizes the index once, when it is first loaded. */
export function prepareIndex(index: DocSearchIndex): PreparedIndex {
  const titleWords = index.pages.map((p) => words(p.title));
  return {
    pages: index.pages,
    sections: index.sections.map(([page, anchor, heading, text]) => ({
      page,
      anchor,
      heading,
      text,
      titleWords: titleWords[page] ?? [],
      headingWords: words(heading),
      bodyWords: words(text),
    })),
  };
}

/** Segment is a piece of a snippet; match marks the text a query word matched. */
export interface Segment {
  text: string;
  match: boolean;
}

/** SearchResult is one section that matched every word of the query. */
export interface SearchResult {
  slug: DocPage;
  /** The heading ID, or "" for the text before the page's first heading. */
  anchor: string;
  page: string;
  heading: string;
  snippet: Segment[];
  score: number;
}

export const MAX_RESULTS = 20;
const SNIPPET_LENGTH = 120;

/** Points per query word (design section 5). */
const POINTS = { title: 10, heading: 6, exact: 2, prefix: 1 };

const hasPrefix = (list: string[], q: string): boolean => list.some((w) => w.startsWith(q));

/**
 * searchDocs returns up to limit sections that match every word of query,
 * best first; equal scores keep the index order.
 */
export function searchDocs(
  index: PreparedIndex,
  query: string,
  limit = MAX_RESULTS,
): SearchResult[] {
  const terms = [...new Set(words(query))];
  if (terms.length === 0) return [];
  const scored: { at: number; score: number }[] = [];
  index.sections.forEach((section, at) => {
    let score = 0;
    for (const q of terms) {
      const inTitle = hasPrefix(section.titleWords, q);
      const inHeading = hasPrefix(section.headingWords, q);
      let exact = 0;
      let prefix = 0;
      for (const w of section.bodyWords) {
        if (!w.startsWith(q)) continue;
        prefix++;
        if (w.length === q.length) exact++;
      }
      if (!inTitle && !inHeading && prefix === 0) return;
      if (inTitle) score += POINTS.title;
      if (inHeading) score += POINTS.heading;
      if (exact > 0) score += POINTS.exact;
      else if (prefix > 0) score += POINTS.prefix;
      score += Math.log2(1 + prefix);
    }
    scored.push({ at, score });
  });
  scored.sort((a, b) => b.score - a.score || a.at - b.at);
  return scored.slice(0, limit).map(({ at, score }) => {
    const section = index.sections[at];
    const page = index.pages[section.page];
    return {
      slug: page.slug,
      anchor: section.anchor,
      page: page.title,
      heading: section.heading,
      snippet: snippet(section.text, terms),
      score,
    };
  });
}

/** normalizeWithMap normalizes text and maps each normalized character to its source index. */
function normalizeWithMap(text: string): { norm: string; map: number[] } {
  let norm = "";
  const map: number[] = [];
  let at = 0;
  for (const ch of text) {
    const piece = normalize(ch);
    norm += piece;
    for (let i = 0; i < piece.length; i++) map.push(at);
    at += ch.length;
  }
  return { norm, map };
}

/** wordMatches lists [start, end) ranges of norm where a word starts with a term. */
function wordMatches(norm: string, terms: string[]): [number, number][] {
  const out: [number, number][] = [];
  for (let i = 0; i < norm.length; i++) {
    if (!WORD_CHAR.test(norm[i]) || (i > 0 && WORD_CHAR.test(norm[i - 1]))) continue;
    let longest = 0;
    for (const q of terms) if (q.length > longest && norm.startsWith(q, i)) longest = q.length;
    if (longest > 0) out.push([i, i + longest]);
  }
  return out;
}

/**
 * snippet cuts about SNIPPET_LENGTH characters of text around the first
 * match, and marks every match inside it. Without a match in the text (the
 * title or heading matched) it is the start of the text.
 */
export function snippet(text: string, terms: string[], length = SNIPPET_LENGTH): Segment[] {
  if (text === "") return [];
  const { norm, map } = normalizeWithMap(text);
  const matches = wordMatches(norm, terms).map(([s, e]): [number, number] => [
    map[s],
    map[e - 1] + 1,
  ]);
  let start = 0;
  if (matches.length > 0) {
    start = Math.max(0, matches[0][0] - Math.floor(length / 3));
    if (start > 0) {
      const space = text.indexOf(" ", start);
      if (space >= 0 && space < matches[0][0]) start = space + 1;
    }
  }
  let end = Math.min(text.length, start + length);
  if (end < text.length) {
    const space = text.lastIndexOf(" ", end);
    if (space > start && space > (matches[0]?.[1] ?? 0)) end = space;
  }
  const segments: Segment[] = [];
  const push = (s: string, match: boolean) => {
    if (s !== "") segments.push({ text: s, match });
  };
  let at = start;
  for (const [s, e] of matches) {
    if (e <= at || s >= end) continue;
    push(text.slice(at, Math.max(at, s)), false);
    push(text.slice(Math.max(at, s), Math.min(e, end)), true);
    at = Math.min(e, end);
  }
  push(text.slice(at, end), false);
  if (start > 0) segments.unshift({ text: "… ", match: false });
  if (end < text.length) segments.push({ text: " …", match: false });
  return segments;
}
