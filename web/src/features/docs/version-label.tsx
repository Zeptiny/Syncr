import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useControllerInfo } from "@/hooks/use-controller-info";
import { cn } from "@/lib/utils";

import { githubRef, parseRelease } from "./version";

/** useGithubRef is the Git ref the docs' GitHub links point to (main until GetInfo answers). */
export function useGithubRef(): string {
  return githubRef(useControllerInfo().version);
}

/**
 * VersionLabel says which version the docs describe: the running
 * controller's. It is hidden while GetInfo loads or when it fails.
 */
export function VersionLabel({ className }: { className?: string }) {
  const { version } = useControllerInfo();
  if (!version) return null;
  const classes = cn("text-xs text-muted-foreground", className);
  if (parseRelease(version)) return <p className={classes}>Docs for {version}</p>;
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <p
          className={cn(
            classes,
            "w-fit cursor-default underline decoration-dotted underline-offset-2",
          )}
          tabIndex={0}
        >
          Docs for a development build
        </p>
      </TooltipTrigger>
      <TooltipContent side="bottom" className="max-w-64">
        Version {version}. GitHub links point to main.
      </TooltipContent>
    </Tooltip>
  );
}
