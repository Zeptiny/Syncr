import { BookOpen, Compass, Search } from "lucide-react";

import { EmptyState } from "@/components/app/empty-state";
import { Button } from "@/components/ui/button";

import { DocLink } from "./doc-link";
import { useDocsSearch } from "./docs-search";
import { useDocumentTitle } from "./use-document-title";

/** DocNotFound is shown inside the docs layout for an address that is no page. */
export function DocNotFound() {
  const { openSearch } = useDocsSearch();
  useDocumentTitle("Page not found · Syncr docs");
  return (
    <EmptyState
      icon={Compass}
      title="No page at this address"
      description="The link may be old, or the page was renamed. Search the docs or start from the overview."
      action={
        <>
          <Button type="button" variant="outline" onClick={openSearch}>
            <Search aria-hidden />
            Search the docs
          </Button>
          <Button asChild>
            <DocLink page="">
              <BookOpen aria-hidden />
              All documentation
            </DocLink>
          </Button>
        </>
      }
    />
  );
}
