import { Plus, Trash2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";

/** PatternList edits a list of exclude patterns, one input each. */
export function PatternList({
  id,
  value,
  onChange,
  noun = "pattern",
  placeholder = "*.tmp",
}: {
  id: string;
  value: string[];
  onChange: (v: string[]) => void;
  /** noun names one row for screen readers. */
  noun?: string;
  placeholder?: string;
}) {
  return (
    <div className="flex flex-col gap-2">
      {value.map((p, i) => (
        <div key={i} className="flex items-center gap-2">
          <Input
            id={i === 0 ? id : undefined}
            aria-label={`Exclude ${noun} ${i + 1}`}
            className="bg-card font-mono text-[13px]"
            value={p}
            placeholder={placeholder}
            spellCheck={false}
            autoComplete="off"
            onChange={(e) => onChange(value.map((x, j) => (j === i ? e.target.value : x)))}
          />
          <Button
            type="button"
            variant="ghost"
            size="icon"
            aria-label={`Remove exclude ${noun} ${i + 1}`}
            onClick={() => onChange(value.filter((_, j) => j !== i))}
          >
            <Trash2 aria-hidden />
          </Button>
        </div>
      ))}
      <div>
        <Button
          id={value.length === 0 ? id : undefined}
          type="button"
          variant="outline"
          size="sm"
          onClick={() => onChange([...value, ""])}
        >
          <Plus aria-hidden />
          Add an exclude
        </Button>
      </div>
    </div>
  );
}
