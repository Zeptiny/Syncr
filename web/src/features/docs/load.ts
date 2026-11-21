// What the /docs routes run before rendering. The routes import this module
// dynamically, so the manifest stays out of the app's entry chunk.
import type { Transport } from "@connectrpc/connect";
import type { QueryClient } from "@tanstack/react-query";
import { notFound, redirect } from "@tanstack/react-router";
import type { DocContent } from "virtual:syncr-docs/manifest";

import { controllerInfoOptions } from "@/hooks/use-controller-info";

import type { DocPage } from "./docs.gen";
import { canonicalSlug, isPage, loadPage } from "./manifest";

/** LoadedDoc is the loader data of a docs page route. */
export interface LoadedDoc {
  slug: DocPage;
  content: DocContent;
}

/** LoadContext is what a docs loader needs from the router context. */
export interface LoadContext {
  queryClient: QueryClient;
  transport: Transport;
}

/**
 * load loads a page together with the controller's version, which pins its
 * GitHub links: the page renders once, with its final links. A failing
 * GetInfo only means links to main.
 */
async function load(slug: DocPage, ctx: LoadContext): Promise<LoadedDoc> {
  const [content] = await Promise.all([
    loadPage(slug),
    ctx.queryClient.ensureQueryData(controllerInfoOptions(ctx.transport)).catch(() => undefined),
  ]);
  return { slug, content };
}

/** loadIndex loads the index page (/docs). */
export function loadIndex(ctx: LoadContext): Promise<LoadedDoc> {
  return load("", ctx);
}

/**
 * loadSplat loads the page at /docs/<splat>. A GitHub-style path redirects to
 * the page's address; an unknown page is not found.
 */
export async function loadSplat(splat: string, hash: string, ctx: LoadContext): Promise<LoadedDoc> {
  const slug = canonicalSlug(splat);
  if (slug !== splat) {
    const to = hash ? { hash } : {};
    if (slug === "") throw redirect({ to: "/docs", replace: true, ...to });
    throw redirect({ to: "/docs/$", params: { _splat: slug }, replace: true, ...to });
  }
  if (slug === "" || !isPage(slug)) throw notFound();
  return load(slug, ctx);
}
