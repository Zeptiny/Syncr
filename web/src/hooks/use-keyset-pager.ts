import { useState } from "react";

/** KeysetPager walks a list API that pages with opaque next-page tokens. */
export interface KeysetPager {
  /** token is the page token of the current page ("" for the first). */
  token: string;
  /** page is the 1-based page number. */
  page: number;
  canPrev: boolean;
  next: (nextPageToken: string) => void;
  prev: () => void;
}

/**
 * useKeysetPager keeps the stack of visited page tokens. It starts over at
 * the first page whenever resetKey changes (e.g. when the filters change).
 */
export function useKeysetPager(resetKey: string): KeysetPager {
  const [stack, setStack] = useState<string[]>([""]);
  const [key, setKey] = useState(resetKey);
  if (key !== resetKey) {
    setKey(resetKey);
    setStack([""]);
  }
  return {
    token: stack[stack.length - 1] ?? "",
    page: stack.length,
    canPrev: stack.length > 1,
    next: (t) => {
      if (t) setStack((s) => [...s, t]);
    },
    prev: () => setStack((s) => (s.length > 1 ? s.slice(0, -1) : s)),
  };
}
