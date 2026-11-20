import type { MessageInitShape } from "@bufbuild/protobuf";
import { z } from "zod";

import type { FieldViolation } from "@/gen/syncr/v1/api_pb";
import { UnionUpstreamMode } from "@/gen/syncr/v1/link_pb";
import {
  FieldKind,
  type Backend,
  type BackendField,
  type Remote,
  type RemoteInputSchema,
} from "@/gen/syncr/v1/remotes_pb";

/** MemberValue is one union member row in the form. */
export interface MemberValue {
  remoteId: string;
  path: string;
  /** mode is a UnionUpstreamMode number as a string (for the select). */
  mode: string;
}

/**
 * FieldValues is the part of a form that DescriptorFields edits: every
 * descriptor field by name, and the stored secrets to clear on save.
 */
export interface FieldValues {
  /** values holds every non-members field by name (booleans for BOOL). */
  values: Record<string, string | boolean>;
  /** clearSecrets lists optional secrets to remove on save (edit only). */
  clearSecrets: string[];
}

/** WrappedValue is where a wrapper remote stores its data, in the form. */
export interface WrappedValue {
  remoteId: string;
  path: string;
}

/** DescriptorValues is the form state of a remote. */
export interface DescriptorValues extends FieldValues {
  name: string;
  members: MemberValue[];
  /** wrapped is used only by wrapper types (a WRAPPED field); empty otherwise. */
  wrapped: WrappedValue;
}

/** Stored is what a saved remote or contact already holds. */
export interface Stored {
  options: Record<string, string>;
  secretsSet: string[];
}

/** FieldsInput is what DescriptorFields contributes to an API input. */
export interface FieldsInput {
  options: Record<string, string>;
  secrets: Record<string, string>;
  clearSecrets: string[];
}

/** RemoteInputInit is what the form submits. */
export type RemoteInputInit = MessageInitShape<typeof RemoteInputSchema>;

/** SchemaContext says what the form edits. */
export interface SchemaContext {
  /** secretsSet lists secrets the stored remote already has (edit). */
  secretsSet?: string[];
  /**
   * editing says the form edits a stored remote: its fixed fields are
   * read-only, so they are not checked.
   */
  editing?: boolean;
}

/**
 * isValueField reports whether a field holds an option or a secret. MEMBERS
 * and WRAPPED fields only mark where union members and the wrapped remote
 * go in the form.
 */
export function isValueField(f: BackendField): boolean {
  return f.kind !== FieldKind.MEMBERS && f.kind !== FieldKind.WRAPPED;
}

/** formFields are the descriptor's value fields (not MEMBERS or WRAPPED). */
export function formFields(descriptor: { fields: BackendField[] }): BackendField[] {
  return descriptor.fields.filter(isValueField);
}

/** membersField returns the MEMBERS field, if the backend is a union. */
export function membersField(backend: Backend): BackendField | undefined {
  return backend.fields.find((f) => f.kind === FieldKind.MEMBERS);
}

/** wrappedField returns the WRAPPED field, if the backend stores its data in another remote. */
export function wrappedField(backend: Backend): BackendField | undefined {
  return backend.fields.find((f) => f.kind === FieldKind.WRAPPED);
}

/**
 * fixedSecretFields are the secrets that are set once, when the remote is
 * created, and never shown again: losing one loses the stored data, so a
 * create form warns about them and asks the user to keep a copy.
 */
export function fixedSecretFields(descriptor: { fields: BackendField[] }): BackendField[] {
  return descriptor.fields.filter((f) => f.fixed && f.secret && isValueField(f));
}

function fieldSchema(f: BackendField, ctx: SchemaContext): z.ZodType {
  if (f.kind === FieldKind.BOOL) return z.boolean();
  const label = f.label || f.name;
  const keep = f.secret && (ctx.secretsSet ?? []).includes(f.name);
  const checks: [(v: string) => boolean, string][] = [];
  if (f.required && !keep) {
    checks.push([
      (v) => v.trim() !== "",
      f.kind === FieldKind.ENUM ? `Choose a ${label.toLowerCase()}.` : `${label} is required.`,
    ]);
  }
  if (f.kind === FieldKind.INT) {
    checks.push([(v) => v.trim() === "" || /^-?\d+$/.test(v.trim()), "Enter a whole number."]);
  }
  if (f.kind === FieldKind.ENUM) {
    const values = f.choices.map((c) => c.value);
    checks.push([(v) => v === "" || values.includes(v), "Choose one of the options."]);
  }
  let schema: z.ZodType<string> = z.string();
  for (const [ok, message] of checks) schema = schema.refine(ok, message);
  return schema;
}

