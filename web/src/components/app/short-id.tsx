import { cn } from "@/lib/utils";

import { CopyButton } from "./copy-button";

/**
 * ShortId shows the first characters of a long ID (a snapshot or a
 * repository) in the monospace face, with the whole ID as its title and an
 * optional copy button.
 */
export function ShortId({
  id,
  length = 8,
  copy,
  className,
}: {
  id: string;
  length?: number;
  /** copy adds a button that copies the whole ID. */
  copy?: boolean;
  className?: string;
}) {
  if (!id) return <span className="text-muted-foreground">–</span>;
  return (
    <span className={cn("inline-flex items-center gap-1 font-mono text-[12.5px]", className)}>
      <span title={id}>{id.slice(0, length)}</span>
      {copy && (
        <span onClick={(e) => e.stopPropagation()} onKeyDown={(e) => e.stopPropagation()}>
          <CopyButton
            value={id}
            label="Copy ID"
            variant="ghost"
            className="h-6 px-1.5 text-xs text-muted-foreground"
          />
        </span>
      )}
    </span>
  );
}
