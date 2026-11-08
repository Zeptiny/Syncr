import type { MessageInitShape } from "@bufbuild/protobuf";
import { durationMs } from "@bufbuild/protobuf/wkt";
import { useMutation } from "@connectrpc/connect-query";
import { zodResolver } from "@hookform/resolvers/zod";
import { CircleCheck, CircleX, Send } from "lucide-react";
import { useId, useMemo, useState } from "react";
import { Controller, FormProvider, useForm, type FieldPath, type Resolver } from "react-hook-form";
import { z } from "zod";

import {
  DescriptorFields,
  FieldShell,
  fieldDefaults,
  fieldPath,
  fieldsInput,
  fieldsSchema,
  type FieldValues,
} from "@/components/app/descriptor-form";
import { EventChips } from "@/components/app/event-chips";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import {
  ContactService,
  type ChannelType,
  type Contact,
  type ContactInputSchema,
  type NotificationEvent,
  type SendTestNotificationResponse,
} from "@/gen/syncr/v1/notify_pb";
import { errorMessage, fieldViolations } from "@/lib/errors";
import { manualJobEvents } from "@/lib/status";
import { cn } from "@/lib/utils";

/** ContactValues is the contact form's state. */
export interface ContactValues extends FieldValues {
  name: string;
  enabled: boolean;
  jobEvents: NotificationEvent[];
}

/** ContactInputInit is what the form submits. */
export type ContactInputInit = MessageInitShape<typeof ContactInputSchema>;

/** contactDefaults is the initial form state, from the contact when editing. */
export function contactDefaults(channel: ChannelType, contact?: Contact): ContactValues {
  return {
    name: contact?.name ?? "",
    enabled: contact?.enabled ?? true,
    jobEvents: contact ? [...contact.jobEvents] : [],
    values: fieldDefaults(channel.fields, contact),
    clearSecrets: [],
  };
}

/** contactSchema validates the form; stored secrets are not required again. */
export function contactSchema(channel: ChannelType, secretsSet: string[] = []) {
  return z.object({
    name: z
      .string()
      .trim()
      .min(1, "Give the contact a name.")
      .max(64, "Use at most 64 characters."),
    enabled: z.boolean(),
    jobEvents: z.array(z.number()),
    values: fieldsSchema(channel.fields, secretsSet),
    clearSecrets: z.array(z.string()),
  });
}

/** toContactInput converts the form state to the API input. */
export function toContactInput(channel: ChannelType, v: ContactValues): ContactInputInit {
  return {
    name: v.name.trim(),
    channelType: channel.id,
    enabled: v.enabled,
    jobEvents: v.jobEvents,
    ...fieldsInput(channel.fields, v),
  };
}

/** newSigningSecret makes a Standard Webhooks secret: "whsec_" and 24 random bytes. */
export function newSigningSecret(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(24));
  return `whsec_${btoa(String.fromCharCode(...bytes))}`;
}

const generators: Record<string, Record<string, () => string>> = {
  webhook: { signing_secret: newSigningSecret },
};

/** TestResult shows the outcome of "Send test". */
export function TestResult({
  result,
  error,
}: {
  result?: SendTestNotificationResponse;
  error?: unknown;
}) {
  if (!result && error === undefined) return null;
  const ok = !!result?.ok;
  const ms = result?.latency ? Math.round(durationMs(result.latency)) : undefined;
  const reply = result?.status ? `The server replied ${result.status}` : undefined;
  return (
    <div
      role="status"
      className={cn(
        "flex items-start gap-2.5 rounded-lg px-3.5 py-2.5 text-[13px]",
        ok ? "bg-success-soft" : "bg-danger-soft",
      )}
    >
      {ok ? (
        <CircleCheck className="mt-0.5 size-4 shrink-0 text-success" aria-hidden />
      ) : (
        <CircleX className="mt-0.5 size-4 shrink-0 text-danger" aria-hidden />
      )}
      <div className="min-w-0">
        <div className={cn("font-semibold", ok ? "text-success" : "text-danger")}>
          {ok ? "Test notification sent" : "The test notification was not delivered"}
        </div>
        <p className="mt-0.5 break-words">
          {ok
            ? [reply, ms !== undefined ? `in ${ms} ms` : undefined].filter(Boolean).join(" ") +
              ". Check that it arrived."
            : result
              ? `${result.error || "The channel refused it."}${result.status ? ` (status ${result.status})` : ""}`
              : errorMessage(error)}
        </p>
      </div>
    </div>
  );
}

/** ContactFormProps configures ContactForm. */
export interface ContactFormProps {
  channel: ChannelType;
  /** contact is the stored contact when editing. */
  contact?: Contact;
  onSubmit: (input: ContactInputInit) => Promise<unknown>;
  onCancel?: () => void;
  /** footer renders extra actions on the left of the footer (e.g. delete). */
  footer?: React.ReactNode;
}

/**
 * ContactForm edits a contact from its channel's descriptor, like a
 * remote: secrets are write-only, and "Send test" tries the form's current
 * values (unsaved changes included) without saving them.
 */
