import { X } from "lucide-react";
import { useState } from "react";

import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

/** ChipsResult is what adding one typed value gives: the new list, or a problem. */
export interface ChipsResult {
  values: string[];
  problem?: string;
}

/** ChipsInputProps configures ChipsInput. */
export interface ChipsInputProps {
  id: string;
  value: string[];
  onChange: (values: string[]) => void;
  /**
   * add puts one typed value into the list. It may normalize the value,
   * sort the list, or refuse with a problem. The default trims and skips
   * duplicates.
   */
  add?: (values: string[], raw: string) => ChipsResult;
  /** noun names one value in button labels, e.g. "label" or "tag". */
  noun?: string;
  /** error is a server error to show under the input. */
  error?: string;
  /** help shows under the input while there is no error. */
  help?: React.ReactNode;
  /** suggestions are known values, offered as you type. */
  suggestions?: string[];
  placeholder?: string;
  /** separators end a value when typed; Enter always does. */
  separators?: RegExp;
  disabled?: boolean;
  /**
   * text is the typed text not added yet, and onTextChange is told when it
   * changes. Pass both to control it, e.g. so a Save button can include
   * it; otherwise the input keeps it.
   */
  text?: string;
  onTextChange?: (text: string) => void;
}

function addPlain(values: string[], raw: string): ChipsResult {
  const v = raw.trim();
  return { values: v && !values.includes(v) ? [...values, v] : values };
}

/**
 * ChipsInput edits a list of short values as chips. Type a value and press
 * Enter or a separator to add it; Backspace in the empty input removes the
 * last one.
 */
export function ChipsInput({
  id,
  value,
  onChange,
  add: addValue = addPlain,
  noun = "value",
  error,
  help,
  suggestions = [],
  placeholder,
  separators = /[,\s]$/,
  disabled,
  text,
  onTextChange,
}: ChipsInputProps) {
  const [ownDraft, setOwnDraft] = useState("");
  const draft = text ?? ownDraft;
  const setDraft = onTextChange ?? setOwnDraft;
  const [problem, setProblem] = useState<string | undefined>();
  const add = (raw: string) => {
    if (!raw.trim()) return true;
    const next = addValue(value, raw);
    if (next.problem) {
      setProblem(next.problem);
      return false;
    }
    onChange(next.values);
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
              aria-label={`Remove ${noun} ${l}`}
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
            if (separators.test(v)) {
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
          placeholder={value.length ? "Add another" : placeholder}
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
        help && (
          <p id={`${id}-help`} className="text-xs leading-5 text-muted-foreground">
            {help}
          </p>
        )
      )}
    </div>
  );
}
