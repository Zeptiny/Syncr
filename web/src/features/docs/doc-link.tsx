// Typed links into the docs, for any page of the app. Only the generated
// titles and anchors are imported here, never the manifest or a page, so
// a help link adds next to nothing to the page that shows it.
import { Link, type LinkComponentProps } from "@tanstack/react-router";
import { CircleHelp } from "lucide-react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

import { DOC_TITLES, type DocAnchors, type DocPage } from "./docs.gen";

/** DocTarget is a docs page and, optionally, one of its heading IDs. */
export type DocTarget<P extends DocPage = DocPage> = { page: P; anchor?: DocAnchors[P] };

/**
 * docTarget builds a checked target, for tables that map an ID from the API
 * (a backend type, a channel) to the section that explains it.
 */
export function docTarget<P extends DocPage>(page: P, anchor?: DocAnchors[P]): DocTarget<P> {
  return anchor === undefined ? { page } : { page, anchor };
}

/** isDocPage reports whether slug is a page shipped in the app. */
export function isDocPage(slug: string): slug is DocPage {
  return Object.hasOwn(DOC_TITLES, slug);
}

/** docPath is the app path of a docs page (without the anchor). */
export function docPath(page: DocPage): string {
  return page === "" ? "/docs" : `/docs/${page}`;
}

/** docHref is the app URL of a target: /docs, /docs/<page>, plus #<anchor>. */
export function docHref(target: DocTarget): string {
  return docPath(target.page) + (target.anchor ? `#${target.anchor}` : "");
}

type DocLinkProps<P extends DocPage> = DocTarget<P> & {
  children: React.ReactNode;
  /** newTab opens the page in a new tab (help links, so a half-filled form stays). */
  newTab?: boolean;
  className?: string;
  "aria-label"?: string;
  onClick?: React.MouseEventHandler<HTMLAnchorElement>;
  /** activeProps are applied while this exact page is open (docs navigation). */
  activeProps?: LinkComponentProps["activeProps"];
};

/** DocLink is a router link to a docs page or section. */
export function DocLink<P extends DocPage>({
  page,
  anchor,
  children,
  newTab = false,
  ...props
}: DocLinkProps<P>) {
  const tab = newTab ? ({ target: "_blank", rel: "noopener noreferrer" } as const) : {};
  const body = (
    <>
      {children}
      {newTab && !props["aria-label"] && <span className="sr-only"> (opens in a new tab)</span>}
    </>
  );
  const common = {
    ...props,
    ...tab,
    hash: anchor,
    activeOptions: { exact: true, includeHash: false },
  };
  return page === "" ? (
    <Link to="/docs" {...common}>
      {body}
    </Link>
  ) : (
    <Link to="/docs/$" params={{ _splat: page }} {...common}>
      {body}
    </Link>
  );
}

/**
 * HelpLink is the small "Help" button next to a page title or panel: it
 * opens the section that explains the screen in a new tab.
 */
export function HelpLink<P extends DocPage>({
  page,
  anchor,
  label = "Help",
  className,
}: DocTarget<P> & { label?: string; className?: string }) {
  return (
    <Button
      asChild
      variant="ghost"
      size="sm"
      className={cn("text-muted-foreground hover:text-foreground", className)}
    >
      <DocLink
        page={page}
        anchor={anchor}
        newTab
        aria-label={`${label}: ${DOC_TITLES[page]} (opens in a new tab)`}
      >
        <CircleHelp aria-hidden />
        {label}
      </DocLink>
    </Button>
  );
}
