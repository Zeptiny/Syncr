import { Tag, X } from "lucide-react";
import { useState } from "react";

import { Input } from "@/components/ui/input";
import { addLabel, MAX_LABELS } from "@/lib/labels";
import { cn } from "@/lib/utils";

/** LabelChips shows worker labels as small monospace chips. */
export function LabelChips({
  labels,
  className,
  empty,
}: {
  labels: string[];
  className?: string;
  empty?: React.ReactNode;
}) {
  if (!labels.length) return empty ? <>{empty}</> : null;
  return (
    <span className={cn("inline-flex flex-wrap gap-1", className)}>
      {labels.map((l) => (
        <LabelChip key={l} label={l} />
      ))}
    </span>
  );
}

/** LabelChip is one worker label. */
export function LabelChip({ label, className }: { label: string; className?: string }) {
  return (
    <span
      className={cn(
        "inline-flex h-5 items-center gap-1 rounded bg-secondary px-1.5 font-mono text-[11.5px] text-secondary-foreground",
        className,
      )}
    >
      <Tag className="size-2.5 opacity-60" aria-hidden />
      {label}
    </span>
  );
}

/** LabelEditorProps configures LabelEditor. */
export interface LabelEditorProps {
  id: string;
  value: string[];
  onChange: (labels: string[]) => void;
  /** error is a server error to show under the editor. */
  error?: string;
  /** suggestions are labels other workers use, offered as you type. */
  suggestions?: string[];
  disabled?: boolean;
  /**
   * text is the typed text not added as a label yet, and onTextChange is
   * told when it changes. Pass both to control it, e.g. so a Save button
   * can include it (see addLabel); otherwise the editor keeps it.
   */
  text?: string;
  onTextChange?: (text: string) => void;
}

/**
 * LabelEditor edits a worker's labels as chips. Type a label and press
 * Enter, a comma or a space to add it; Backspace in the empty input removes
 * the last one. Labels are checked as typed and kept lower-case and sorted.
 */
export function LabelEditor({
  id,
  value,
  onChange,
  error,
  suggestions = [],
  disabled,
  text,
  onTextChange,
}: LabelEditorProps) {
  const [ownDraft, setOwnDraft] = useState("");
  const draft = text ?? ownDraft;
  const setDraft = onTextChange ?? setOwnDraft;
  const [problem, setProblem] = useState<string | undefined>();
  const add = (raw: string) => {
    if (!raw.trim()) return true;
    const next = addLabel(value, raw);
    if (next.problem) {
      setProblem(next.problem);
      return false;
    }
    onChange(next.labels);
    setProblem(undefined);
    return true;
  };
  const offered = suggestions.filter(
    (s) => !value.includes(s) && s.startsWith(draft.trim().toLowerCase()),
  );
  const shownError = problem ?? error;
  return (
    <div className="flex flex-col gap-2">
      <div
        className={cn(
          "flex min-h-9 flex-wrap items-center gap-1.5 rounded-md border bg-card px-2 py-1.5 focus-within:border-ring focus-within:ring-[3px] focus-within:ring-ring/50",
          shownError && "border-danger",
          disabled && "opacity-60",
        )}
      >
        {value.map((l) => (
          <span
            key={l}
            className="inline-flex h-6 items-center gap-1 rounded bg-secondary pr-0.5 pl-1.5 font-mono text-[12px] text-secondary-foreground"
          >
            {l}
            <button
              type="button"
              disabled={disabled}
              onClick={() => onChange(value.filter((x) => x !== l))}
              className="grid size-5 place-items-center rounded text-muted-foreground hover:bg-background hover:text-foreground"
              aria-label={`Remove label ${l}`}
            >
              <X className="size-3" aria-hidden />
            </button>
          </span>
        ))}
        <Input
          id={id}
          value={draft}
          disabled={disabled}
          onChange={(e) => {
            const v = e.target.value;
            if (/[,\s]$/.test(v)) {
              if (add(v.slice(0, -1))) setDraft("");
              else setDraft(v.slice(0, -1));
            } else {
              setDraft(v);
              setProblem(undefined);
            }
          }}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              if (add(draft)) setDraft("");
            } else if (e.key === "Backspace" && draft === "" && value.length) {
              onChange(value.slice(0, -1));
            }
          }}
          onBlur={() => {
            if (draft && add(draft)) setDraft("");
          }}
          placeholder={value.length ? "Add another" : "nas, linux, offsite…"}
          aria-invalid={shownError ? true : undefined}
          aria-describedby={`${id}-help`}
          className="h-6 min-w-28 flex-1 border-0 bg-transparent px-1 font-mono text-[13px] shadow-none focus-visible:ring-0 dark:bg-transparent"
          spellCheck={false}
          autoComplete="off"
        />
      </div>
      {offered.length > 0 && draft !== "" && (
        <div className="flex flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
          Used elsewhere:
          {offered.slice(0, 8).map((s) => (
            <button
              key={s}
              type="button"
              // Keep the input focused, so its blur doesn't add the partial text.
              onMouseDown={(e) => e.preventDefault()}
              onClick={() => {
                add(s);
                setDraft("");
              }}
              className="rounded bg-muted px-1.5 py-0.5 font-mono hover:bg-accent hover:text-foreground"
            >
              {s}
            </button>
          ))}
        </div>
      )}
      {shownError ? (
        <p role="alert" className="text-xs leading-5 text-danger">
          {shownError}
        </p>
      ) : (
        <p id={`${id}-help`} className="text-xs leading-5 text-muted-foreground">
          Lower-case letters, digits, dots, dashes and underscores; up to {MAX_LABELS}. Press Enter
          or a comma to add one.
        </p>
      )}
    </div>
  );
}
