import { useLocation } from "@tanstack/react-router";
import { ChevronRight, ExternalLink } from "lucide-react";
import type { DocNavItem } from "virtual:syncr-docs/manifest";
import { useId, useState } from "react";

import { cn } from "@/lib/utils";

import { DocLink } from "./doc-link";
import { manifest, pinRef } from "./manifest";
import { useGithubRef } from "./version-label";

const itemClass =
  "flex min-h-8 items-center gap-1.5 rounded-md px-2.5 py-1 text-[13px] leading-5 text-muted-foreground transition-colors hover:bg-accent hover:text-foreground";
const activeClass = "bg-accent font-medium text-foreground";

/** currentSlug is the docs page of a pathname ("" for /docs). */
export function currentSlug(pathname: string): string {
  const slug = pathname.replace(/^\/docs\/?/, "").replace(/\/+$/, "");
  try {
    return decodeURIComponent(slug);
  } catch {
    return slug;
  }
}

function contains(item: DocNavItem, slug: string): boolean {
  return item.slug === slug || (item.children ?? []).some((c) => contains(c, slug));
}

function NavEntry({
  item,
  slug,
  onNavigate,
}: {
  item: DocNavItem;
  slug: string;
  onNavigate?: () => void;
}) {
  const ref = useGithubRef();
  const childrenId = useId();
  const [opened, setOpened] = useState(false);
  const children = item.children ?? [];
  const expanded = opened || contains(item, slug);

  let link: React.ReactNode;
  if (item.slug !== undefined) {
    link = (
      <DocLink
        page={item.slug}
        onClick={onNavigate}
        className={cn(itemClass, "min-w-0 flex-1")}
        activeProps={{ className: activeClass }}
      >
        <span className="min-w-0 flex-1">{item.label}</span>
      </DocLink>
    );
  } else if (item.external) {
    link = (
      <a
        href={pinRef(item.external, ref)}
        target="_blank"
        rel="noopener noreferrer"
        className={cn(itemClass, "min-w-0 flex-1")}
      >
        <span className="min-w-0 flex-1">{item.label}</span>
        <ExternalLink className="size-3.5 shrink-0 opacity-70" aria-hidden />
        <span className="sr-only"> (opens in a new tab)</span>
      </a>
    );
  } else {
    link = <span className={cn(itemClass, "min-w-0 flex-1")}>{item.label}</span>;
  }

  return (
    <li>
      <div className="flex items-center gap-0.5">
        {link}
        {children.length > 0 && (
          <button
            type="button"
            className="grid size-7 shrink-0 place-items-center rounded-md text-muted-foreground hover:bg-accent hover:text-foreground"
            aria-expanded={expanded}
            aria-controls={childrenId}
            aria-label={`${item.label}: ${expanded ? "hide" : "show"} the pages`}
            onClick={() => setOpened(!expanded)}
          >
            <ChevronRight
              className={cn("size-4 transition-transform", expanded && "rotate-90")}
              aria-hidden
            />
          </button>
        )}
      </div>
      {children.length > 0 && (
        <ul id={childrenId} hidden={!expanded} className="mt-0.5 ml-3 space-y-0.5 border-l pl-2">
          {children.map((c) => (
            <NavEntry
              key={c.slug ?? c.external ?? c.label}
              item={c}
              slug={slug}
              onNavigate={onNavigate}
            />
          ))}
        </ul>
      )}
    </li>
  );
}

/**
 * DocsNav is the docs' table of contents in the groups and order of
 * docs/README.md. onNavigate runs when a page link is followed (it closes
 * the phone sheet).
 */
export function DocsNav({
  onNavigate,
  className,
}: {
  onNavigate?: () => void;
  className?: string;
}) {
  const pathname = useLocation({ select: (l) => l.pathname });
  const slug = currentSlug(pathname);
  const ids = useId();
  return (
    <nav aria-label="Documentation" className={cn("text-sm", className)}>
      <DocLink
        page=""
        onClick={onNavigate}
        className={cn(itemClass, "mb-3 font-medium text-foreground")}
        activeProps={{ className: activeClass }}
      >
        {manifest.title}
      </DocLink>
      <ul className="space-y-5">
        {manifest.groups.map((group, g) => (
          <li key={group.title}>
            <p
              id={`${ids}-${g}`}
              className="mb-1.5 px-2.5 text-[11px] font-semibold tracking-[0.06em] text-muted-foreground/80 uppercase"
            >
              {group.title}
            </p>
            <ul aria-labelledby={`${ids}-${g}`} className="space-y-0.5">
              {group.items.map((item) => (
                <NavEntry
                  key={item.slug ?? item.external ?? item.label}
                  item={item}
                  slug={slug}
                  onNavigate={onNavigate}
                />
              ))}
            </ul>
          </li>
        ))}
      </ul>
    </nav>
  );
}
