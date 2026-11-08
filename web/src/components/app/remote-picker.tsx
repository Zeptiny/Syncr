import { HardDrive } from "lucide-react";

import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectLabel,
  SelectSeparator,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { Backend, Remote } from "@/gen/syncr/v1/remotes_pb";

/** LOCAL is the picker value of the worker's own filesystem. */
export const LOCAL = "__local__";

/** RemotePickerProps configures RemotePicker. */
export interface RemotePickerProps {
  id?: string;
  remotes: Remote[];
  backends: Map<string, Backend>;
  /** value is a remote ID, "" for none, or LOCAL. */
  value: string;
  onChange: (value: string) => void;
  /** filter limits the offered remotes, e.g. to union members. */
  filter?: (remote: Remote, backend: Backend | undefined) => boolean;
  /** allowLocal offers the worker's filesystem first. */
  allowLocal?: boolean;
  placeholder?: string;
  invalid?: boolean;
  disabled?: boolean;
  className?: string;
}

/** RemotePicker selects one of the user's remotes. */
export function RemotePicker({
  id,
  remotes,
  backends,
  value,
  onChange,
  filter,
  allowLocal,
  placeholder = "Choose a remote",
  invalid,
  disabled,
  className,
}: RemotePickerProps) {
  const offered = remotes.filter((r) => !filter || filter(r, backends.get(r.backendType)));
  return (
    <Select value={value || undefined} onValueChange={onChange} disabled={disabled}>
      <SelectTrigger id={id} aria-invalid={invalid || undefined} className={className ?? "w-full"}>
        <SelectValue placeholder={placeholder} />
      </SelectTrigger>
      <SelectContent>
        {allowLocal && (
          <>
            <SelectItem value={LOCAL}>
              <HardDrive className="text-muted-foreground" aria-hidden />
              Worker filesystem
            </SelectItem>
            {offered.length > 0 && <SelectSeparator />}
          </>
        )}
        {offered.length > 0 && (
          <SelectGroup>
            {allowLocal && <SelectLabel>Remotes</SelectLabel>}
            {offered.map((r) => (
              <SelectItem key={r.id} value={r.id}>
                <span className="truncate">{r.name}</span>
                <span className="ml-1 text-xs text-muted-foreground">
                  {backends.get(r.backendType)?.label ?? r.backendType}
                </span>
              </SelectItem>
            ))}
          </SelectGroup>
        )}
        {!allowLocal && offered.length === 0 && (
          <div className="px-2 py-3 text-[13px] text-muted-foreground">
            No matching remotes yet.
          </div>
        )}
      </SelectContent>
    </Select>
  );
}
