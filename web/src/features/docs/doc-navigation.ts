// Following links inside rendered docs HTML without a page reload, and
// bringing a section into view.
import { useRouter } from "@tanstack/react-router";
import { useCallback } from "react";

/**
 * focusSection scrolls the element with the ID in hash into view and moves
 * the focus there, so keyboard and screen reader users land on it too. It
 * returns the element, or undefined when the page has no such ID.
 */
export function focusSection(hash: string): HTMLElement | undefined {
  let id = hash.replace(/^#/, "");
  try {
    id = decodeURIComponent(id);
  } catch {
    // Not percent-encoded: use it as it is.
  }
  const el = id ? document.getElementById(id) : null;
  if (!el) return undefined;
  if (!el.hasAttribute("tabindex")) el.tabIndex = -1;
  el.scrollIntoView({ block: "start" });
  el.focus({ preventScroll: true });
  return el;
}

/** The reader's own input: after any of these, the page is theirs to scroll. */
const READER_EVENTS = ["wheel", "touchstart", "keydown", "pointerdown"] as const;

/** How long keepInView follows the page at least, and at most. */
const SETTLE_MIN_MS = 1000;
const SETTLE_MAX_MS = 10_000;

/**
 * keepInView keeps el where the last scroll put it while the page settles
 * after a load: the web font and images arrive after the first scroll and
 * reflow the text above the section (on a cold load by hundreds of pixels).
 * Whenever the page changes size and el has moved, it scrolls el back into
 * view. It stops at the reader's first wheel, touch, key or pointer input
 * (a reader who scrolls is never pulled back), once the fonts and the
 * document have loaded, or after SETTLE_MAX_MS; the returned function stops
 * it too.
 */
export function keepInView(el: HTMLElement): () => void {
  const top = el.getBoundingClientRect().top;
  const realign = () => {
    if (el.isConnected && Math.abs(el.getBoundingClientRect().top - top) > 1) {
      el.scrollIntoView({ block: "start" });
    }
  };
  const observer = typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(realign);
  observer?.observe(document.documentElement);
  observer?.observe(document.body);
  const article = el.closest("article");
  if (article) observer?.observe(article);

  let stopped = false;
  const timers: number[] = [];
  const stop = () => {
    if (stopped) return;
    stopped = true;
    observer?.disconnect();
    for (const t of timers) window.clearTimeout(t);
    for (const type of READER_EVENTS) window.removeEventListener(type, stop, true);
  };
  for (const type of READER_EVENTS) {
    window.addEventListener(type, stop, { capture: true, passive: true });
  }
  timers.push(window.setTimeout(stop, SETTLE_MAX_MS));

  const loaded =
    document.readyState === "complete"
      ? Promise.resolve()
      : new Promise<void>((resolve) =>
          window.addEventListener("load", () => resolve(), { once: true }),
        );
  const minimum = new Promise<void>((resolve) =>
    timers.push(window.setTimeout(resolve, SETTLE_MIN_MS)),
  );
  void Promise.all([document.fonts?.ready, loaded, minimum]).then(() => {
    if (stopped) return;
    // The last reflow is laid out by the next frame.
    requestAnimationFrame(() => {
      if (stopped) return;
      realign();
      stop();
    });
  });
  return stop;
}

/**
 * internalHref returns the app address of a link a click may follow in the
 * app: an absolute path, or a section of the current page (here is the
 * current path).
 */
function internalHref(a: HTMLAnchorElement, here: string): string | undefined {
  if (a.target || a.hasAttribute("download")) return undefined;
  const raw = a.getAttribute("href") ?? "";
  if (raw.startsWith("#")) return here + raw;
  if (!raw.startsWith("/") || raw.startsWith("//") || raw.startsWith("/\\")) return undefined;
  return raw;
}

/**
 * useDocLinkHandlers handles clicks and hovers on a container of plain links
 * (rendered docs, the outline): a primary click without modifier keys on a
 * same-origin link navigates in the app; hovering a docs link preloads it.
 */
export function useDocLinkHandlers() {
  const router = useRouter();
  const onClick = useCallback(
    (e: React.MouseEvent<HTMLElement>) => {
      if (e.defaultPrevented || e.button !== 0) return;
      if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
      const a = (e.target as Element).closest("a");
      if (!a || !e.currentTarget.contains(a)) return;
      const current = router.state.location;
      const href = internalHref(a, current.pathname + current.searchStr);
      if (href === undefined) return;
      e.preventDefault();
      const [path, hash = ""] = href.split("#", 2);
      if (path === current.pathname + current.searchStr && hash !== "" && hash === current.hash) {
        // The same section again: the location does not change.
        focusSection(hash);
        return;
      }
      void router.navigate({ href });
    },
    [router],
  );
  const onMouseOver = useCallback(
    (e: React.MouseEvent<HTMLElement>) => {
      const a = (e.target as Element).closest("a");
      if (!a || !e.currentTarget.contains(a)) return;
      const here = router.state.location.pathname;
      const path = internalHref(a, here)?.split(/[?#]/, 1)[0];
      if (path === undefined || path === here) return;
      const target =
        path === "/docs"
          ? router.preloadRoute({ to: "/docs" })
          : path.startsWith("/docs/")
            ? router.preloadRoute({
                to: "/docs/$",
                params: { _splat: path.slice("/docs/".length) },
              })
            : undefined;
      target?.catch(() => {
        // Preloading is a hint; the click loads the page again.
      });
    },
    [router],
  );
  return { onClick, onMouseOver };
}
