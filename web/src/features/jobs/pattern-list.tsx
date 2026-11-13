import { Plus, Trash2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { FilterAction } from "@/gen/syncr/v1/link_pb";

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

/** FilterRule is one rclone filter rule as a form edits it. */
export interface FilterRule {
  action: FilterAction;
  pattern: string;
}

/**
 * FilterRuleList edits rclone filter rules, include or exclude, in order:
 * transfers and two-way syncs use it. help explains them under the title;
 * notice (e.g. that a change needs a resync) and error follow the rules.
 */
export function FilterRuleList({
  value,
  onChange,
  help,
  notice,
  error,
}: {
  value: FilterRule[];
  onChange: (v: FilterRule[]) => void;
  help?: React.ReactNode;
  notice?: React.ReactNode;
  error?: string;
}) {
  const set = (i: number, f: Partial<FilterRule>) =>
    onChange(value.map((x, j) => (j === i ? { ...x, ...f } : x)));
  return (
    <div className="flex flex-col gap-2">
      <div>
        <div className="text-[13px] font-medium">Filters</div>
        {help && <p className="text-xs leading-5 text-muted-foreground">{help}</p>}
      </div>
      {value.map((f, i) => (
        <div key={i} className="flex items-center gap-2">
          <Select value={String(f.action)} onValueChange={(v) => set(i, { action: Number(v) })}>
            <SelectTrigger className="w-32 bg-card" aria-label={`Filter ${i + 1} action`}>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={String(FilterAction.EXCLUDE)}>Exclude</SelectItem>
              <SelectItem value={String(FilterAction.INCLUDE)}>Include</SelectItem>
            </SelectContent>
          </Select>
          <Input
            aria-label={`Filter ${i + 1} pattern`}
            className="bg-card font-mono text-[13px]"
            value={f.pattern}
            placeholder="*.tmp"
            spellCheck={false}
            autoComplete="off"
            aria-invalid={error ? true : undefined}
            onChange={(e) => set(i, { pattern: e.target.value })}
          />
          <Button
            type="button"
            variant="ghost"
            size="icon"
            aria-label={`Remove filter ${i + 1}`}
            onClick={() => onChange(value.filter((_, j) => j !== i))}
          >
            <Trash2 aria-hidden />
          </Button>
        </div>
      ))}
      <div>
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={() => onChange([...value, { action: FilterAction.EXCLUDE, pattern: "" }])}
        >
          <Plus aria-hidden />
          Add filter
        </Button>
      </div>
      {notice}
      {error && (
        <p role="alert" className="text-xs leading-5 text-danger">
          {error}
        </p>
      )}
    </div>
  );
}
