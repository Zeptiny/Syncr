import { Archive } from "lucide-react";

import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { Repository } from "@/gen/syncr/v1/restic_pb";
import { cn } from "@/lib/utils";

/** RepositoryPickerProps configures RepositoryPicker. */
export interface RepositoryPickerProps {
  id?: string;
  repositories: Repository[];
  /** value is a repository ID, or "" for none. */
  value: string;
  onChange: (id: string) => void;
  invalid?: boolean;
  disabled?: boolean;
  className?: string;
}

/**
 * RepositoryPicker selects one of the user's restic repositories. A
 * repository whose data is being deleted is listed but cannot be chosen.
 */
export function RepositoryPicker({
  id,
  repositories,
  value,
  onChange,
  invalid,
  disabled,
  className,
}: RepositoryPickerProps) {
  return (
    <Select value={value || undefined} onValueChange={onChange} disabled={disabled}>
      <SelectTrigger
        id={id}
        aria-invalid={invalid || undefined}
        className={cn("w-full", className)}
      >
        <SelectValue placeholder="Choose a repository" />
      </SelectTrigger>
      <SelectContent>
        {repositories.map((r) => (
          <SelectItem key={r.id} value={r.id} disabled={r.deleting}>
            <Archive className="text-muted-foreground" aria-hidden />
            <span className="truncate">{r.name}</span>
            <span className="ml-1 truncate text-xs text-muted-foreground">
              {r.deleting
                ? "data is being deleted"
                : `${r.remote?.name ?? "Unknown remote"}${r.path ? `: ${r.path}` : ""}`}
            </span>
          </SelectItem>
        ))}
        {repositories.length === 0 && (
          <div className="px-2 py-3 text-[13px] text-muted-foreground">
            No repositories yet. Add one under Repositories.
          </div>
        )}
      </SelectContent>
    </Select>
  );
}
