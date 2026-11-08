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

/** DescriptorValues is the form state of a remote. */
export interface DescriptorValues {
  name: string;
  /** values holds every non-members field by name (booleans for BOOL). */
  values: Record<string, string | boolean>;
  members: MemberValue[];
  /** clearSecrets lists optional secrets to remove on save (edit only). */
  clearSecrets: string[];
}

/** RemoteInputInit is what the form submits. */
export type RemoteInputInit = MessageInitShape<typeof RemoteInputSchema>;

/** SchemaContext says what the form edits. */
export interface SchemaContext {
  /** secretsSet lists secrets the stored remote already has (edit). */
  secretsSet?: string[];
}

/** formFields are the descriptor's fields other than MEMBERS. */
export function formFields(backend: Backend): BackendField[] {
  return backend.fields.filter((f) => f.kind !== FieldKind.MEMBERS);
}

/** membersField returns the MEMBERS field, if the backend is a union. */
export function membersField(backend: Backend): BackendField | undefined {
  return backend.fields.find((f) => f.kind === FieldKind.MEMBERS);
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
 * buildSchema derives the zod schema of a backend's form from its
 * descriptor: required fields, integer and enum parsing, and at least two
 * union members. Stored secrets are not required again when editing.
 */
export function buildSchema(backend: Backend, ctx: SchemaContext = {}) {
  const shape: Record<string, z.ZodType> = {};
  for (const f of formFields(backend)) shape[f.name] = fieldSchema(f, ctx);
  const member = z.object({
    remoteId: z.string().min(1, "Choose a remote."),
    path: z.string(),
    mode: z.string(),
  });
  const mf = membersField(backend);
  return z.object({
    name: z.string().trim().min(1, "Give the remote a name.").max(64, "Use at most 64 characters."),
    values: z.object(shape),
    members: mf
      ? z.array(member).min(mf.required ? 2 : 0, "A union needs at least two members.")
      : z.array(member),
    clearSecrets: z.array(z.string()),
  });
}

/**
 * defaultValues returns the initial form state, from a remote when editing.
 * Options the remote lacks take the descriptor default; an enum value that
 * is not one of its choices starts empty.
 */
export function defaultValues(backend: Backend, remote?: Remote): DescriptorValues {
  const values: Record<string, string | boolean> = {};
  for (const f of formFields(backend)) {
    // A value the stored remote lacks falls back to the descriptor default,
    // on edit as well as create, so the form shows what the backend uses.
    const value = remote?.options[f.name] ?? f.defaultValue;
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
  const members: MemberValue[] = remote
    ? remote.members.map((m) => ({
        remoteId: m.remoteId,
        path: m.path,
        mode: String(m.mode || UnionUpstreamMode.READ_WRITE),
      }))
    : membersField(backend)
      ? [emptyMember(), emptyMember()]
      : [];
  return { name: remote?.name ?? "", values, members, clearSecrets: [] };
}

/** emptyMember is a new union member row. */
export function emptyMember(): MemberValue {
  return { remoteId: "", path: "", mode: String(UnionUpstreamMode.READ_WRITE) };
}

/**
 * toRemoteInput converts the form state to the API input. Empty secrets
 * are omitted (on update that keeps the stored value); booleans are sent
 * only when they differ from the descriptor default.
 */
export function toRemoteInput(backend: Backend, v: DescriptorValues): RemoteInputInit {
  const options: Record<string, string> = {};
  const secrets: Record<string, string> = {};
  for (const f of formFields(backend)) {
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
  return {
    name: v.name.trim(),
    backendType: backend.id,
    options,
    secrets,
    clearSecrets: v.clearSecrets,
    members: membersField(backend)
      ? v.members.map((m) => ({ remoteId: m.remoteId, path: m.path.trim(), mode: Number(m.mode) }))
      : [],
  };
}

/** formPath maps a server violation field to a form path; undefined if unknown. */
export function formPath(backend: Backend, field: string): string | undefined {
  if (field === "name") return "name";
  const opt = /^(?:options|secrets)\.([a-z][a-z0-9_]*)$/.exec(field);
  if (opt)
    return formFields(backend).some((f) => f.name === opt[1]) ? `values.${opt[1]}` : undefined;
  if (field === "members" || field === membersField(backend)?.name) return "members.root";
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
