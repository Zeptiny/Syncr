// The "What's new" notice signed-in users see once per release, after an
// upgrade (design section 7). The acknowledged version lives in the
// browser's localStorage: one value per browser, not per user.
import { Sparkles } from "lucide-react";
import { useState, useSyncExternalStore } from "react";

import { Button } from "@/components/ui/button";
import { useControllerInfo } from "@/hooks/use-controller-info";

import { DocLink, isDocPage } from "./doc-link";
import type { DocPage } from "./docs.gen";
import { compareReleases, parseRelease } from "./version";

/** WHATS_NEW_KEY holds the last acknowledged final version, vX.Y.Z. */
export const WHATS_NEW_KEY = "syncr-whats-new";

/** defaultStorage is localStorage, or undefined where the browser refuses it. */
export function defaultStorage(): Storage | undefined {
  try {
    return window.localStorage;
  } catch {
    return undefined;
  }
}

// What was acknowledged in this page's lifetime, per storage, for browsers
// that refuse to store it: the notice stays away until the next reload.
const NO_STORAGE = {};
const remembered = new WeakMap<object, string>();
const listeners = new Set<() => void>();

function subscribe(onChange: () => void): () => void {
  listeners.add(onChange);
  window.addEventListener("storage", onChange);
  return () => {
    listeners.delete(onChange);
    window.removeEventListener("storage", onChange);
  };
}

/** acknowledged returns the stored version, or undefined (nothing stored, storage failing). */
export function acknowledged(storage: Storage | undefined): string | undefined {
  let stored: string | undefined;
  try {
    stored = storage?.getItem(WHATS_NEW_KEY) ?? undefined;
  } catch {
    stored = undefined;
  }
  const memory = remembered.get(storage ?? NO_STORAGE);
  return newest(stored, memory);
}

/** finalRelease parses a stored value: only vX.Y.Z counts. */
function finalRelease(v: string | undefined) {
  const release = parseRelease(v);
  return release && release.rc === undefined ? release : undefined;
}

function newest(a: string | undefined, b: string | undefined): string | undefined {
  const [ra, rb] = [finalRelease(a), finalRelease(b)];
  if (!ra) return rb ? b : a;
  if (!rb) return a;
  return compareReleases(ra, rb) >= 0 ? a : b;
}

/**
 * acknowledgeRelease records that the notes of base (vX.Y.Z) were seen. An
 * older version never replaces a newer one, so opening old notes after a
 * downgrade or for reading changes nothing.
 */
export function acknowledgeRelease(base: string, storage = defaultStorage()): void {
  if (!finalRelease(base)) return;
  const current = acknowledged(storage);
  if (newest(current, base) === current && finalRelease(current)) return;
  remembered.set(storage ?? NO_STORAGE, base);
  try {
    storage?.setItem(WHATS_NEW_KEY, base);
  } catch {
    // Kept in memory until the next reload.
  }
  for (const l of listeners) l();
}

/** WhatsNew is the notice to show: the running version and its notes page. */
export interface WhatsNew {
  version: string;
  base: string;
  notes: DocPage;
}

/**
 * whatsNew decides whether the notice shows: only for a release whose
 * notes ship in the app, when the stored version is missing, unreadable or
 * older than the running release's final version (rc.N shares it).
 */
export function whatsNew(
  version: string | undefined,
  stored: string | undefined,
): WhatsNew | undefined {
  const release = parseRelease(version);
  if (!release || version === undefined) return undefined;
  const notes = `releases/${release.base}`;
  if (!isDocPage(notes)) return undefined;
  const ack = finalRelease(stored);
  if (ack && compareReleases(ack, { ...release, rc: undefined }) >= 0) return undefined;
  return { version, base: release.base, notes };
}

/** WhatsNewNotice sits above the page content after an upgrade until it is dismissed. */
export function WhatsNewNotice({ storage = defaultStorage() }: { storage?: Storage }) {
  const { version } = useControllerInfo();
  const stored = useSyncExternalStore(subscribe, () => acknowledged(storage));
  const [dismissed, setDismissed] = useState<string>();
  const news = whatsNew(version, stored);
  if (!news || dismissed === news.base) return null;

  const acknowledge = () => {
    setDismissed(news.base);
    acknowledgeRelease(news.base, storage);
  };
  const dismiss = () => {
    acknowledge();
    // The button goes away: keep the keyboard in the page, not at the top.
    document.getElementById("main-content")?.focus({ preventScroll: true });
  };

  return (
    <section
      aria-label="What's new"
      className="mb-6 flex flex-col gap-3 rounded-lg border bg-card p-4 sm:flex-row sm:items-center"
    >
      <div className="flex min-w-0 flex-1 items-center gap-3">
        <span className="grid size-8 shrink-0 place-items-center rounded-md bg-primary/10 text-primary">
          <Sparkles className="size-4" aria-hidden />
        </span>
        <p className="min-w-0 text-sm leading-5">
          <span className="font-medium">Syncr was updated to {news.version}.</span>{" "}
          <span className="text-muted-foreground">See what changed in this release.</span>
        </p>
      </div>
      <div className="flex shrink-0 flex-wrap items-center gap-2 pl-11 sm:pl-0">
        <Button asChild size="sm">
          <DocLink page={news.notes} onClick={acknowledge}>
            Read the release notes
          </DocLink>
        </Button>
        <Button type="button" size="sm" variant="ghost" onClick={dismiss}>
          Dismiss
        </Button>
      </div>
    </section>
  );
}
