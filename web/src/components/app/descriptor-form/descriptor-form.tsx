import { zodResolver } from "@hookform/resolvers/zod";
import { ChevronRight } from "lucide-react";
import { useId, useMemo, useState } from "react";
import { FormProvider, useForm, type FieldPath, type Resolver } from "react-hook-form";

import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Input } from "@/components/ui/input";
import { FieldKind, type Backend, type BackendField, type Remote } from "@/gen/syncr/v1/remotes_pb";
import { errorMessage, fieldViolations } from "@/lib/errors";
import { cn } from "@/lib/utils";

import {
  BoolField,
  EnumField,
  FieldShell,
  IntField,
  MembersField,
  SecretField,
  TextField,
} from "./fields";
import {
  buildSchema,
  defaultValues,
  mapViolations,
  membersField,
  toRemoteInput,
  type DescriptorValues,
  type RemoteInputInit,
} from "./schema";

/** DescriptorFormProps configures DescriptorForm. */
export interface DescriptorFormProps {
  backend: Backend;
  /** remote is the stored remote when editing. */
  remote?: Remote;
  /** remotes and backends feed the union member picker. */
  remotes?: Remote[];
  backends?: Map<string, Backend>;
  /**
   * onSubmit sends the input. If it rejects with INVALID_ARGUMENT and a
   * ValidationError, the violations are shown on their fields.
   */
  onSubmit: (input: RemoteInputInit) => Promise<unknown>;
  submitLabel?: string;
  onCancel?: () => void;
  /** footer renders extra actions on the left of the footer (e.g. delete). */
  footer?: React.ReactNode;
}

function FieldFor({
  field,
  idPrefix,
  remote,
  remotes,
  backends,
}: {
  field: BackendField;
  idPrefix: string;
  remote?: Remote;
  remotes: Remote[];
  backends: Map<string, Backend>;
}) {
  if (field.kind === FieldKind.MEMBERS) {
    return (
      <MembersField
        field={field}
        idPrefix={idPrefix}
        remotes={remotes}
        backends={backends}
        selfId={remote?.id}
      />
    );
  }
  if (field.secret) {
    return (
      <SecretField
        field={field}
        idPrefix={idPrefix}
        isSet={remote?.secretsSet.includes(field.name)}
      />
    );
  }
  switch (field.kind) {
    case FieldKind.BOOL:
      return <BoolField field={field} idPrefix={idPrefix} />;
    case FieldKind.INT:
      return <IntField field={field} idPrefix={idPrefix} />;
    case FieldKind.ENUM:
      return <EnumField field={field} idPrefix={idPrefix} />;
    default:
      return <TextField field={field} idPrefix={idPrefix} />;
  }
}

/**
 * DescriptorForm renders a remote's form from its backend descriptor, so a
 * new backend needs no frontend change. Advanced fields are collapsed;
 * secrets are write-only (see SecretField); server field violations land
 * on their fields.
 */
export function DescriptorForm({
  backend,
  remote,
  remotes = [],
  backends = new Map(),
  onSubmit,
  submitLabel,
  onCancel,
  footer,
}: DescriptorFormProps) {
  const idPrefix = useId().replaceAll(":", "");
  const schema = useMemo(
    () => buildSchema(backend, { secretsSet: remote?.secretsSet }),
    [backend, remote?.secretsSet],
  );
  const form = useForm<DescriptorValues>({
    resolver: zodResolver(schema) as unknown as Resolver<DescriptorValues>,
    defaultValues: defaultValues(backend, remote),
    mode: "onTouched",
  });
  const [advancedOpen, setAdvancedOpen] = useState(false);
  const [formError, setFormError] = useState<string | undefined>();

  const all = backend.fields;
  const basic = all.filter((f) => !f.advanced);
  const advanced = all.filter((f) => f.advanced);
  const errors = form.formState.errors;
  const advancedHasError = advanced.some((f) => errors.values?.[f.name]);

  const submit = form.handleSubmit(async (values) => {
    setFormError(undefined);
    try {
      await onSubmit(toRemoteInput(backend, values));
    } catch (err) {
      const { fields, other } = mapViolations(backend, fieldViolations(err));
      for (const f of fields) {
        form.setError(f.path as FieldPath<DescriptorValues>, {
          type: "server",
          message: f.message,
        });
      }
      if (fields.some((f) => advanced.some((a) => f.path === `values.${a.name}`))) {
        setAdvancedOpen(true);
      }
      if (other.length) setFormError(other.map((v) => `${v.field}: ${v.message}`).join(" "));
      else if (!fields.length) setFormError(errorMessage(err));
    }
  });

  const nameId = `${idPrefix}-name`;
  const nameError = errors.name?.message;
  const isUnion = !!membersField(backend);

  return (
    <FormProvider {...form}>
      <form
        onSubmit={(e) => void submit(e)}
        noValidate
        className="flex flex-col gap-6"
        aria-label={`${backend.label} remote`}
      >
        <div className="flex flex-col gap-5">
          <FieldShell
            id={nameId}
            label="Name"
            help="How this remote appears in jobs and pickers. Unique among your remotes."
            error={nameError}
          >
            <Input
              id={nameId}
              className="max-w-md bg-card"
              autoComplete="off"
              placeholder={isUnion ? "Pooled disks" : `My ${backend.label}`}
              aria-invalid={nameError ? true : undefined}
              {...form.register("name")}
            />
          </FieldShell>
          <div className="grid grid-cols-1 gap-x-5 gap-y-5 md:grid-cols-2">
            {basic.map((f) => (
              <div
                key={f.name}
                className={cn(
                  (f.kind === FieldKind.TEXT ||
                    f.kind === FieldKind.MEMBERS ||
                    f.kind === FieldKind.BOOL) &&
                    "md:col-span-2",
                )}
              >
                <FieldFor
                  field={f}
                  idPrefix={idPrefix}
                  remote={remote}
                  remotes={remotes}
                  backends={backends}
                />
              </div>
            ))}
          </div>
        </div>

        {advanced.length > 0 && (
          <Collapsible
            open={advancedOpen || advancedHasError}
            onOpenChange={setAdvancedOpen}
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
                    (advancedOpen || advancedHasError) && "rotate-90",
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
                    <FieldFor
                      field={f}
                      idPrefix={idPrefix}
                      remote={remote}
                      remotes={remotes}
                      backends={backends}
                    />
                  </div>
                ))}
              </div>
            </CollapsibleContent>
          </Collapsible>
        )}

        {formError && (
          <p role="alert" className="rounded-md bg-danger-soft px-3 py-2 text-[13px] text-danger">
            {formError}
          </p>
        )}

        <div className="flex flex-wrap items-center justify-between gap-3 border-t pt-5">
          <div>{footer}</div>
          <div className="flex gap-2">
            {onCancel && (
              <Button type="button" variant="outline" onClick={onCancel}>
                Cancel
              </Button>
            )}
            <Button type="submit" disabled={form.formState.isSubmitting}>
              {form.formState.isSubmitting
                ? "Saving…"
                : (submitLabel ?? (remote ? "Save changes" : "Create remote"))}
            </Button>
          </div>
        </div>
      </form>
    </FormProvider>
  );
}
