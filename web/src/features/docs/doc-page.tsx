import { useLocation } from "@tanstack/react-router";
import { ArrowLeft, ArrowRight, ExternalLink } from "lucide-react";
import type { DocContent } from "virtual:syncr-docs/manifest";
import { useEffect, useMemo, useRef } from "react";

import { DocBody } from "./doc-body";
import { DocLink } from "./doc-link";
import { focusSection, keepInView } from "./doc-navigation";
import { DocToc } from "./doc-toc";
import type { DocPage } from "./docs.gen";
import { useFirstLocationKey } from "./docs-layout";
import { githubFileUrl, pageInfo, pinHtml } from "./manifest";
import { useDocumentTitle } from "./use-document-title";
import { useGithubRef } from "./version-label";
import { acknowledgeRelease } from "./whats-new";

/** DocPager links the previous and next pages in the order of docs/README.md. */
export function DocPager({ slug }: { slug: DocPage }) {
  const { previous, next } = pageInfo(slug);
  if (!previous && !next) return null;
  const card =
    "group flex min-w-0 flex-col gap-0.5 rounded-lg border bg-card px-4 py-3 transition-colors hover:border-primary/50";
  return (
    <nav aria-label="Previous and next page" className="grid gap-3 sm:grid-cols-2">
      {previous ? (
        <DocLink page={previous.slug} className={card}>
          <span className="inline-flex items-center gap-1 text-xs text-muted-foreground">
            <ArrowLeft className="size-3.5" aria-hidden />
            Previous
          </span>
          <span className="truncate text-sm font-medium group-hover:text-primary">
            {previous.title}
          </span>
        </DocLink>
      ) : (
        <span className="max-sm:hidden" />
      )}
      {next && (
        <DocLink page={next.slug} className={`${card} sm:items-end sm:text-right`}>
          <span className="inline-flex items-center gap-1 text-xs text-muted-foreground">
            Next
            <ArrowRight className="size-3.5" aria-hidden />
          </span>
          <span className="max-w-full truncate text-sm font-medium group-hover:text-primary">
            {next.title}
          </span>
        </DocLink>
      )}
    </nav>
  );
}

const RELEASE_NOTES = /^releases\/(v\d+\.\d+\.\d+)$/;

/**
 * DocPageView is one docs page: its title, outline, body, Previous/Next and
 * the link to the file on GitHub. After a client navigation it moves the
 * focus to the section in the address or to the title.
 */
export function DocPageView({ slug, content }: { slug: DocPage; content: DocContent }) {
  const ref = useGithubRef();
  const html = useMemo(() => pinHtml(content.html, ref), [content.html, ref]);
  const { page } = pageInfo(slug);
  const hash = useLocation({ select: (l) => l.hash });
  const title = useRef<HTMLHeadingElement>(null);
  const firstKey = useFirstLocationKey();
  const key = useLocation({ select: (l) => l.state.__TSR_key });
  useDocumentTitle(`${content.title} · Syncr docs`);

  useEffect(() => {
    const notes = RELEASE_NOTES.exec(slug);
    if (notes) acknowledgeRelease(notes[1]);
  }, [slug]);

  useEffect(() => {
    const section = hash ? focusSection(hash) : undefined;
    // Fonts and images that arrive later move the text above the section.
    if (section) return keepInView(section);
    // The page the reader opened keeps the browser's focus and scroll.
    if (key === firstKey) return;
    window.scrollTo({ top: 0 });
    title.current?.focus({ preventScroll: true });
  }, [slug, hash, key, firstKey]);

  return (
    <div className="xl:grid xl:grid-cols-[minmax(0,1fr)_13rem] xl:gap-10">
      <article id="doc-content" className="max-w-[72ch] min-w-0">
        <h1
          ref={title}
          tabIndex={-1}
          className="scroll-mt-16 text-[28px] leading-9 font-semibold tracking-[-0.015em] break-words outline-none"
        >
          {content.title}
        </h1>
        <DocToc
          headings={content.headings}
          content={html}
          variant="collapsible"
          className="mt-5 xl:hidden"
        />
        <DocBody html={html} className="mt-6" />
        <footer className="mt-12 space-y-6 border-t pt-6">
          <DocPager slug={slug} />
          {page && (
            <a
              href={githubFileUrl(page.file, ref)}
              target="_blank"
              rel="noopener noreferrer"
              className="inline-flex items-center gap-1.5 text-[13px] text-muted-foreground hover:text-foreground"
            >
              <ExternalLink className="size-3.5" aria-hidden />
              View on GitHub
              <span className="sr-only"> (opens in a new tab)</span>
            </a>
          )}
        </footer>
      </article>
      <aside className="hidden xl:block">
        <DocToc
          headings={content.headings}
          content={html}
          variant="column"
          className="sticky top-[var(--docs-sticky-top,1.5rem)] max-h-[calc(100svh-var(--docs-sticky-top,1.5rem)-1.5rem)] overflow-y-auto"
        />
      </aside>
    </div>
  );
}
