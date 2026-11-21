import { createFileRoute } from "@tanstack/react-router";

import { DocNotFound } from "@/features/docs/doc-not-found";
import { DocPageView } from "@/features/docs/doc-page";

export const Route = createFileRoute("/docs/$")({
  // Imported here, not at the top: the docs stay out of the app's entry chunk.
  loader: async ({ params, location, context }) =>
    (await import("@/features/docs/load")).loadSplat(params._splat ?? "", location.hash, context),
  component: DocsPage,
  notFoundComponent: DocNotFound,
});

function DocsPage() {
  const { slug, content } = Route.useLoaderData();
  return <DocPageView key={slug} slug={slug} content={content} />;
}
