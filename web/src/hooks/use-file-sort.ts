import { useState } from "react";

import { loadSort, saveSort, type FileSort } from "@/lib/sort-entries";

/**
 * useFileSort is the remembered sort of file lists (localStorage
 * "syncr-file-sort", shared by every browser).
 */
export function useFileSort(): [FileSort, (s: FileSort) => void] {
  const [sort, setSort] = useState<FileSort>(loadSort);
  return [
    sort,
    (s) => {
      setSort(s);
      saveSort(s);
    },
  ];
}
