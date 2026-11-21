import { createFileRoute } from "@tanstack/react-router";

import { DocPageView } from "@/features/docs/doc-page";

export const Route = createFileRoute("/docs/")({
  // Imported here, not at the top: the docs stay out of the app's entry chunk.
  loader: async ({ context }) => (await import("@/features/docs/load")).loadIndex(context),
  component: DocsIndex,
});

function DocsIndex() {
  const { slug, content } = Route.useLoaderData();
  return <DocPageView slug={slug} content={content} />;
}
