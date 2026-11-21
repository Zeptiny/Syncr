import { ChevronDown, TableOfContents } from "lucide-react";
import type { DocHeading } from "virtual:syncr-docs/manifest";
import { useEffect, useMemo, useState } from "react";

import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { cn } from "@/lib/utils";

import { useDocLinkHandlers } from "./doc-navigation";

/**
 * useActiveHeading follows the heading being read: the first one in the
 * top part of the window, else the last one above it. Without
 * IntersectionObserver (old browsers, jsdom) nothing is marked.
 */
function useActiveHeading(ids: string[], content: unknown): string | undefined {
  const [active, setActive] = useState<string>();
  useEffect(() => {
    if (typeof IntersectionObserver === "undefined" || ids.length === 0) return;
    const elements = ids
      .map((id) => document.getElementById(id))
      .filter((el): el is HTMLElement => el !== null);
    const visible = new Set<string>();
    const observer = new IntersectionObserver(
      (entries) => {
        for (const e of entries) {
          if (e.isIntersecting) visible.add(e.target.id);
          else visible.delete(e.target.id);
        }
        const first = ids.find((id) => visible.has(id));
        if (first) {
          setActive(first);
          return;
        }
        // Between two headings: the last one above the window's top.
        const above = elements.filter((el) => el.getBoundingClientRect().top < 0).pop();
        setActive(above?.id);
      },
      { rootMargin: "0px 0px -60% 0px" },
    );
    for (const el of elements) observer.observe(el);
    return () => observer.disconnect();
  }, [ids, content]);
  return active;
}

function TocList({ headings, active }: { headings: DocHeading[]; active?: string }) {
  const handlers = useDocLinkHandlers();
  return (
    <ul className="space-y-0.5 text-[13px] leading-5" {...handlers}>
      {headings.map((h) => (
        <li key={h.id} className={cn(h.depth === 3 && "pl-3")}>
          <a
            href={`#${h.id}`}
            aria-current={h.id === active ? "location" : undefined}
            className={cn(
              "-ml-px block border-l border-transparent py-1 pl-3 text-muted-foreground hover:text-foreground",
              h.id === active && "border-primary font-medium text-foreground",
            )}
          >
            {h.text}
          </a>
        </li>
      ))}
    </ul>
  );
}

/**
 * DocToc is the outline of a page ("On this page", H2 and H3): a column from
 * xl up, a collapsible list above the page below it.
 */
export function DocToc({
  headings,
  content,
  variant,
  className,
}: {
  headings: DocHeading[];
  /** content changes when the page does (re-observes the headings). */
  content: unknown;
  variant: "column" | "collapsible";
  className?: string;
}) {
  // Only the column shows the active section: the list above the page
  // observes nothing.
  const ids = useMemo(
    () => (variant === "column" ? headings.map((h) => h.id) : []),
    [headings, variant],
  );
  const active = useActiveHeading(ids, content);
  if (headings.length === 0) return null;

  if (variant === "column") {
    return (
      <nav aria-label="On this page" className={className}>
        <p className="mb-2 text-[11px] font-semibold tracking-[0.06em] text-muted-foreground/80 uppercase">
          On this page
        </p>
        <div className="border-l">
          <TocList headings={headings} active={active} />
        </div>
      </nav>
    );
  }
  return (
    <Collapsible className={cn("rounded-lg border bg-card", className)}>
      <CollapsibleTrigger asChild>
        <Button
          type="button"
          variant="ghost"
          className="group h-10 w-full justify-start px-3 text-[13px] font-medium"
        >
          <TableOfContents aria-hidden />
          On this page
          <ChevronDown
            className="ml-auto transition-transform group-data-[state=open]:rotate-180"
            aria-hidden
          />
        </Button>
      </CollapsibleTrigger>
      <CollapsibleContent>
        <nav aria-label="On this page" className="border-t px-3 py-2">
          <TocList headings={headings} />
        </nav>
      </CollapsibleContent>
    </Collapsible>
  );
}
