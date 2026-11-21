import "./prose.css";

import { useRouter } from "@tanstack/react-router";
import { PanelLeft } from "lucide-react";
import { createContext, use, useState } from "react";

import { Button } from "@/components/ui/button";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";

import { DocsNav } from "./docs-nav";
import { SearchButton } from "./docs-search";
import { VersionLabel } from "./version-label";

const FirstLocationContext = createContext<string | undefined>(undefined);

/**
 * useFirstLocationKey is the history key of the first docs page shown: that
 * page keeps the browser's focus and scroll, later ones move them.
 */
export function useFirstLocationKey(): string | undefined {
  return use(FirstLocationContext);
}

/**
 * DocsLayout is the docs' own layout inside AppShell or PublicDocsLayout: the
 * navigation on the left (a sheet behind "Contents" below lg) and the page.
 * showSearch adds the Search button next to the navigation (the public
 * header has its own).
 */
export function DocsLayout({
  children,
  showSearch = true,
}: {
  children: React.ReactNode;
  showSearch?: boolean;
}) {
  const router = useRouter();
  const [firstKey] = useState(() => router.state.location.state.__TSR_key);
  const [sheetOpen, setSheetOpen] = useState(false);

  return (
    <FirstLocationContext value={firstKey}>
      <div className="lg:grid lg:grid-cols-[13.5rem_minmax(0,1fr)] lg:gap-10">
        <aside className="hidden lg:block">
          <div className="sticky top-[var(--docs-sticky-top,1.5rem)] -mx-2.5 flex max-h-[calc(100svh-var(--docs-sticky-top,1.5rem)-1.5rem)] flex-col gap-3 overflow-y-auto px-2.5 pb-4">
            <VersionLabel className="px-2.5" />
            {showSearch && <SearchButton />}
            <DocsNav />
          </div>
        </aside>
        <div className="min-w-0">
          <div className="mb-5 flex items-center gap-2 lg:hidden">
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => setSheetOpen(true)}
              aria-haspopup="dialog"
            >
              <PanelLeft aria-hidden />
              Contents
            </Button>
            {showSearch && <SearchButton compact />}
            <VersionLabel className="ml-auto truncate" />
          </div>
          {children}
        </div>
      </div>
      <Sheet open={sheetOpen} onOpenChange={setSheetOpen}>
        <SheetContent side="left" className="w-[min(20rem,85vw)] gap-0 overflow-y-auto">
          <SheetHeader className="border-b">
            <SheetTitle>Contents</SheetTitle>
            <SheetDescription className="sr-only">The pages of the documentation</SheetDescription>
          </SheetHeader>
          <div className="p-3">
            <DocsNav onNavigate={() => setSheetOpen(false)} />
          </div>
        </SheetContent>
      </Sheet>
    </FirstLocationContext>
  );
}