/**
 * fieldsSchema is the zod schema of the descriptor fields' values: required
 * fields, integers and enums. Stored secrets are not required again, and
 * fixed fields are not checked when editing (they are read-only).
 */
export function fieldsSchema(fields: BackendField[], secretsSet: string[] = [], editing = false) {
  const shape: Record<string, z.ZodType> = {};
  for (const f of fields) {
    if (!isValueField(f)) continue;
    // A fixed field is read-only when editing: nothing there can be fixed.
    shape[f.name] = editing && f.fixed ? z.any() : fieldSchema(f, { secretsSet });
  }
  return z.object(shape);
}

/**
 * fieldDefaults returns the initial values of the fields, from what is
 * stored when editing. Options it lacks take the descriptor default; an
 * enum value that is not one of its choices starts empty; secrets start
 * empty (write-only).
 */
export function fieldDefaults(
  fields: BackendField[],
  stored?: Pick<Stored, "options">,
): Record<string, string | boolean> {
  const values: Record<string, string | boolean> = {};
  for (const f of fields) {
    if (!isValueField(f)) continue;
    // A value the stored remote lacks falls back to the descriptor default,
    // on edit as well as create, so the form shows what the backend uses.
    const value = stored?.options[f.name] ?? f.defaultValue;
    if (f.kind === FieldKind.BOOL) {
      values[f.name] = value === "true";
    } else if (f.secret) {
      values[f.name] = "";
    } else if (f.kind === FieldKind.ENUM) {
      // A select can only show one of its choices; anything else would be
      // submitted without the user seeing it, so it starts unchosen.
      values[f.name] = f.choices.some((c) => c.value === value) ? value : "";
    } else {
      values[f.name] = value;
    }
  }
  return values;
}

/**
 * fieldsInput converts the field values to options and secrets. Empty
 * secrets are omitted (on update that keeps the stored value); booleans
 * are sent only when they differ from the descriptor default.
 *
 * With stored (an update), fixed fields are never taken from the form: a
 * fixed option is sent as stored (omitted when the remote has none, so the
 * same default applies), and a fixed secret is never sent or cleared (the
 * controller refuses any value for it, even the stored one).
 */
export function fieldsInput(
  fields: BackendField[],
  v: FieldValues,
  stored?: Pick<Stored, "options">,
): FieldsInput {
  const options: Record<string, string> = {};
  const secrets: Record<string, string> = {};
  for (const f of fields) {
    if (!isValueField(f)) continue;
    if (stored && f.fixed) {
      const kept = stored.options[f.name];
      if (!f.secret && kept) options[f.name] = kept;
      continue;
    }
    const value = v.values[f.name];
    if (f.kind === FieldKind.BOOL) {
      const on = value === true;
      if (on !== (f.defaultValue === "true")) options[f.name] = String(on);
      continue;
    }
    const s = typeof value === "string" ? value : "";
    if (f.secret) {
      if (s !== "" && !v.clearSecrets.includes(f.name)) secrets[f.name] = s;
      continue;
    }
    const out = f.kind === FieldKind.TEXT ? s : s.trim();
    if (out !== "") options[f.name] = out;
  }
  const fixed = new Set(fields.filter((f) => f.fixed).map((f) => f.name));
  const clearSecrets = stored ? v.clearSecrets.filter((n) => !fixed.has(n)) : v.clearSecrets;
  return { options, secrets, clearSecrets };
}

/**
 * fieldPath maps an "options.x" or "secrets.x" violation to the form path
 * of field x; undefined when x is not one of the fields.
 */
export function fieldPath(fields: BackendField[], field: string): string | undefined {
  const opt = /^(?:options|secrets)\.([a-z][a-z0-9_]*)$/.exec(field);
  if (!opt) return undefined;
  return fields.some((f) => f.name === opt[1] && isValueField(f)) ? `values.${opt[1]}` : undefined;
}

/**
 * buildSchema derives the zod schema of a backend's form from its
 * descriptor: required fields, integer and enum parsing, at least two
 * union members, and the remote a wrapper stores its data in. Stored
 * secrets are not required again when editing.
 */
export function buildSchema(backend: Backend, ctx: SchemaContext = {}) {
  const member = z.object({
    remoteId: z.string().min(1, "Choose a remote."),
    path: z.string(),
    mode: z.string(),
  });
  const mf = membersField(backend);
  const wf = wrappedField(backend);
  return z.object({
    name: z.string().trim().min(1, "Give the remote a name.").max(64, "Use at most 64 characters."),
    values: fieldsSchema(backend.fields, ctx.secretsSet, ctx.editing),
    members: mf
      ? z.array(member).min(mf.required ? 2 : 0, "A union needs at least two members.")
      : z.array(member),
    wrapped: z.object({
      remoteId: wf?.required ? z.string().min(1, "Choose a remote.") : z.string(),
      path: z.string(),
    }),
    clearSecrets: z.array(z.string()),
  });
}

