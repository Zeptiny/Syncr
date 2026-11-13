import { useLayoutEffect, useState, type RefObject } from "react";

/**
 * useMinWidth reports whether the element is at least rem root-font units
 * wide, following its size as it changes. Until the element has a width
 * (before layout, or where nothing is laid out) it reports fallback.
 */
export function useMinWidth(
  ref: RefObject<HTMLElement | null>,
  rem: number,
  fallback = true,
): boolean {
  const [wide, setWide] = useState(fallback);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const check = () => {
      const width = el.getBoundingClientRect().width;
      if (width <= 0) return;
      const unit = parseFloat(getComputedStyle(document.documentElement).fontSize) || 16;
      setWide(width >= rem * unit);
    };
    check();
    const ro = new ResizeObserver(check);
    ro.observe(el);
    return () => ro.disconnect();
  }, [ref, rem]);
  return wide;
}
