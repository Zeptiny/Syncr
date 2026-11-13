import { useState } from "react";

import { emptySelection, type Selection } from "./selection";

/**
 * useBrowserSelection keeps a browser's selection, cleared whenever key
 * changes (another worker, remote or snapshot: see sourceKey). It survives
 * moving between folders of the same source.
 */
export function useBrowserSelection(key: string): [Selection, (s: Selection) => void] {
  const [state, setState] = useState<{ key: string; selection: Selection }>({
    key,
    selection: emptySelection,
  });
  const selection = state.key === key ? state.selection : emptySelection;
  return [selection, (s) => setState({ key, selection: s })];
}
