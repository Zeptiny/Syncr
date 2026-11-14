import { cn } from "@/lib/utils";

/** ChoiceCard is one option of ChoiceCards. */
export interface ChoiceCard<T> {
  value: T;
  label: React.ReactNode;
  help?: React.ReactNode;
  /** children render inside the card while it is chosen (e.g. a select). */
  children?: React.ReactNode;
  /** disabled keeps the option from being chosen; say why in note. */
  disabled?: boolean;
  /** note renders under the help whether or not it is chosen, e.g. why it is disabled. */
  note?: React.ReactNode;
}

/** ChoiceCards is a radio group of explained options, laid out as cards. */
export function ChoiceCards<T extends string | number>({
  label,
  labelId,
  value,
  onChange,
  options,
  className,
}: {
  /** label is the group's visible name. */
  label?: React.ReactNode;
  labelId: string;
  value: T;
  onChange: (v: T) => void;
  options: ChoiceCard<T>[];
  className?: string;
}) {
  return (
    <div className="flex flex-col gap-1.5">
      {label && (
        <span className="text-[13px] font-medium" id={labelId}>
          {label}
        </span>
      )}
      <div role="radiogroup" aria-labelledby={labelId} className={cn("grid gap-2", className)}>
        {options.map((o) => {
          const on = o.value === value;
          return (
            <div
              key={String(o.value)}
              className={cn(
                "rounded-lg border bg-card transition-colors",
                on
                  ? "border-primary ring-[3px] ring-primary/20"
                  : !o.disabled && "hover:border-foreground/25",
              )}
            >
              <button
                type="button"
                role="radio"
                aria-checked={on}
                disabled={o.disabled}
                onClick={() => onChange(o.value)}
                className="w-full rounded-lg px-3.5 py-3 text-left disabled:cursor-not-allowed"
              >
                <span
                  className={cn(
                    "flex items-center gap-2 text-[13px] font-semibold",
                    o.disabled && "opacity-60",
                  )}
                >
                  <span
                    className={cn(
                      "grid size-3.5 shrink-0 place-items-center rounded-full border",
                      on && "border-primary",
                    )}
                    aria-hidden
                  >
                    {on && <span className="size-2 rounded-full bg-primary" />}
                  </span>
                  {o.label}
                </span>
                {o.help && (
                  <span
                    className={cn(
                      "mt-1 block pl-5.5 text-xs leading-5 text-muted-foreground",
                      o.disabled && "opacity-60",
                    )}
                  >
                    {o.help}
                  </span>
                )}
                {o.note && <span className="mt-1.5 block pl-5.5 text-xs leading-5">{o.note}</span>}
              </button>
              {on && o.children && <div className="px-3.5 pb-3 pl-9">{o.children}</div>}
            </div>
          );
        })}
      </div>
    </div>
  );
}
