import { ChevronRight, TriangleAlert } from "lucide-react";
import { Fragment, useState } from "react";
import { useFormContext } from "react-hook-form";

import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { FieldKind, type BackendField } from "@/gen/syncr/v1/remotes_pb";
import { randomSecret } from "@/lib/random-secret";
import { cn } from "@/lib/utils";

import {
  BoolField,
  EnumField,
  FixedField,
  FixedNote,
  IntField,
  SecretField,
  TextField,
} from "./fields";
import { fixedSecretFields, isValueField, type FieldValues } from "./schema";

/** DescriptorFieldsProps configures DescriptorFields. */
export interface DescriptorFieldsProps {
  /** descriptor is a backend or a notification channel. */
  descriptor: { fields: BackendField[] };
  /** secretsSet lists the secrets already stored (edit). */
  secretsSet?: string[];
  idPrefix: string;
  /** renderField overrides a field's control (e.g. union members); undefined falls back. */
  renderField?: (field: BackendField) => React.ReactNode | undefined;
  /**
   * generators offer "Generate" on secret fields, by field name. Fixed
   * secrets get one by default when creating (32 URL-safe characters).
   */
  generators?: Record<string, () => string>;
  /**
   * storedOptions are the stored remote's options when editing it. Then
   * fixed fields (BackendField.fixed) show read-only; without it they are
   * ordinary inputs, and the first fixed secret gets its help as a warning
   * above it (it can never be shown or changed later).
   */
  storedOptions?: Record<string, string>;
  /** openAdvanced opens the advanced section (e.g. after a server error there). */
  openAdvanced?: boolean;
  onOpenAdvancedChange?: (open: boolean) => void;
}

/**
 * wide marks fields that take the whole row of the two-column grid. A
 * locked (read-only) switch is a short value, so it is not wide.
 */
function wide(f: BackendField, locked = false): boolean {
  if (locked) return f.kind === FieldKind.TEXT;
  return (
    f.kind === FieldKind.TEXT ||
    f.kind === FieldKind.MEMBERS ||
    f.kind === FieldKind.WRAPPED ||
    f.kind === FieldKind.BOOL
  );
}

/**
 * SecretWarning is the callout above a secret that is set once and never
 * shown again: its text is the field's help.
 */
function SecretWarning({ id, children }: { id: string; children: React.ReactNode }) {
  return (
    <div
      id={id}
      role="note"
      className="flex items-start gap-2.5 rounded-lg border border-warning/30 bg-warning-soft px-3.5 py-3 text-[13px] leading-5 text-warning"
    >
      <TriangleAlert className="mt-0.5 size-4 shrink-0" aria-hidden />
      <p>{children}</p>
    </div>
  );
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
  storedOptions,
  openAdvanced,
  onOpenAdvancedChange,
}: DescriptorFieldsProps) {
  const editing = storedOptions !== undefined;
  // The first fixed secret of a new remote carries the warning (its help).
  const warned = editing ? undefined : fixedSecretFields(descriptor).find((f) => f.help);
  const warningId = `${idPrefix}-fixed-secret-warning`;
  // When editing, a note above the first locked field of each section says why.
  const firstLocked = (fields: BackendField[]) =>
    editing ? fields.find((f) => f.fixed && isValueField(f))?.name : undefined;
  const noteId = (advanced: boolean) => `${idPrefix}-fixed-note${advanced ? "-advanced" : ""}`;
  const locked = (f: BackendField) => editing && f.fixed;
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
    if (editing && f.fixed) {
      return (
        <FixedField
          field={f}
          idPrefix={idPrefix}
          value={storedOptions?.[f.name]}
          isSet={secretsSet.includes(f.name)}
          noteId={noteId(f.advanced)}
        />
      );
    }
    if (f.secret) {
      const isWarned = f.name === warned?.name;
      return (
        <SecretField
          field={f}
          idPrefix={idPrefix}
          isSet={secretsSet.includes(f.name)}
          generate={generators[f.name] ?? (f.fixed && !editing ? () => randomSecret() : undefined)}
          hideHelp={isWarned}
          describedBy={isWarned ? warningId : undefined}
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
            <Fragment key={f.name}>
              {f.name === warned?.name && (
                <div className="md:col-span-2">
                  <SecretWarning id={warningId}>{f.help}</SecretWarning>
                </div>
              )}
              {f.name === firstLocked(basic) && (
                <div className="md:col-span-2">
                  <FixedNote id={noteId(false)} />
                </div>
              )}
              <div className={cn(wide(f, locked(f)) && "md:col-span-2")}>{control(f)}</div>
            </Fragment>
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
                <Fragment key={f.name}>
                  {f.name === warned?.name && (
                    <div className="md:col-span-2">
                      <SecretWarning id={warningId}>{f.help}</SecretWarning>
                    </div>
                  )}
                  {f.name === firstLocked(advanced) && (
                    <div className="md:col-span-2">
                      <FixedNote id={noteId(true)} />
                    </div>
                  )}
                  <div className={cn(f.kind === FieldKind.TEXT && "md:col-span-2")}>
                    {control(f)}
                  </div>
                </Fragment>
              ))}
            </div>
          </CollapsibleContent>
        </Collapsible>
      )}
    </>
  );
}
