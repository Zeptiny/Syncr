// The search index: each shipped page split at H2 and H3 into sections of
// plain text. The viewer searches it in the browser (design section 5).

import type { Token, Tokens } from "marked";

import { plainText, type Page, type ParsedFile } from "./model.ts";

/** [page index, anchor ("" for the intro), heading ("" for the intro), text] */
export type SearchSection = [number, string, string, string];

export interface SearchIndex {
  pages: { slug: string; title: string }[];
  sections: SearchSection[];
}

/** The plain text of block tokens: prose, list items, table cells and code. */
export function blockText(tokens: Token[]): string {
  const parts: string[] = [];
  for (const token of tokens) {
    switch (token.type) {
      case "space":
      case "hr":
      case "html":
      case "def":
        break;
      case "code":
        parts.push((token as Tokens.Code).text);
        break;
      case "list":
        for (const item of (token as Tokens.List).items) parts.push(blockText(item.tokens));
        break;
      case "blockquote":
      case "list_item":
        parts.push(blockText((token as Tokens.Blockquote).tokens));
        break;
      case "table": {
        const table = token as Tokens.Table;
        for (const cell of [...table.header, ...table.rows.flat()])
          parts.push(plainText(cell.tokens));
        break;
      }
      default:
        parts.push(plainText([token]));
    }
  }
  return parts.join(" ");
}

const squash = (text: string): string => text.replace(/\s+/g, " ").trim();

/** The index of the shipped pages; the index page itself (slug "") is left out. */
export function buildSearchIndex(pages: { page: Page; parsed: ParsedFile }[]): SearchIndex {
  const index: SearchIndex = { pages: [], sections: [] };
  for (const { page, parsed } of pages) {
    if (page.slug === "") continue;
    const pageIndex = index.pages.push({ slug: page.slug, title: page.title }) - 1;
    let anchor = "";
    let heading = "";
    let body: Token[] = [];
    const flush = (): void => {
      const text = squash(blockText(body));
      if (text !== "" || anchor !== "") index.sections.push([pageIndex, anchor, heading, text]);
    };
    for (const token of parsed.tokens) {
      if (token.type === "heading") {
        const h = token as Tokens.Heading;
        if (h.depth === 1) continue;
        if (h.depth <= 3) {
          flush();
          anchor = parsed.headingIds.get(h) ?? "";
          heading = squash(plainText(h.tokens));
          body = [];
          continue;
        }
      }
      body.push(token);
    }
    flush();
  }
  return index;
}
