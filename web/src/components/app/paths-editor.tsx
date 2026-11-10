import { Plus, Trash2 } from "lucide-react";
import { useState } from "react";

import { Button } from "@/components/ui/button";
import type { PathSource } from "@/hooks/use-path-listing";

import { PathBrowserDialog } from "./path-browser";
import { PathInput } from "./path-input";

/** PathsEditorProps configures PathsEditor. */
export interface PathsEditorProps {
  id: string;
  value: string[];
  onChange: (paths: string[]) => void;
  /**
   * browse, when given, adds a Browse button to every path that opens the
   * path browser on this source (usually a worker).
   */
  browse?: { source: PathSource; title: string; description?: React.ReactNode };
  /** suggestions are offered under the first path (a worker's folders). */
  suggestions?: string[];
  /** label names one path for screen readers, e.g. "Path to back up". */
  label?: string;
  placeholder?: string;
  invalid?: boolean;
}

/**
 * PathsEditor edits a list of paths, one PathInput each, with an optional
 * Browse button. There is always at least one row.
 */
export function PathsEditor({
  id,
  value,
  onChange,
  browse,
  suggestions,
  label = "Path",
  placeholder = "/srv/data",
  invalid,
}: PathsEditorProps) {
  const rows = value.length ? value : [""];
  const [browsing, setBrowsing] = useState<number | undefined>();
  const set = (i: number, v: string) => onChange(rows.map((x, j) => (j === i ? v : x)));
  return (
    <div className="flex flex-col gap-2">
      {rows.map((p, i) => (
        <div key={i} className="flex items-start gap-2">
          <PathInput
            id={i === 0 ? id : `${id}-${i}`}
            aria-label={`${label} ${i + 1}`}
            className="min-w-0 flex-1"
            value={p}
            onChange={(v) => set(i, v)}
            placeholder={placeholder}
            invalid={invalid && !p.trim()}
            suggestions={i === 0 && !p ? suggestions : undefined}
            onBrowse={browse ? () => setBrowsing(i) : undefined}
          />
          <Button
            type="button"
            variant="ghost"
            size="icon"
            aria-label={`Remove ${label.toLowerCase()} ${i + 1}`}
            disabled={rows.length === 1}
            onClick={() => onChange(rows.filter((_, j) => j !== i))}
          >
            <Trash2 aria-hidden />
          </Button>
        </div>
      ))}
      <div>
        <Button type="button" variant="outline" size="sm" onClick={() => onChange([...rows, ""])}>
          <Plus aria-hidden />
          Add a path
        </Button>
      </div>
      {browse && (
        <PathBrowserDialog
          open={browsing !== undefined}
          onOpenChange={(o) => !o && setBrowsing(undefined)}
          source={browse.source}
          title={browse.title}
          description={browse.description}
          mode="any"
          multiple
          onSelect={(paths) => {
            if (browsing === undefined || paths.length === 0) return;
            const next = [...rows];
            next.splice(browsing, 1, ...paths);
            onChange([...new Set(next)]);
          }}
        />
      )}
    </div>
  );
}
