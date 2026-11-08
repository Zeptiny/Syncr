import { Check, Copy } from "lucide-react";
import { useEffect, useState } from "react";

import { Button } from "@/components/ui/button";
import { copyText } from "@/lib/clipboard";
import { cn } from "@/lib/utils";

/** CopyButton copies a value and confirms with a check mark. */
export function CopyButton({
  value,
  label = "Copy",
  className,
  variant = "outline",
}: {
  value: string;
  label?: string;
  className?: string;
  variant?: "outline" | "ghost" | "secondary";
}) {
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return;
    const id = window.setTimeout(() => setCopied(false), 1800);
    return () => window.clearTimeout(id);
  }, [copied]);
  return (
    <Button
      type="button"
      size="sm"
      variant={variant}
      className={cn("font-sans", className)}
      onClick={() => {
        void copyText(value).then(() => setCopied(true));
      }}
    >
      {copied ? <Check aria-hidden /> : <Copy aria-hidden />}
      <span aria-live="polite">{copied ? "Copied" : label}</span>
    </Button>
  );
}

/** CodeBlock shows a command or secret in the monospace face with a copy button. */
export function CodeBlock({
  value,
  label,
  className,
  copyLabel,
}: {
  value: string;
  /** label names the content, e.g. the shell ("Shell", "PowerShell"). */
  label?: string;
  className?: string;
  copyLabel?: string;
}) {
  return (
    <div
      className={cn(
        "overflow-hidden rounded-lg border border-sidebar-border bg-sidebar",
        className,
      )}
    >
      <div className="flex h-10 items-center justify-between border-b border-sidebar-border pr-1.5 pl-4 text-xs text-sidebar-foreground">
        <span>{label}</span>
        <CopyButton
          value={value}
          label={copyLabel}
          variant="ghost"
          className="h-7 font-sans text-sidebar-foreground hover:bg-sidebar-accent hover:text-sidebar-accent-foreground"
        />
      </div>
      <pre className="overflow-x-auto px-4 py-3.5 font-mono text-[12.5px] leading-[1.7] break-all whitespace-pre-wrap text-sidebar-accent-foreground">
        {value}
      </pre>
    </div>
  );
}
