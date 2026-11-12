import { Tag } from "lucide-react";

import { addLabel, MAX_LABELS } from "@/lib/labels";
import { cn } from "@/lib/utils";

import { ChipsInput } from "./chips-input";

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
export function LabelEditor(props: LabelEditorProps) {
  return (
    <ChipsInput
      {...props}
      noun="label"
      placeholder="nas, linux, offsite…"
      add={(labels, raw) => {
        const next = addLabel(labels, raw);
        return { values: next.labels, problem: next.problem };
      }}
      help={
        <>
          Lower-case letters, digits, dots, dashes and underscores; up to {MAX_LABELS}. Press Enter
          or a comma to add one.
        </>
      }
    />
  );
}