/**
 * defaultValues returns the initial form state, from a remote when editing.
 * Options the remote lacks take the descriptor default; an enum value that
 * is not one of its choices starts empty.
 */
export function defaultValues(backend: Backend, remote?: Remote): DescriptorValues {
  const values = fieldDefaults(backend.fields, remote);
  const members: MemberValue[] = remote
    ? remote.members.map((m) => ({
        remoteId: m.remoteId,
        path: m.path,
        mode: String(m.mode || UnionUpstreamMode.READ_WRITE),
      }))
    : membersField(backend)
      ? [emptyMember(), emptyMember()]
      : [];
  const wrapped = {
    remoteId: remote?.wrapped?.remoteId ?? "",
    path: remote?.wrapped?.path ?? "",
  };
  return { name: remote?.name ?? "", values, members, wrapped, clearSecrets: [] };
}

/** emptyMember is a new union member row. */
export function emptyMember(): MemberValue {
  return { remoteId: "", path: "", mode: String(UnionUpstreamMode.READ_WRITE) };
}

/**
 * toRemoteInput converts the form state to the API input. Empty secrets
 * are omitted (on update that keeps the stored value); booleans are sent
 * only when they differ from the descriptor default. With the stored
 * remote (an update or a test of it), fixed fields keep what it stores
 * (see fieldsInput). A wrapper type sends where it stores its data.
 */
export function toRemoteInput(
  backend: Backend,
  v: DescriptorValues,
  remote?: Remote,
): RemoteInputInit {
  return {
    name: v.name.trim(),
    backendType: backend.id,
    ...fieldsInput(backend.fields, v, remote),
    members: membersField(backend)
      ? v.members.map((m) => ({ remoteId: m.remoteId, path: m.path.trim(), mode: Number(m.mode) }))
      : [],
    ...(wrappedField(backend)
      ? { wrapped: { remoteId: v.wrapped.remoteId, path: v.wrapped.path.trim() } }
      : {}),
  };
}

/** formPath maps a server violation field to a form path; undefined if unknown. */
export function formPath(backend: Backend, field: string): string | undefined {
  if (field === "name") return "name";
  const opt = fieldPath(backend.fields, field);
  if (opt) return opt;
  if (field === "members" || field === membersField(backend)?.name) return "members.root";
  const wf = wrappedField(backend);
  if (wf && (field === "wrapped" || field === "wrapped.remote_id")) return "wrapped.remoteId";
  if (wf && field === "wrapped.path") return "wrapped.path";
  const mem = /^members\[(\d+)\](?:\.(remote_id|path|mode))?$/.exec(field);
  if (mem) {
    const sub = mem[2] === "path" ? "path" : mem[2] === "mode" ? "mode" : "remoteId";
    return `members.${mem[1]}.${sub}`;
  }
  return undefined;
}

/** mapViolations splits violations into form-field errors and the rest. */
export function mapViolations(
  backend: Backend,
  violations: FieldViolation[],
): { fields: { path: string; message: string }[]; other: FieldViolation[] } {
  const fields: { path: string; message: string }[] = [];
  const other: FieldViolation[] = [];
  for (const v of violations) {
    const path = formPath(backend, v.field);
    if (path) fields.push({ path, message: v.message });
    else other.push(v);
  }
  return { fields, other };
}

/**
 * FIXED_NOTE explains why fixed fields cannot be edited. The form shows it
 * once above the locked fields of a section, and each of them refers to it.
 */
export const FIXED_NOTE =
  "Locked settings were set when the remote was created. Changing them would make the files already stored unreadable.";

/** fixedDisplay is how a fixed field's stored value reads. */
export function fixedDisplay(field: BackendField, value: string | undefined, isSet?: boolean) {
  if (field.secret) return isSet ? "Set" : "Not set";
  const v = value || field.defaultValue;
  if (field.kind === FieldKind.BOOL) return v === "true" ? "On" : "Off";
  if (field.kind === FieldKind.ENUM) {
    const choice = field.choices.find((c) => c.value === v);
    return choice ? choice.label || choice.value : v || "Not set";
  }
  return v || "Not set";
}

/**
 * canBeWrapped reports whether a remote can hold another remote's data: a
 * known type that is not a worker's folder, a union or a wrapper itself.
 */
export function canBeWrapped(backend: Backend | undefined): boolean {
  const c = backend?.capabilities;
  return !!backend && !c?.local && !c?.union && !c?.wrapper;
}
