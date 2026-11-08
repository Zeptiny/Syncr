import { Label } from "@/components/ui/label";
import type { Worker } from "@/gen/syncr/v1/api_pb";
import type { Backend, Remote } from "@/gen/syncr/v1/remotes_pb";

import { PathInput } from "./path-input";
import { LOCAL, RemotePicker } from "./remote-picker";

/** Location is a job endpoint: a remote ID ("" = worker filesystem) and a path. */
export interface Location {
  remoteId: string;
  path: string;
}

/** LocationEditorProps configures LocationEditor. */
export interface LocationEditorProps {
  id: string;
  label: string;
  value: Location;
  onChange: (v: Location) => void;
  remotes: Remote[];
  backends: Map<string, Backend>;
  /** worker provides the allowed local folders for the filesystem option. */
  worker?: Worker;
  errors?: { remote?: string; path?: string };
}

/** LocationEditor picks where a job reads from or writes to. */
export function LocationEditor({
  id,
  label,
  value,
  onChange,
  remotes,
  backends,
  worker,
  errors,
}: LocationEditorProps) {
  const local = value.remoteId === "";
  const remote = remotes.find((r) => r.id === value.remoteId);
  const roots = worker?.info?.localRoots ?? [];
  return (
    <fieldset className="flex min-w-0 flex-col gap-3">
      <legend className="mb-3 text-[13px] font-semibold">{label}</legend>
      <div className="flex flex-col gap-1.5">
        <Label htmlFor={`${id}-remote`} className="text-xs font-normal text-muted-foreground">
          Where
        </Label>
        <RemotePicker
          id={`${id}-remote`}
          remotes={remotes}
          backends={backends}
          allowLocal
          value={local ? LOCAL : value.remoteId}
          onChange={(v) => onChange({ remoteId: v === LOCAL ? "" : v, path: value.path })}
          invalid={!!errors?.remote}
        />
        {errors?.remote && <p className="text-xs text-danger">{errors.remote}</p>}
      </div>
      <div className="flex flex-col gap-1.5">
        <Label htmlFor={`${id}-path`} className="text-xs font-normal text-muted-foreground">
          Path
        </Label>
        <PathInput
          id={`${id}-path`}
          value={value.path}
          onChange={(path) => onChange({ ...value, path })}
          prefix={local ? undefined : remote?.name}
          placeholder={local ? "/srv/data" : "folder/inside/remote"}
          suggestions={local ? roots : undefined}
          invalid={!!errors?.path}
        />
        {errors?.path ? (
          <p className="text-xs text-danger">{errors.path}</p>
        ) : (
          <p className="text-xs leading-5 text-muted-foreground">
            {local
              ? "An absolute path on the worker, inside one of its allowed folders."
              : "Relative to the remote's root. Leave empty for the root itself."}
          </p>
        )}
      </div>
    </fieldset>
  );
}
