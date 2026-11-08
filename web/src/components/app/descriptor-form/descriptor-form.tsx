import { zodResolver } from "@hookform/resolvers/zod";
import { useId, useMemo, useState } from "react";
import { FormProvider, useForm, type FieldPath, type Resolver } from "react-hook-form";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { FieldKind, type Backend, type Remote } from "@/gen/syncr/v1/remotes_pb";
import { errorMessage, fieldViolations } from "@/lib/errors";

import { DescriptorFields } from "./descriptor-fields";
import { FieldShell, MembersField } from "./fields";
import {
  buildSchema,
  defaultValues,
  mapViolations,
  membersField,
  toRemoteInput,
  type DescriptorValues,
  type RemoteInputInit,
} from "./schema";

/** RemoteFormProps configures RemoteForm. */
export interface RemoteFormProps {
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

/**
 * RemoteForm renders a remote's form: its name, the backend descriptor's
 * fields (DescriptorFields) and, for unions, the members. Server field
 * violations land on their fields.
 */
export function RemoteForm({
  backend,
  remote,
  remotes = [],
  backends = new Map(),
  onSubmit,
  submitLabel,
  onCancel,
  footer,
}: RemoteFormProps) {
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

  const advanced = backend.fields.filter((f) => f.advanced);
  const errors = form.formState.errors;

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
          <DescriptorFields
            descriptor={backend}
            secretsSet={remote?.secretsSet}
            idPrefix={idPrefix}
            openAdvanced={advancedOpen}
            onOpenAdvancedChange={setAdvancedOpen}
            renderField={(f) =>
              f.kind === FieldKind.MEMBERS ? (
                <MembersField
                  field={f}
                  idPrefix={idPrefix}
                  remotes={remotes}
                  backends={backends}
                  selfId={remote?.id}
                />
              ) : undefined
            }
          />
        </div>

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
