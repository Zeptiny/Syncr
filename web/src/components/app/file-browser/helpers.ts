// Helpers of the file browser's components that are not components.
import { Copy } from "lucide-react";
import { toast } from "sonner";

import type { Worker } from "@/gen/syncr/v1/api_pb";
import type { ProbeEntry } from "@/gen/syncr/v1/link_pb";
import { copyText } from "@/lib/clipboard";
import { fieldViolations } from "@/lib/errors";
import { formatBytes } from "@/lib/format";
import { isOnline, sortWorkers } from "@/lib/workers";

import type { RowMenuItem } from "../file-table";
import type { KnownSize } from "./selection";

/**
 * isMissingFolder reports whether a listing failed because the folder is
 * not there: the API's field error on "path" ("does not exist", "is not a
 * folder in this snapshot").
 */
export function isMissingFolder(err: unknown): boolean {
  return fieldViolations(err).some(
    (v) => v.field === "path" && /does not exist|is not a folder/.test(v.message),
  );
}

/** copyPathItem is the row menu's "Copy path". */
export function copyPathItem(entry: ProbeEntry): RowMenuItem {
  return {
    label: "Copy path",
    icon: Copy,
    onSelect: () =>
      void copyText(entry.path).then(
        () => toast.success(`Copied ${entry.path}`),
        () => toast.error("Couldn't copy to the clipboard"),
      ),
  };
}

/** sizeText writes a known size: "12.4 MB", "at least 12.4 MB" or "folders not measured". */
export function sizeText(k: KnownSize): string {
  if (k.atLeast && k.bytes === 0n) return "folders not measured";
  return k.atLeast ? `at least ${formatBytes(k.bytes)}` : formatBytes(k.bytes);
}

/**
 * eligibleWorkers are the workers that can list for a browser right now:
 * online ones that advertise the probe operation op, by name.
 */
export function eligibleWorkers(workers: Worker[], op: string): Worker[] {
  return sortWorkers(workers).filter((w) => isOnline(w) && !!w.info?.probeOps.includes(op));
}
