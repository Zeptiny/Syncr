import { Eye, EyeOff, KeyRound, Plus, Trash2, Undo2, WandSparkles } from "lucide-react";
import { useState } from "react";
import {
  Controller,
  get,
  useFieldArray,
  useFormContext,
  type FieldError,
  type FieldErrors,
} from "react-hook-form";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { UnionUpstreamMode } from "@/gen/syncr/v1/link_pb";
import { FieldKind, type Backend, type BackendField, type Remote } from "@/gen/syncr/v1/remotes_pb";
import { cn } from "@/lib/utils";

import { PathInput } from "../path-input";
import { RemotePicker } from "../remote-picker";
import { emptyMember, type DescriptorValues, type FieldValues } from "./schema";

/** FieldProps is shared by the descriptor field components. */
export interface FieldProps {
  field: BackendField;
  /** idPrefix keeps input IDs unique on the page. */
  idPrefix: string;
}

/** isOptional marks fields the user may leave empty (no default, not required). */
function isOptional(field: BackendField): boolean {
  return !field.required && !field.defaultValue;
}

function errorAt(errors: FieldErrors<FieldValues>, path: string): string | undefined {
  return get<FieldErrors<FieldValues>, FieldError | undefined>(errors, path)?.message;
}

/** FieldShell lays out a label, the control, help and the error. */
export function FieldShell({
  id,
  label,
  optional,
  help,
  error,
  aside,
  children,
}: {
  id: string;
  label: React.ReactNode;
  optional?: boolean;
  help?: React.ReactNode;
  error?: string;
  aside?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <div className="flex min-w-0 flex-col gap-1.5" data-field={id}>
      <div className="flex min-h-5 items-center justify-between gap-2">
        <Label htmlFor={id} className="text-[13px]">
          {label}
          {optional && <span className="font-normal text-muted-foreground">(optional)</span>}
        </Label>
        {aside}
      </div>
      {children}
      {error ? (
        <p id={`${id}-error`} role="alert" className="text-xs leading-5 text-danger">
          {error}
        </p>
      ) : (
        help && (
          <p id={`${id}-help`} className="text-xs leading-5 text-muted-foreground">
            {help}
          </p>
        )
      )}
    </div>
  );
}

function useFieldState(field: BackendField, idPrefix: string) {
  const form = useFormContext<FieldValues>();
  const path = `values.${field.name}` as const;
  const error = errorAt(form.formState.errors, path);
  const id = `${idPrefix}-${field.name}`;
  return { form, path, error, id, describedBy: error ? `${id}-error` : `${id}-help` };
}

/** TextField edits STRING and TEXT fields. */
export function TextField({ field, idPrefix }: FieldProps) {
  const { form, path, error, id, describedBy } = useFieldState(field, idPrefix);
  const common = {
    id,
    placeholder: field.placeholder,
    "aria-invalid": error ? true : undefined,
    "aria-describedby": describedBy,
    ...form.register(path),
  };
  return (
    <FieldShell
      id={id}
      label={field.label}
      optional={isOptional(field)}
      help={field.help}
      error={error}
    >
      {field.kind === FieldKind.TEXT ? (
        <Textarea
          rows={4}
          className="bg-card font-mono text-[13px]"
          spellCheck={false}
          {...common}
        />
      ) : (
        <Input className="bg-card" autoComplete="off" spellCheck={false} {...common} />
      )}
    </FieldShell>
  );
}

/** IntField edits INT fields. */
export function IntField({ field, idPrefix }: FieldProps) {
  const { form, path, error, id, describedBy } = useFieldState(field, idPrefix);
  return (
    <FieldShell
      id={id}
      label={field.label}
      optional={isOptional(field)}
      help={field.help}
      error={error}
    >
      <Input
        id={id}
        inputMode="numeric"
        className="tabular max-w-48 bg-card"
        placeholder={field.placeholder || field.defaultValue}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy}
        {...form.register(path)}
      />
    </FieldShell>
  );
}

/** BoolField edits BOOL fields with a switch. */
export function BoolField({ field, idPrefix }: FieldProps) {
  const { form, path, id } = useFieldState(field, idPrefix);
  return (
    <div className="flex items-start justify-between gap-4 py-1" data-field={id}>
      <div className="min-w-0">
        <Label htmlFor={id} className="text-[13px]">
          {field.label}
        </Label>
        {field.help && (
          <p className="mt-0.5 text-xs leading-5 text-muted-foreground">{field.help}</p>
        )}
      </div>
      <Controller
        control={form.control}
        name={path}
        render={({ field: f }) => (
          <Switch
            id={id}
            checked={f.value === true}
            onCheckedChange={(v) => f.onChange(v)}
            onBlur={f.onBlur}
          />
        )}
      />
    </div>
  );
}

