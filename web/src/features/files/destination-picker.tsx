import { HardDrive } from "lucide-react";
import { useState } from "react";

import { PathBrowserDialog } from "@/components/app/path-browser";
import { PathInput } from "@/components/app/path-input";
import { Label } from "@/components/ui/label";
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
import type { Worker } from "@/gen/syncr/v1/api_pb";
import type { Backend, Remote } from "@/gen/syncr/v1/remotes_pb";
import type { PathSource } from "@/hooks/use-path-listing";
import { isOnline, sortWorkers } from "@/lib/workers";

import type { Endpoint, Place } from "./transfer";

/** DestinationPickerProps configures DestinationPicker. */
export interface DestinationPickerProps {
  id: string;
  value: Endpoint | undefined;
  onChange: (value: Endpoint | undefined) => void;
  /** workers offer their allowed folders; only those that reported some are listed. */
  workers: Worker[];
  remotes: Remote[];
  backends: Map<string, Backend>;
  /**
   * jobWorker is the worker that runs the job: it lists remotes, and a
   * remote on a worker's own disk (the local type) is on its disk.
   */
  jobWorker?: Worker;
  error?: string;
}

const placeValue = (p: Place) => (p.kind === "worker" ? `w:${p.workerId}` : `r:${p.remoteId}`);

/**
 * DestinationPicker chooses the folder a copy or move goes into: a
 * worker's allowed folder or a folder of one of the user's remotes, typed
 * or picked with the folder browser.
 */
export function DestinationPicker({
  id,
  value,
  onChange,
  workers,
  remotes,
  backends,
  jobWorker,
  error,
}: DestinationPickerProps) {
  const [browsing, setBrowsing] = useState(false);
  const withFolders = sortWorkers(workers).filter((w) => (w.info?.localRoots.length ?? 0) > 0);
  const place = value?.place;
  const worker =
    place?.kind === "worker" ? workers.find((w) => w.id === place.workerId) : undefined;
  const remote =
    place?.kind === "remote" ? remotes.find((r) => r.id === place.remoteId) : undefined;
  const local = place?.kind === "remote" && !!place.local;
  const lister = jobWorker && isOnline(jobWorker) ? jobWorker : undefined;
  const source: PathSource | undefined =
    place?.kind === "worker"
      ? isOnline(worker)
        ? { kind: "worker", workerId: place.workerId }
        : undefined
      : place?.kind === "remote" && (!local || lister)
        ? { kind: "remote", remoteId: place.remoteId, workerId: lister?.id }
        : undefined;

  const choose = (v: string) => {
    const [kind, ref] = [v.slice(0, 1), v.slice(2)];
    if (kind === "w") onChange({ place: { kind: "worker", workerId: ref }, path: "" });
    else {
      const r = remotes.find((x) => x.id === ref);
      const isLocal = !!backends.get(r?.backendType ?? "")?.capabilities?.local;
      onChange({ place: { kind: "remote", remoteId: ref, local: isLocal }, path: "" });
    }
  };

  return (
    <fieldset className="flex min-w-0 flex-col gap-3" aria-describedby={`${id}-help`}>
      <legend className="mb-1.5 text-[13px] font-medium">Into</legend>
      <div className="flex flex-col gap-1.5">
        <Label htmlFor={`${id}-where`} className="text-xs font-normal text-muted-foreground">
          Where
        </Label>
        <Select value={place ? placeValue(place) : undefined} onValueChange={choose}>
          <SelectTrigger id={`${id}-where`} aria-invalid={!!error || undefined} className="w-full">
            <SelectValue placeholder="Choose a worker's folders or a remote" />
          </SelectTrigger>
          <SelectContent>
            {withFolders.length > 0 && (
              <SelectGroup>
                <SelectLabel>Workers' folders</SelectLabel>
                {withFolders.map((w) => (
                  <SelectItem key={w.id} value={`w:${w.id}`}>
                    <HardDrive className="text-muted-foreground" aria-hidden />
                    <span className="truncate">Folders on {w.name}</span>
                    {!isOnline(w) && (
                      <span className="ml-1 text-xs text-muted-foreground">offline</span>
                    )}
                  </SelectItem>
                ))}
              </SelectGroup>
            )}
            {withFolders.length > 0 && remotes.length > 0 && <SelectSeparator />}
            {remotes.length > 0 && (
              <SelectGroup>
                <SelectLabel>Remotes</SelectLabel>
                {remotes.map((r) => (
                  <SelectItem key={r.id} value={`r:${r.id}`}>
                    <span className="truncate">{r.name}</span>
                    <span className="ml-1 text-xs text-muted-foreground">
                      {backends.get(r.backendType)?.label ?? r.backendType}
                    </span>
                  </SelectItem>
                ))}
              </SelectGroup>
            )}
          </SelectContent>
        </Select>
      </div>
      {value && (
        <div className="flex flex-col gap-1.5">
          <Label htmlFor={`${id}-path`} className="text-xs font-normal text-muted-foreground">
            Folder
          </Label>
          <PathInput
            id={`${id}-path`}
            value={value.path}
            onChange={(path) => onChange({ ...value, path })}
            prefix={remote?.name}
            placeholder={place?.kind === "worker" ? "/srv/data" : "folder/inside/remote"}
            suggestions={place?.kind === "worker" ? worker?.info?.localRoots : undefined}
            invalid={!!error}
            onBrowse={source ? () => setBrowsing(true) : undefined}
          />
        </div>
      )}
      {error ? (
        <p id={`${id}-help`} className="text-xs text-danger">
          {error}
        </p>
      ) : (
        <p id={`${id}-help`} className="text-xs leading-5 text-muted-foreground">
          {place?.kind === "worker"
            ? "An absolute path inside one of the worker's allowed folders."
            : place?.kind === "remote"
              ? local
                ? `A folder on ${jobWorker?.name ?? "the job's worker"}'s disk, relative to the remote's root. It is created if missing.`
                : "Relative to the remote's root; empty for the root itself. It is created if missing."
              : "The selected items arrive in this folder under their own names."}
        </p>
      )}
      {source && (
        <PathBrowserDialog
          open={browsing}
          onOpenChange={setBrowsing}
          source={source}
          title={
            place?.kind === "worker"
              ? `Folders on ${worker?.name ?? "the worker"}`
              : `Browse ${remote?.name ?? "the remote"}`
          }
          rootLabel={remote?.name}
          mode="dir"
          initialPath={value?.path || undefined}
          onSelect={([path]) => value && path !== undefined && onChange({ ...value, path })}
        />
      )}
    </fieldset>
  );
}
