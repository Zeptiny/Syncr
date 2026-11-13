import { FolderInput } from "lucide-react";
import { useState } from "react";

import {
  commonParent,
  type KnownSize,
  type SelectedItem,
} from "@/components/app/file-browser/selection";
import type { RowMenuItem } from "@/components/app/file-table";
import type { ProbeEntry } from "@/gen/syncr/v1/link_pb";

import type { Place, TransferSource } from "./transfer";
import { useStartedJobs } from "./use-started-jobs";

/** TransferRequest is what the transfer sheet opens with. */
export interface TransferRequest {
  source: TransferSource;
  size?: KnownSize;
  /** clear empties the selection the request came from: a started move takes its items away. */
  clear?: () => void;
}

/**
 * useTransferActions holds a file browser page's copy and move: the open
 * transfer sheet and the jobs it started. Render TransferActionsHost with
 * the result.
 */
export function useTransferActions(place: Place) {
  const [request, setRequest] = useState<TransferRequest>();
  const started = useStartedJobs();
  const openFor = (
    items: SelectedItem[],
    parent: string | undefined,
    size?: KnownSize,
    clear?: () => void,
  ) => setRequest({ source: { place, items, parent }, size, clear });
  /** rowItem is the row menu's "Copy or move…" for one entry. */
  const rowItem = (entry: ProbeEntry): RowMenuItem => ({
    label: "Copy or move…",
    icon: FolderInput,
    onSelect: () => openFor([{ path: entry.path, entry }], commonParent([entry.path], place.kind)),
  });
  return { request, close: () => setRequest(undefined), openFor, rowItem, started };
}

/** TransferActions is what useTransferActions returns. */
export type TransferActions = ReturnType<typeof useTransferActions>;
