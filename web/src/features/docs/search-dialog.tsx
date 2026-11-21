import { useNavigate } from "@tanstack/react-router";
import { BookOpen, LoaderCircle, Search } from "lucide-react";
import { useEffect, useId, useMemo, useState } from "react";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "@/components/ui/dialog";
import { useDebounced } from "@/hooks/use-debounced";
import { cn } from "@/lib/utils";

import { DocLink } from "./doc-link";
import { prepareIndex, searchDocs, type PreparedIndex, type SearchResult } from "./search";

let indexPromise: Promise<PreparedIndex> | undefined;

/**
 * loadSearchIndex loads the search chunk once (on the first open, or when the
 * reader points at a Search button) and prepares it for matching.
 */
export function loadSearchIndex(): Promise<PreparedIndex> {
  indexPromise ??= import("virtual:syncr-docs/search").then(
    (m) => prepareIndex(m.default),
    (err: unknown) => {
      indexPromise = undefined;
      throw err;
    },
  );
  return indexPromise;
}

type IndexState =
  { status: "loading" } | { status: "ready"; index: PreparedIndex } | { status: "error" };

function useSearchIndex(enabled: boolean): IndexState {
  const [state, setState] = useState<IndexState>({ status: "loading" });
  useEffect(() => {
    if (!enabled || state.status === "ready") return;
    let live = true;
    loadSearchIndex().then(
      (index) => live && setState({ status: "ready", index }),
      () => live && setState({ status: "error" }),
    );
    return () => {
      live = false;
    };
  }, [enabled, state.status]);
  return state;
}

/** SEARCH_DEBOUNCE_MS keeps typing smooth; a search of the whole index takes a few ms. */
const SEARCH_DEBOUNCE_MS = 80;

/** SearchDialog searches the docs; arrows move, Enter opens, Escape closes. */
export function SearchDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="top-[10%] translate-y-0 gap-0 overflow-hidden p-0 data-[state=closed]:zoom-out-100 data-[state=open]:zoom-in-100 sm:max-w-xl">
        {open && <SearchPanel onClose={() => onOpenChange(false)} />}
      </DialogContent>
    </Dialog>
  );
}

function SearchPanel({ onClose }: { onClose: () => void }) {
  const navigate = useNavigate();
  const ids = useId();
  const listId = `${ids}-results`;
  const optionId = (i: number) => `${ids}-option-${i}`;
  const [query, setQuery] = useState("");
  const debounced = useDebounced(query.trim(), SEARCH_DEBOUNCE_MS);
  const index = useSearchIndex(true);
  const results = useMemo(
    () => (index.status === "ready" ? searchDocs(index.index, debounced) : []),
    [index, debounced],
  );
  // The highlighted result belongs to one query; a new query starts at the top.
  const [selection, setSelection] = useState({ query: "", at: 0 });
  const active = selection.query === debounced ? Math.min(selection.at, results.length - 1) : 0;
  const select = (at: number) => setSelection({ query: debounced, at });

  const expanded = results.length > 0;
  const activeId = expanded ? optionId(active) : undefined;
  useEffect(() => {
    if (activeId) document.getElementById(activeId)?.scrollIntoView({ block: "nearest" });
  }, [activeId, results]);

  const open = (r: SearchResult) => {
    onClose();
    const hash = r.anchor || undefined;
    void navigate({ to: "/docs/$", params: { _splat: r.slug }, hash });
  };

  const onKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (results.length === 0) return;
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      const step = e.key === "ArrowDown" ? 1 : -1;
      select((active + step + results.length) % results.length);
    } else if (e.key === "Enter" && !e.nativeEvent.isComposing) {
      e.preventDefault();
      open(results[active]);
    }
  };

  return (
    <>
      <DialogTitle className="sr-only">Search the docs</DialogTitle>
      <DialogDescription className="sr-only">
        Type to search. Use the up and down arrows to choose a result and Enter to open it.
      </DialogDescription>
      <div className="flex items-center gap-2 border-b px-4 pr-12">
        <Search className="size-4 shrink-0 text-muted-foreground" aria-hidden />
        <input
          role="combobox"
          aria-label="Search the docs"
          aria-autocomplete="list"
          aria-expanded={expanded}
          aria-controls={listId}
          aria-activedescendant={activeId}
          autoComplete="off"
          spellCheck={false}
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={onKeyDown}
          placeholder="Search the docs"
          className="h-12 min-w-0 flex-1 bg-transparent text-[15px] outline-none placeholder:text-muted-foreground"
        />
      </div>
      <div className="max-h-[min(60vh,28rem)] overflow-y-auto overscroll-contain">
        <ul id={listId} role="listbox" aria-label="Results" className={cn(expanded && "p-2")}>
          {results.map((r, i) => (
            <li
              key={`${r.slug}#${r.anchor}`}
              id={optionId(i)}
              role="option"
              aria-selected={i === active}
              onMouseMove={() => i !== active && select(i)}
              onClick={() => open(r)}
              className="cursor-pointer rounded-md px-3 py-2.5 aria-selected:bg-accent"
            >
              <div className="truncate text-[13px] font-medium">
                {r.page}
                {r.heading && (
                  <>
                    <span className="px-1.5 text-muted-foreground" aria-hidden>
                      ›
                    </span>
                    <span className="sr-only">, </span>
                    {r.heading}
                  </>
                )}
              </div>
              {r.snippet.length > 0 && (
                <p className="mt-0.5 line-clamp-2 text-xs leading-5 text-muted-foreground">
                  {r.snippet.map((s, k) =>
                    s.match ? (
                      <mark key={k} className="rounded-sm bg-primary/15 text-foreground">
                        {s.text}
                      </mark>
                    ) : (
                      <span key={k}>{s.text}</span>
                    ),
                  )}
                </p>
              )}
            </li>
          ))}
        </ul>
        <SearchStatus
          state={index.status}
          query={debounced}
          count={results.length}
          onClose={onClose}
        />
      </div>
    </>
  );
}

function SearchStatus({
  state,
  query,
  count,
  onClose,
}: {
  state: IndexState["status"];
  query: string;
  count: number;
  onClose: () => void;
}) {
  let body: React.ReactNode = null;
  if (state === "loading") {
    body = (
      <span className="inline-flex items-center gap-2">
        <LoaderCircle className="size-4 animate-spin" aria-hidden />
        Loading the search index…
      </span>
    );
  } else if (state === "error") {
    body = "The search index couldn't load. Check the connection and try again.";
  } else if (query === "") {
    body = "Search every page of the documentation.";
  } else if (count === 0) {
    body = (
      <span className="flex flex-col items-center gap-3">
        <span>
          No results for “<span className="font-medium text-foreground">{query}</span>”.
        </span>
        <Button asChild variant="outline" size="sm">
          <DocLink page="" onClick={onClose}>
            <BookOpen aria-hidden />
            All documentation
          </DocLink>
        </Button>
      </span>
    );
  }
  return (
    <>
      <p className="sr-only" aria-live="polite">
        {state === "ready" && query !== "" ? `${count} ${count === 1 ? "result" : "results"}` : ""}
      </p>
      {body && (
        <div className="px-4 py-8 text-center text-[13px] text-muted-foreground">{body}</div>
      )}
    </>
  );
}
