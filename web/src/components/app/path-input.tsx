import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

/** PathInputProps configures PathInput. */
export interface PathInputProps extends Omit<React.ComponentProps<"input">, "onChange" | "value"> {
  value: string;
  onChange: (value: string) => void;
  /** prefix is shown before the path, e.g. the remote's name. */
  prefix?: string;
  /** suggestions are offered as one-click values (e.g. local roots). */
  suggestions?: string[];
  invalid?: boolean;
}

/** PathInput edits a path in the monospace face. */
export function PathInput({
  value,
  onChange,
  prefix,
  suggestions,
  invalid,
  className,
  ...props
}: PathInputProps) {
  return (
    <div className={cn("flex flex-col gap-2", className)}>
      <div
        className={cn(
          "flex h-9 items-center overflow-hidden rounded-md border border-input bg-card shadow-xs focus-within:border-ring focus-within:ring-[3px] focus-within:ring-ring/50",
          invalid && "border-destructive",
        )}
      >
        {prefix && (
          <span className="flex h-full max-w-[45%] items-center truncate border-r bg-muted px-2.5 font-mono text-xs text-muted-foreground">
            {prefix}:
          </span>
        )}
        <Input
          {...props}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          aria-invalid={invalid || undefined}
          spellCheck={false}
          autoComplete="off"
          className="h-full min-w-0 flex-1 rounded-none border-0 bg-transparent font-mono text-[13px] shadow-none focus-visible:ring-0 dark:bg-transparent"
        />
      </div>
      {suggestions && suggestions.length > 0 && (
        <div className="flex flex-wrap items-center gap-1.5">
          <span className="text-xs text-muted-foreground">Allowed folders:</span>
          {suggestions.map((s) => (
            <button
              key={s}
              type="button"
              onClick={() => onChange(s.endsWith("/") || s.endsWith("\\") ? s : `${s}/`)}
              className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs hover:bg-accent"
            >
              {s}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
