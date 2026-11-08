import { ChevronRight } from "lucide-react";
import { useState } from "react";
import { useFormContext } from "react-hook-form";

import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { FieldKind, type BackendField } from "@/gen/syncr/v1/remotes_pb";
import { cn } from "@/lib/utils";

import { BoolField, EnumField, IntField, SecretField, TextField } from "./fields";
import type { FieldValues } from "./schema";

/** DescriptorFieldsProps configures DescriptorFields. */
export interface DescriptorFieldsProps {
  /** descriptor is a backend or a notification channel. */
  descriptor: { fields: BackendField[] };
  /** secretsSet lists the secrets already stored (edit). */
  secretsSet?: string[];
  idPrefix: string;
  /** renderField overrides a field's control (e.g. union members); undefined falls back. */
  renderField?: (field: BackendField) => React.ReactNode | undefined;
  /** generators offer "Generate" on secret fields, by field name. */
  generators?: Record<string, () => string>;
  /** openAdvanced opens the advanced section (e.g. after a server error there). */
  openAdvanced?: boolean;
  onOpenAdvancedChange?: (open: boolean) => void;
}

/** wide marks fields that take the whole row of the two-column grid. */
function wide(f: BackendField): boolean {
  return f.kind === FieldKind.TEXT || f.kind === FieldKind.MEMBERS || f.kind === FieldKind.BOOL;
}

/**
 * DescriptorFields renders a descriptor's fields inside a react-hook-form
 * form whose values extend FieldValues, so a new backend or notification
 * channel needs no frontend change. Advanced fields are collapsed (and
 * open by themselves when one has an error); secrets are write-only.
 */
export function DescriptorFields({
  descriptor,
  secretsSet = [],
  idPrefix,
  renderField,
  generators = {},
  openAdvanced,
  onOpenAdvancedChange,
}: DescriptorFieldsProps) {
  const form = useFormContext<FieldValues>();
  const [ownOpen, setOwnOpen] = useState(false);
  const open = openAdvanced ?? ownOpen;
  const setOpen = onOpenAdvancedChange ?? setOwnOpen;
  const basic = descriptor.fields.filter((f) => !f.advanced);
  const advanced = descriptor.fields.filter((f) => f.advanced);
  const errors = form.formState.errors;
  const advancedHasError = advanced.some((f) => errors.values?.[f.name]);

  const control = (f: BackendField) => {
    const custom = renderField?.(f);
    if (custom !== undefined) return custom;
    if (f.secret) {
      return (
        <SecretField
          field={f}
          idPrefix={idPrefix}
          isSet={secretsSet.includes(f.name)}
          generate={generators[f.name]}
        />
      );
    }
    switch (f.kind) {
      case FieldKind.BOOL:
        return <BoolField field={f} idPrefix={idPrefix} />;
      case FieldKind.INT:
        return <IntField field={f} idPrefix={idPrefix} />;
      case FieldKind.ENUM:
        return <EnumField field={f} idPrefix={idPrefix} />;
      default:
        return <TextField field={f} idPrefix={idPrefix} />;
    }
  };

  return (
    <>
      {basic.length > 0 && (
        <div className="grid grid-cols-1 gap-x-5 gap-y-5 md:grid-cols-2">
          {basic.map((f) => (
            <div key={f.name} className={cn(wide(f) && "md:col-span-2")}>
              {control(f)}
            </div>
          ))}
        </div>
      )}
      {advanced.length > 0 && (
        <Collapsible
          open={open || advancedHasError}
          onOpenChange={setOpen}
          className="rounded-lg border"
        >
          <CollapsibleTrigger asChild>
            <button
              type="button"
              className="flex w-full items-center gap-2 rounded-lg px-4 py-3 text-left text-[13px] font-medium hover:bg-accent/50"
            >
              <ChevronRight
                className={cn(
                  "size-4 transition-transform",
                  (open || advancedHasError) && "rotate-90",
                )}
                aria-hidden
              />
              Advanced options
              <span className="font-normal text-muted-foreground">{advanced.length}</span>
            </button>
          </CollapsibleTrigger>
          <CollapsibleContent>
            <div className="grid grid-cols-1 gap-x-5 gap-y-5 border-t px-4 py-5 md:grid-cols-2">
              {advanced.map((f) => (
                <div key={f.name} className={cn(f.kind === FieldKind.TEXT && "md:col-span-2")}>
                  {control(f)}
                </div>
              ))}
            </div>
          </CollapsibleContent>
        </Collapsible>
      )}
    </>
  );
}
