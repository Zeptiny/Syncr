// The docs manifest and what the viewer derives from it. This module and
// everything that imports it load with the docs routes, never with the app.
import manifest, { type DocContent, type DocHtml } from "virtual:syncr-docs/manifest";

import type { DocPage } from "./docs.gen";
import { isSafeRef } from "./version";

export { manifest };

/**
 * pinRef replaces the GitHub ref token in rendered HTML or a URL with ref,
 * which must be a release tag or main (githubRef); anything else gives main.
 */
export function pinRef<T extends string>(text: T, ref: string): T {
  const safe = isSafeRef(ref) ? ref : "main";
  return text.replaceAll(manifest.githubRefToken, safe) as T;
}

/** pinHtml is pinRef for page HTML: the result is still build output. */
export function pinHtml(html: DocHtml, ref: string): DocHtml {
  return pinRef(html, ref);
}

/** githubFileUrl is a file of the repository on GitHub at ref. */
export function githubFileUrl(file: string, ref: string): string {
  return pinRef(`${manifest.githubRepo}/blob/${manifest.githubRefToken}/${file}`, ref);
}

/** pageInfo is a page's entry in reading order with its neighbours. */
export function pageInfo(slug: DocPage) {
  const i = manifest.pages.findIndex((p) => p.slug === slug);
  return {
    page: manifest.pages[i],
    previous: i > 0 ? manifest.pages[i - 1] : undefined,
    next: i >= 0 && i < manifest.pages.length - 1 ? manifest.pages[i + 1] : undefined,
  };
}

/** isPage reports whether slug is a shipped page (a URL segment is untrusted). */
export function isPage(slug: string): slug is DocPage {
  return Object.hasOwn(manifest.loaders, slug);
}

/** loadPage loads a page's chunk. */
export async function loadPage(slug: DocPage): Promise<DocContent> {
  return (await manifest.loaders[slug]()).default;
}

/**
 * canonicalSlug maps what may follow /docs/ in a URL to a page slug: GitHub
 * style paths (restic.md, releases/README.md) and stray slashes are
 * accepted, so links copied from GitHub keep working.
 */
export function canonicalSlug(splat: string): string {
  let slug = splat.replace(/^\/+|\/+$/g, "");
  if (slug.toLowerCase().endsWith(".md")) slug = slug.slice(0, -3);
  if (slug === "README") return "";
  if (slug.endsWith("/README")) slug = slug.slice(0, -"/README".length);
  return slug;
}
