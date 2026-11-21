// Following links inside rendered docs HTML without a page reload, and
// bringing a section into view.
import { useRouter } from "@tanstack/react-router";
import { useCallback } from "react";

/**
 * focusSection scrolls the element with the ID in hash into view and moves
 * the focus there, so keyboard and screen reader users land on it too.
 */
export function focusSection(hash: string): boolean {
  let id = hash.replace(/^#/, "");
  try {
    id = decodeURIComponent(id);
  } catch {
    // Not percent-encoded: use it as it is.
  }
  const el = id ? document.getElementById(id) : null;
  if (!el) return false;
  if (!el.hasAttribute("tabindex")) el.tabIndex = -1;
  el.scrollIntoView({ block: "start" });
  el.focus({ preventScroll: true });
  return true;
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
