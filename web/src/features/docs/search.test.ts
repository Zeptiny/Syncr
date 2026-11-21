import type { DocSearchIndex } from "virtual:syncr-docs/search";
import { describe, expect, it } from "vitest";

import { normalize, prepareIndex, searchDocs, snippet, words, type Segment } from "./search";

const fixture: DocSearchIndex = {
  pages: [
    { slug: "remotes", title: "Remotes" },
    { slug: "restic", title: "Restic backups" },
    { slug: "hooks", title: "Hooks" },
  ],
  sections: [
    [0, "", "", "A remote is a place jobs read from and write to."],
    [0, "sftp", "SFTP", "Host, user and a password or a key. Backups over SFTP work too."],
    [1, "", "", "Syncr runs restic on your workers and keeps your backups safe."],
    [1, "restore", "Restore", "Pick a snapshot and restore files to a worker folder."],
    [2, "examples", "Examples", "A database dump, then a backup of the dump. Café crème."],
    [2, "windows", "Windows", "Backup hooks run in PowerShell on Windows workers."],
  ],
};

const index = prepareIndex(fixture);
const found = (q: string) => searchDocs(index, q).map((r) => `${r.slug}#${r.anchor}`);
const text = (s: Segment[]) => s.map((x) => (x.match ? `[${x.text}]` : x.text)).join("");

describe("normalize and words", () => {
  it("lowercases and drops diacritics", () => {
    expect(normalize("Café CRÈME")).toBe("cafe creme");
    expect(words("data_dir/sources, v2.3.0!")).toEqual(["data", "dir", "sources", "v2", "3", "0"]);
  });
});

describe("searchDocs", () => {
  it("needs every word to match the start of a word", () => {
    expect(found("restore worker")).toEqual(["restic#restore"]);
    // The title and the heading both match "rest" in the Restore section.
    expect(found("rest")).toEqual(["restic#restore", "restic#"]);
    // "estore" is inside a word, not at its start.
    expect(found("estore")).toEqual([]);
    expect(found("restore database")).toEqual([]);
  });

  it("ranks title over heading over an exact word over a prefix", () => {
    // "backup": page title (prefix of "backups") beats the exact word in a body.
    expect(found("backup")).toEqual([
      "restic#",
      "restic#restore",
      "hooks#examples",
      "hooks#windows",
      "remotes#sftp",
    ]);
    // A heading beats the body; the title of "Hooks" makes both of its sections match.
    expect(found("windows")).toEqual(["hooks#windows"]);
    expect(found("hook")[0]).toBe("hooks#windows");
  });

  it("keeps the index order for equal scores", () => {
    // Only the page title matches "remotes" (the body says "remote").
    const both = searchDocs(index, "remotes");
    expect(both.map((r) => r.anchor)).toEqual(["", "sftp"]);
    expect(both[0].score).toBe(both[1].score);
  });

  it("matches without diacritics and returns page and heading", () => {
    const [hit] = searchDocs(index, "cafe");
    expect(hit).toMatchObject({
      slug: "hooks",
      anchor: "examples",
      page: "Hooks",
      heading: "Examples",
    });
    expect(text(hit.snippet)).toContain("[Café]");
  });

  it("ignores empty queries and caps the results", () => {
    expect(searchDocs(index, "  ,. ")).toEqual([]);
    expect(searchDocs(index, "a", 2)).toHaveLength(2);
  });
});

describe("snippet", () => {
  it("cuts around the first match and marks every match", () => {
    const long = `${"lorem ".repeat(40)}the backup ran and the backups were kept ${"ipsum ".repeat(40)}`;
    const s = text(snippet(long, ["backup"], 80));
    expect(s.startsWith("… ")).toBe(true);
    expect(s.endsWith(" …")).toBe(true);
    expect(s).toContain("the [backup] ran and the [backup]s were kept");
    expect(s.length).toBeLessThanOrEqual(80 + 4);
  });

  it("starts at the beginning without a match in the text", () => {
    expect(text(snippet("Short text.", ["title"]))).toBe("Short text.");
    expect(snippet("", ["x"])).toEqual([]);
  });
});
