// Search inside the docs layout: one dialog, the buttons that open it, and
// the shortcuts (/ and Ctrl/Cmd+K), which exist only on the docs pages.
import { Search } from "lucide-react";
import { createContext, use, useEffect, useMemo, useState } from "react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

import { loadSearchIndex, SearchDialog } from "./search-dialog";

interface DocsSearch {
  openSearch: () => void;
}

const DocsSearchContext = createContext<DocsSearch>({ openSearch: () => {} });

/** useDocsSearch opens the docs search dialog. */
export function useDocsSearch(): DocsSearch {
  return use(DocsSearchContext);
}

/** isTyping reports whether a key press goes into a text field. */
function isTyping(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  if (target.isContentEditable) return true;
  return target.closest("input, textarea, select, [role='combobox']") !== null;
}

/** DocsSearchProvider renders the search dialog and listens for its shortcuts. */
export function DocsSearchProvider({ children }: { children: React.ReactNode }) {
  const [open, setOpen] = useState(false);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.defaultPrevented || e.isComposing) return;
      const k = e.key.toLowerCase() === "k" && (e.ctrlKey || e.metaKey) && !e.altKey && !e.shiftKey;
      const slash = e.key === "/" && !e.ctrlKey && !e.metaKey && !e.altKey && !isTyping(e.target);
      if (!k && !slash) return;
      e.preventDefault();
      setOpen(true);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);
  const value = useMemo(() => ({ openSearch: () => setOpen(true) }), []);
  return (
    <DocsSearchContext value={value}>
      {children}
      <SearchDialog open={open} onOpenChange={setOpen} />
    </DocsSearchContext>
  );
}

const preload = () => {
  loadSearchIndex().catch(() => {
    // The dialog shows the error when it opens.
  });
};

/** SearchButton opens the docs search; pointing at it starts loading the index. */
export function SearchButton({ className, compact }: { className?: string; compact?: boolean }) {
  const { openSearch } = useDocsSearch();
  return (
    <Button
      type="button"
      variant="outline"
      size="sm"
      onClick={openSearch}
      onPointerEnter={preload}
      onFocus={preload}
      aria-keyshortcuts="/ Control+K Meta+K"
      className={cn("justify-start text-muted-foreground", !compact && "w-full", className)}
    >
      <Search aria-hidden />
      <span className={cn(compact && "max-sm:sr-only")}>Search</span>
      <kbd
        aria-hidden
        className={cn(
          "ml-auto rounded border bg-muted px-1.5 font-mono text-[11px] leading-4",
          compact && "max-sm:hidden",
        )}
      >
        /
      </kbd>
    </Button>
  );
}
