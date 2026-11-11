import { Search, X } from "lucide-react";
import { useEffect, useState } from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

/** FilterBar lays out list filters, with "Clear filters" when any is set. */
export function FilterBar({
  active,
  onClear,
  className,
  children,
}: {
  active?: boolean;
  onClear?: () => void;
  className?: string;
  children: React.ReactNode;
}) {
  return (
    <div className={cn("mb-4 flex flex-wrap items-center gap-2", className)}>
      {children}
      {active && onClear && (
        <Button variant="ghost" size="sm" onClick={onClear} className="text-muted-foreground">
          <X aria-hidden />
          Clear filters
        </Button>
      )}
    </div>
  );
}

/**
 * SearchInput is a search box that reports its value after a short pause.
 * When value comes back as what it reported, what was typed since stays:
 * only a change from elsewhere (clearing the filters, the back button)
 * replaces the text.
 */
export function SearchInput({
  value,
  onChange,
  placeholder = "Search",
  delayMs = 300,
  className,
}: {
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  delayMs?: number;
  className?: string;
}) {
  const [draft, setDraft] = useState(value);
  const [synced, setSynced] = useState(value);
  // sent is the last value reported, until value catches up with it.
  const [sent, setSent] = useState<string | undefined>(undefined);
  if (value !== synced) {
    setSynced(value);
    setSent(undefined);
    if (value !== sent) setDraft(value);
  }
  useEffect(() => {
    if (draft === value || draft === sent) return;
    const id = window.setTimeout(() => {
      setSent(draft);
      onChange(draft);
    }, delayMs);
    return () => window.clearTimeout(id);
  }, [draft, value, sent, onChange, delayMs]);
  return (
    <div className={cn("relative w-full sm:w-64", className)}>
      <Search
        className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground"
        aria-hidden
      />
      <Input
        type="search"
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        placeholder={placeholder}
        aria-label={placeholder}
        className="bg-card pl-8"
      />
    </div>
  );
}
