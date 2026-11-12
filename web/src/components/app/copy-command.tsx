import { TriangleAlert } from "lucide-react";
import { useState } from "react";

import { InstallPlatform, InstallScope, type InstallCommand } from "@/gen/syncr/v1/api_pb";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { cn } from "@/lib/utils";

import { CodeBlock } from "./copy-button";

const platformLabel: Record<InstallPlatform, string> = {
  [InstallPlatform.UNSPECIFIED]: "Other",
  [InstallPlatform.LINUX]: "Linux",
  [InstallPlatform.WINDOWS]: "Windows",
};

const scopeLabel: Record<InstallScope, string> = {
  [InstallScope.UNSPECIFIED]: "Default",
  [InstallScope.SYSTEM]: "System service",
  [InstallScope.USER]: "Current user",
};

/** CopyCommandProps configures CopyCommand. */
export interface CopyCommandProps {
  commands: InstallCommand[];
  /**
   * shownOnce warns that the commands embed a secret that is not shown
   * again (e.g. an enrollment token).
   */
  shownOnce?: React.ReactNode;
  /** scopeLabel names the scope switch (default "Install for"). */
  scopeLabel?: string;
  className?: string;
}

function unique<T>(xs: T[]): T[] {
  return [...new Set(xs)];
}

/**
 * CopyCommand presents install commands by platform and scope, with a copy
 * button and, for one-time secrets, a warning. The platform tabs show only
 * when there is more than one platform.
 */
export function CopyCommand({
  commands,
  shownOnce,
  scopeLabel: scopeGroupLabel = "Install for",
  className,
}: CopyCommandProps) {
  const platforms = unique(commands.map((c) => c.platform));
  const [platformPref, setPlatform] = useState<InstallPlatform>(
    platforms[0] ?? InstallPlatform.LINUX,
  );
  const platform = platforms.includes(platformPref) ? platformPref : platforms[0];
  const forPlatform = commands.filter((c) => c.platform === platform);
  const scopes = unique(forPlatform.map((c) => c.scope));
  const [scopePref, setScope] = useState<InstallScope>(scopes[0] ?? InstallScope.SYSTEM);
  const scope = scopes.includes(scopePref) ? scopePref : scopes[0];
  const selected = forPlatform.filter((c) => c.scope === scope);

  if (!commands.length) return null;

  return (
    <div className={cn("flex flex-col gap-4", className)}>
      {shownOnce && (
        <div
          role="note"
          className="flex gap-2.5 rounded-lg bg-warning-soft px-3.5 py-3 text-[13px] leading-5 text-warning"
        >
          <TriangleAlert className="mt-0.5 size-4 shrink-0" aria-hidden />
          <div className="text-foreground">{shownOnce}</div>
        </div>
      )}
      <div className="flex flex-wrap items-center justify-between gap-3">
        {platforms.length > 1 && (
          <Tabs value={String(platform)} onValueChange={(v) => setPlatform(Number(v))}>
            <TabsList aria-label="Platform">
              {platforms.map((p) => (
                <TabsTrigger key={p} value={String(p)} className="px-3">
                  {platformLabel[p]}
                </TabsTrigger>
              ))}
            </TabsList>
          </Tabs>
        )}
        {scopes.length > 1 && (
          <div
            role="radiogroup"
            aria-label={scopeGroupLabel}
            className="inline-flex rounded-lg bg-muted p-[3px]"
          >
            {scopes.map((s) => (
              <button
                key={s}
                type="button"
                role="radio"
                aria-checked={s === scope}
                onClick={() => setScope(s)}
                className={cn(
                  "h-[30px] rounded-md px-3 text-[13px] font-medium text-muted-foreground",
                  s === scope && "bg-card text-foreground shadow-xs",
                )}
              >
                {scopeLabel[s]}
              </button>
            ))}
          </div>
        )}
      </div>
      {selected.map((c, i) => (
        <div key={i} className="flex flex-col gap-2">
          {c.title && <div className="text-[13px] font-medium">{c.title}</div>}
          <CodeBlock
            value={c.command}
            label={c.shell === "powershell" ? "PowerShell" : "Shell"}
            copyLabel="Copy command"
          />
          {c.note && <p className="text-[13px] leading-5 text-muted-foreground">{c.note}</p>}
        </div>
      ))}
    </div>
  );
}