/** EnumField edits ENUM fields with a select. */
export function EnumField({ field, idPrefix }: FieldProps) {
  const { form, path, error, id } = useFieldState(field, idPrefix);
  return (
    <FieldShell
      id={id}
      label={field.label}
      optional={isOptional(field)}
      help={field.help}
      error={error}
    >
      <Controller
        control={form.control}
        name={path}
        render={({ field: f }) => (
          <Select
            value={typeof f.value === "string" && f.value ? f.value : undefined}
            onValueChange={f.onChange}
          >
            <SelectTrigger
              id={id}
              className="w-full bg-card"
              aria-invalid={error ? true : undefined}
              onBlur={f.onBlur}
            >
              <SelectValue placeholder={field.placeholder || "Choose…"} />
            </SelectTrigger>
            <SelectContent>
              {field.choices.map((c) => (
                <SelectItem key={c.value} value={c.value}>
                  {c.label || c.value}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        )}
      />
    </FieldShell>
  );
}

/** SecretFieldProps adds whether the stored remote has the secret. */
export interface SecretFieldProps extends FieldProps {
  /** isSet is true when editing a remote or contact that already stores this secret. */
  isSet?: boolean;
  /** generate, when given, offers a "Generate" button that fills in a new value. */
  generate?: () => string;
}

/**
 * SecretField edits a write-only secret. When the remote already stores it,
 * an empty input keeps it; optional secrets can be cleared.
 */
export function SecretField({ field, idPrefix, isSet, generate }: SecretFieldProps) {
  const { form, path, error, id, describedBy } = useFieldState(field, idPrefix);
  const [reveal, setReveal] = useState(false);
  const clear = form.watch("clearSecrets");
  const cleared = clear.includes(field.name);
  const setClear = (on: boolean) => {
    const next = on ? [...clear, field.name] : clear.filter((n) => n !== field.name);
    form.setValue("clearSecrets", next, { shouldDirty: true });
    if (on) form.setValue(path, "", { shouldDirty: true });
  };
  const placeholder = isSet ? "Set. Leave empty to keep it." : field.placeholder;
  const multiline = field.kind === FieldKind.TEXT;

  const generateButton = generate && !cleared && (
    <Button
      type="button"
      variant="ghost"
      size="xs"
      onClick={() => {
        form.setValue(path, generate(), { shouldDirty: true, shouldValidate: true });
        setReveal(true);
      }}
    >
      <WandSparkles aria-hidden />
      Generate
    </Button>
  );
  const aside = isSet ? (
    <span className="flex items-center gap-2">
      {generateButton}
      {!cleared && (
        <span className="inline-flex items-center gap-1 rounded-full bg-success-soft px-2 py-0.5 text-[11px] font-medium text-success">
          <KeyRound className="size-3" aria-hidden />
          Saved
        </span>
      )}
      {!field.required &&
        (cleared ? (
          <Button type="button" variant="ghost" size="xs" onClick={() => setClear(false)}>
            <Undo2 aria-hidden />
            Keep
          </Button>
        ) : (
          <Button
            type="button"
            variant="ghost"
            size="xs"
            className="text-danger hover:text-danger"
            onClick={() => setClear(true)}
            aria-label={`Clear ${field.label}`}
          >
            Clear
          </Button>
        ))}
    </span>
  ) : (
    generateButton || undefined
  );

  return (
    <FieldShell
      id={id}
      label={field.label}
      optional={isOptional(field)}
      help={field.help}
      error={error}
      aside={aside}
    >
      {cleared ? (
        <div className="flex h-9 items-center rounded-md border border-dashed border-danger/40 bg-danger-soft px-3 text-[13px] text-danger">
          Removed when you save.
        </div>
      ) : multiline ? (
        <Textarea
          id={id}
          rows={4}
          placeholder={placeholder}
          className="bg-card font-mono text-[13px]"
          spellCheck={false}
          autoComplete="off"
          aria-invalid={error ? true : undefined}
          aria-describedby={describedBy}
          {...form.register(path)}
        />
      ) : (
        <div className="relative">
          <Input
            id={id}
            type={reveal ? "text" : "password"}
            placeholder={placeholder}
            className="bg-card pr-10"
            autoComplete="new-password"
            spellCheck={false}
            aria-invalid={error ? true : undefined}
            aria-describedby={describedBy}
            {...form.register(path)}
          />
          <Button
            type="button"
            variant="ghost"
            size="icon-sm"
            className="absolute top-0.5 right-0.5 text-muted-foreground"
            onClick={() => setReveal((r) => !r)}
            aria-label={reveal ? `Hide ${field.label}` : `Show ${field.label}`}
          >
            {reveal ? <EyeOff aria-hidden /> : <Eye aria-hidden />}
          </Button>
        </div>
      )}
    </FieldShell>
  );
}

const modes = [
  { value: String(UnionUpstreamMode.READ_WRITE), label: "Read and write" },
  { value: String(UnionUpstreamMode.READ_ONLY), label: "Read only" },
  { value: String(UnionUpstreamMode.NO_CREATE), label: "No new files" },
];

/** MembersFieldProps configures MembersField. */
export interface MembersFieldProps extends FieldProps {
  remotes: Remote[];
  backends: Map<string, Backend>;
  /** selfId excludes the union being edited. */
  selfId?: string;
}

/**
 * MembersField edits a union's members: a remote that can be a member, a
 * path inside it and the upstream mode.
 */
export function MembersField({ field, idPrefix, remotes, backends, selfId }: MembersFieldProps) {
  const form = useFormContext<DescriptorValues>();
  const { fields, append, remove } = useFieldArray({ control: form.control, name: "members" });
  const errors = form.formState.errors;
  const rootError = errorAt(errors, "members.root") ?? errorAt(errors, "members");
  const id = `${idPrefix}-${field.name}`;
  return (
    <fieldset className="flex flex-col gap-3" data-field={id} aria-describedby={`${id}-help`}>
      {/* A legend is not a flex item, so the gap does not apply to it. */}
      <legend className="mb-0.5 text-[13px] leading-5 font-medium">
        {field.label || "Members"}
      </legend>
      <p id={`${id}-help`} className="-mt-1 text-xs leading-5 text-muted-foreground">
        {field.help || "The remotes this union combines, in order."}
      </p>
      <ol className="flex flex-col gap-2">
        {fields.map((row, i) => {
          const remoteErr = errorAt(errors, `members.${i}.remoteId`);
          const pathErr = errorAt(errors, `members.${i}.path`);
          const chosen = remotes.find((r) => r.id === form.watch(`members.${i}.remoteId`));
          return (
            <li
              key={row.id}
              className="grid grid-cols-1 gap-2 rounded-lg border bg-muted/50 p-3 sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_10rem_auto] sm:items-start"
            >
              <div className="flex flex-col gap-1">
                <Label htmlFor={`${id}-${i}-remote`} className="sr-only">
                  Member {i + 1} remote
                </Label>
                <Controller
                  control={form.control}
                  name={`members.${i}.remoteId`}
                  render={({ field: f }) => (
                    <RemotePicker
                      id={`${id}-${i}-remote`}
                      remotes={remotes}
                      backends={backends}
                      value={f.value}
                      onChange={f.onChange}
                      invalid={!!remoteErr}
                      className="w-full bg-card"
                      filter={(r, b) =>
                        r.id !== selfId && !!b?.capabilities?.unionMember && !b.capabilities.union
                      }
                    />
                  )}
                />
                {remoteErr && <p className="text-xs text-danger">{remoteErr}</p>}
              </div>
              <div className="flex flex-col gap-1">
                <Label htmlFor={`${id}-${i}-path`} className="sr-only">
                  Member {i + 1} path
                </Label>
                <Controller
                  control={form.control}
                  name={`members.${i}.path`}
                  render={({ field: f }) => (
                    <PathInput
                      id={`${id}-${i}-path`}
                      value={f.value}
                      onChange={f.onChange}
                      prefix={chosen?.name}
                      placeholder="path (optional)"
                      invalid={!!pathErr}
                    />
                  )}
                />
                {pathErr && <p className="text-xs text-danger">{pathErr}</p>}
              </div>
              <Controller
                control={form.control}
                name={`members.${i}.mode`}
                render={({ field: f }) => (
                  <Select value={f.value} onValueChange={f.onChange}>
                    <SelectTrigger className="w-full bg-card" aria-label={`Member ${i + 1} mode`}>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {modes.map((m) => (
                        <SelectItem key={m.value} value={m.value}>
                          {m.label}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                )}
              />
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className="justify-self-end text-muted-foreground"
                onClick={() => remove(i)}
                aria-label={`Remove member ${i + 1}`}
              >
                <Trash2 aria-hidden />
              </Button>
            </li>
          );
        })}
      </ol>
      {rootError && (
        <p role="alert" className={cn("text-xs text-danger")}>
          {rootError}
        </p>
      )}
      <div>
        <Button type="button" variant="outline" size="sm" onClick={() => append(emptyMember())}>
          <Plus aria-hidden />
          Add member
        </Button>
      </div>
    </fieldset>
  );
}
