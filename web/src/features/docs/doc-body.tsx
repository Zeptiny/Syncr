import type { DocHtml } from "virtual:syncr-docs/manifest";
import { useMemo, useState } from "react";
import { createPortal } from "react-dom";

import { CopyButton } from "@/components/app/copy-button";
import { cn } from "@/lib/utils";

import { useDocLinkHandlers } from "./doc-navigation";

interface CopySlot {
  slot: Element;
  value: string;
}

/** copySlots finds the copy button places of the code blocks in a rendered page. */
function copySlots(root: HTMLElement | null): CopySlot[] {
  if (!root) return [];
  return [...root.querySelectorAll("[data-copy-slot]")].map((slot) => ({
    slot,
    value: slot.closest(".doc-code")?.querySelector("pre > code")?.textContent ?? "",
  }));
}

/**
 * DocBody shows a page's HTML, rendered at build time from docs/ (see
 * DocHtml), with a copy button on every code block and in-app navigation
 * for its links.
 */
export function DocBody({ html, className }: { html: DocHtml; className?: string }) {
  const handlers = useDocLinkHandlers();
  // The element is keyed by its HTML, so a new page is a new element and
  // its code blocks are found once it is in the document.
  const [root, setRoot] = useState<HTMLDivElement | null>(null);
  const slots = useMemo(() => copySlots(root), [root]);
  return (
    <>
      <div
        key={html}
        ref={setRoot}
        className={cn("doc-prose", className)}
        {...handlers}
        // The only HTML the app injects: build output from the repository's
        // docs (no raw HTML from authors, link schemes checked; design
        // section 10). It holds no event handlers or scripts.
        dangerouslySetInnerHTML={{ __html: html }}
      />
      {slots.map(({ slot, value }, i) =>
        createPortal(
          <CopyButton
            value={value}
            variant="ghost"
            className="h-7 text-sidebar-foreground hover:bg-sidebar-accent hover:text-sidebar-accent-foreground dark:hover:bg-sidebar-accent"
          />,
          slot,
          String(i),
        ),
      )}
    </>
  );
}
