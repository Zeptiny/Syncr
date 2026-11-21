import { AppShell } from "@/components/app/app-shell";

import { DocsLayout } from "./docs-layout";
import { DocsSearchProvider } from "./docs-search";
import { PublicDocsLayout } from "./public-docs-layout";

/**
 * DocsRoot is the /docs layout: inside the app shell for a signed-in user,
 * in the public frame otherwise. Both get search and the docs navigation.
 */
export function DocsRoot({ signedIn, children }: { signedIn: boolean; children: React.ReactNode }) {
  return (
    <DocsSearchProvider>
      {signedIn ? (
        <AppShell>
          <DocsLayout>{children}</DocsLayout>
        </AppShell>
      ) : (
        <PublicDocsLayout>
          <DocsLayout showSearch={false}>{children}</DocsLayout>
        </PublicDocsLayout>
      )}
    </DocsSearchProvider>
  );
}