export function ContactForm({ channel, contact, onSubmit, onCancel, footer }: ContactFormProps) {
  const idPrefix = useId().replaceAll(":", "");
  const schema = useMemo(
    () => contactSchema(channel, contact?.secretsSet),
    [channel, contact?.secretsSet],
  );
  const form = useForm<ContactValues>({
    resolver: zodResolver(schema) as unknown as Resolver<ContactValues>,
    defaultValues: contactDefaults(channel, contact),
    mode: "onTouched",
  });
  const sendTest = useMutation(ContactService.method.sendTestNotification);
  const [test, setTest] = useState<{ result?: SendTestNotificationResponse; error?: unknown }>({});
  const [formError, setFormError] = useState<string | undefined>();
  const [advancedOpen, setAdvancedOpen] = useState(false);
  const errors = form.formState.errors;

  const applyServer = (err: unknown): boolean => {
    const rest: string[] = [];
    let placed = 0;
    for (const v of fieldViolations(err)) {
      const path = v.field === "name" ? "name" : fieldPath(channel.fields, v.field);
      if (path) {
        form.setError(path as FieldPath<ContactValues>, { type: "server", message: v.message });
        placed++;
        if (channel.fields.some((f) => f.advanced && path === `values.${f.name}`))
          setAdvancedOpen(true);
      } else rest.push(`${v.field}: ${v.message}`);
    }
    if (rest.length) setFormError(rest.join(" "));
    return placed > 0 || rest.length > 0;
  };

  const submit = form.handleSubmit(async (values) => {
    setFormError(undefined);
    try {
      await onSubmit(toContactInput(channel, values));
    } catch (err) {
      if (!applyServer(err)) setFormError(errorMessage(err));
    }
  });

  const runTest = async () => {
    setTest({});
    // A saved contact fills in what the form leaves empty; a new one needs it all.
    const ok = contact ? true : await form.trigger("values");
    if (!ok) return;
    try {
      const result = await sendTest.mutateAsync({
        contactId: contact?.id ?? "",
        input: toContactInput(channel, form.getValues()),
      });
      setTest({ result });
    } catch (err) {
      if (!applyServer(err)) setTest({ error: err });
    }
  };

  const nameId = `${idPrefix}-name`;
  return (
    <FormProvider {...form}>
      <form
        onSubmit={(e) => void submit(e)}
        noValidate
        className="flex flex-col gap-6"
        aria-label={`${channel.label} contact`}
      >
        <div className="flex flex-col gap-5">
          <div className="flex flex-col gap-5 sm:flex-row sm:items-end">
            <FieldShell
              id={nameId}
              label="Name"
              help="How this contact appears when you pick who a schedule notifies."
              error={errors.name?.message}
            >
              <Input
                id={nameId}
                className="bg-card sm:w-80"
                autoComplete="off"
                placeholder={channel.id === "email" ? "Admin mail" : `My ${channel.label}`}
                aria-invalid={errors.name ? true : undefined}
                {...form.register("name")}
              />
            </FieldShell>
            <Controller
              control={form.control}
              name="enabled"
              render={({ field }) => (
                <Label className="flex h-9 items-center gap-2.5 text-[13px] font-normal sm:ml-auto">
                  <Switch checked={field.value} onCheckedChange={field.onChange} />
                  {field.value ? "Enabled" : "Disabled: sends nothing new"}
                </Label>
              )}
            />
          </div>
          <DescriptorFields
            descriptor={channel}
            secretsSet={contact?.secretsSet}
            idPrefix={idPrefix}
            generators={generators[channel.id]}
            openAdvanced={advancedOpen}
            onOpenAdvancedChange={setAdvancedOpen}
          />
        </div>

        <fieldset className="flex flex-col gap-2 rounded-lg border bg-muted/40 px-4 py-3.5">
          <legend className="sr-only">Jobs you run by hand</legend>
          <div className="text-[13px] font-medium">Jobs you run by hand</div>
          <p className="-mt-1 text-xs leading-5 text-muted-foreground">
            Scheduled jobs notify the contacts their schedule lists. Choose here what this contact
            hears about the jobs you start yourself.
          </p>
          <Controller
            control={form.control}
            name="jobEvents"
            render={({ field }) => (
              <EventChips
                value={field.value}
                onChange={field.onChange}
                options={manualJobEvents}
                label="Events of jobs run by hand"
              />
            )}
          />
        </fieldset>

        <div className="flex flex-col gap-3">
          <div className="flex flex-wrap items-center gap-3">
            <Button
              type="button"
              variant="outline"
              onClick={() => void runTest()}
              disabled={sendTest.isPending}
            >
              <Send aria-hidden />
              {sendTest.isPending ? "Sending…" : "Send test"}
            </Button>
            <span className="text-xs text-muted-foreground">
              Sends a test notification with the values above{contact ? ", saved or not" : ""}.
            </span>
          </div>
          <TestResult result={test.result} error={test.error} />
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
              {form.formState.isSubmitting ? "Saving…" : contact ? "Save changes" : "Add contact"}
            </Button>
          </div>
        </div>
      </form>
    </FormProvider>
  );
}
