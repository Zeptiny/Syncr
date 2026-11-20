import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { ValidationErrorSchema } from "@/gen/syncr/v1/api_pb";
import { BackendSchema, RemoteSchema, type Remote } from "@/gen/syncr/v1/remotes_pb";
import { URL_SAFE_ALPHABET } from "@/lib/random-secret";
import * as fx from "@/test/fixtures";
import { renderWithProviders } from "@/test/render";

import { RemoteForm } from "./descriptor-form";
import {
  buildSchema,
  defaultValues,
  FIXED_NOTE,
  fixedSecretFields,
  formPath,
  toRemoteInput,
  type RemoteInputInit,
} from "./schema";

// Wrapper types (a WRAPPED field) and fixed fields, driven by the descriptor
// alone: the crypt fixture is the only type with them today.

const now = Date.parse("2026-03-12T12:00:00Z");
const backendMap = new Map(fx.backends.map((b) => [b.id, b]));
const crypt = fx.backend("crypt");

function remoteById(id: string): Remote {
  const r = fx.remotes(now).find((x) => x.id === id);
  if (!r) throw new Error(id);
  return r;
}

function violation(...v: { field: string; message: string }[]): ConnectError {
  return new ConnectError("invalid", Code.InvalidArgument, undefined, [
    { desc: ValidationErrorSchema, value: { violations: v } },
  ]);
}

describe("wrapper and fixed fields in the schema", () => {
  it("maps the wrapped violations to the Store in controls", () => {
    expect(formPath(crypt, "wrapped.remote_id")).toBe("wrapped.remoteId");
    expect(formPath(crypt, "wrapped")).toBe("wrapped.remoteId");
    expect(formPath(crypt, "wrapped.path")).toBe("wrapped.path");
    expect(formPath(fx.backend("s3"), "wrapped.path")).toBeUndefined();
    // The WRAPPED field is not an option.
    expect(formPath(crypt, "options.remote")).toBeUndefined();
  });

  it("loads where a stored remote keeps its data and sends it back", () => {
    const vault = remoteById(fx.ids.vault);
    const v = defaultValues(crypt, vault);
    expect(v.wrapped).toEqual({ remoteId: fx.ids.b2, path: "vault" });
    expect(v.values).not.toHaveProperty("remote");
    expect(
      toRemoteInput(crypt, { ...v, wrapped: { remoteId: fx.ids.b2, path: " other " } }, vault),
    ).toMatchObject({ wrapped: { remoteId: fx.ids.b2, path: "other" } });
    // Other types send no wrapped remote.
    expect(toRemoteInput(fx.backend("s3"), defaultValues(fx.backend("s3")))).not.toHaveProperty(
      "wrapped",
    );
  });

  it("requires the wrapped remote", () => {
    const v = defaultValues(crypt);
    const res = buildSchema(crypt).safeParse({
      ...v,
      name: "Vault",
      values: { ...v.values, password: "pw" },
    });
    expect(res.error?.issues.map((i) => i.path.join("."))).toEqual(["wrapped.remoteId"]);
  });

  it("never sends a fixed secret or a changed fixed option on update", () => {
    const stored = create(RemoteSchema, {
      ...remoteById(fx.ids.vault),
      options: { filename_encryption: "off", suffix: ".enc" },
    });
    const v = defaultValues(crypt, stored);
    v.values.password = "guess";
    v.values.password2 = "salt";
    v.values.filename_encryption = "standard";
    v.values.suffix = ".bin";
    v.values.directory_name_encryption = false;
    v.clearSecrets = ["password2"];
    const input = toRemoteInput(crypt, v, stored);
    expect(input.secrets).toEqual({});
    expect(input.clearSecrets).toEqual([]);
    // Fixed options go back exactly as stored; absent ones stay absent.
    expect(input.options).toEqual({ filename_encryption: "off", suffix: ".enc" });

    // The same values on create are sent as typed.
    const created = toRemoteInput(crypt, { ...v, clearSecrets: [] });
    expect(created.secrets).toEqual({ password: "guess", password2: "salt" });
    expect(created.options).toMatchObject({
      filename_encryption: "standard",
      directory_name_encryption: "false",
    });
  });

  it("does not check read-only fixed fields when editing", () => {
    const odd = create(BackendSchema, {
      ...crypt,
      fields: crypt.fields.map((f) =>
        f.name === "filename_encryption" ? { ...f, required: true } : f,
      ),
    });
    const v = { ...defaultValues(odd), name: "Vault", wrapped: { remoteId: fx.ids.b2, path: "" } };
    v.values.filename_encryption = "";
    expect(buildSchema(odd, { secretsSet: ["password"] }).safeParse(v).success).toBe(false);
    expect(buildSchema(odd, { secretsSet: ["password"], editing: true }).safeParse(v).success).toBe(
      true,
    );
  });

  it("finds the fixed secrets", () => {
    expect(fixedSecretFields(crypt).map((f) => f.name)).toEqual(["password", "password2"]);
    expect(fixedSecretFields(fx.backend("sftp"))).toEqual([]);
  });
});

describe("RemoteForm for a wrapper type", () => {
  it("creates a remote in a folder of another remote, after the password is saved", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn().mockResolvedValue(undefined);
    renderWithProviders(
      <RemoteForm
        backend={crypt}
        remotes={fx.remotes(now)}
        backends={backendMap}
        onSubmit={onSubmit}
      />,
    );

    // The warning (the password's help) sits above the password, once.
    const warning = screen.getByRole("note");
    expect(warning).toHaveTextContent(/Syncr never shows this password again/);
    expect(screen.getByLabelText("Password")).toHaveAttribute(
      "aria-describedby",
      expect.stringContaining(warning.id),
    );
    expect(screen.getAllByText(/Syncr never shows this password again/)).toHaveLength(1);

    // Create waits for the acknowledgement.
    const createButton = screen.getByRole("button", { name: "Create remote" });
    expect(createButton).toBeDisabled();

    // Only remotes that can hold the data are offered: not a worker's
    // folder, a union or another encrypted remote.
    const store = screen.getByRole("group", { name: "Store in" });
    await user.click(within(store).getByRole("combobox", { name: "Remote" }));
    const offered = screen.getAllByRole("option").map((o) => o.textContent);
    expect(offered).toEqual([
      expect.stringContaining("B2 offsite"),
      expect.stringContaining("MinIO archive"),
      expect.stringContaining("Nextcloud"),
      expect.stringContaining("Storage box"),
    ]);
    await user.click(screen.getByRole("option", { name: /B2 offsite/ }));
    expect(within(store).getByRole("button", { name: /^Browse/ })).toBeInTheDocument();
    await user.type(within(store).getByLabelText("Folder"), "vault");

    await user.type(screen.getByLabelText("Name"), "Vault");
    await user.type(screen.getByLabelText("Password"), "correct horse");
    const ack = screen.getByRole("checkbox", {
      name: "I have saved the password somewhere else",
    });
    await user.click(ack);
    expect(createButton).toBeEnabled();

    // A changed password needs the box ticked again.
    await user.type(screen.getByLabelText("Password"), "!");
    expect(ack).not.toBeChecked();
    expect(createButton).toBeDisabled();
    await user.click(ack);

    await user.click(createButton);
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
    expect(onSubmit.mock.calls[0][0]).toMatchObject({
      name: "Vault",
      backendType: "crypt",
      wrapped: { remoteId: fx.ids.b2, path: "vault" },
      secrets: { password: "correct horse!" },
      options: { filename_encryption: "standard" },
    });
    const sent = onSubmit.mock.calls[0][0] as RemoteInputInit;
    expect(sent.options).not.toHaveProperty("remote");
  });

  it("generates a 32-character URL-safe password and offers to copy it", async () => {
    const user = userEvent.setup();
    renderWithProviders(
      <RemoteForm
        backend={crypt}
        remotes={fx.remotes(now)}
        backends={backendMap}
        onSubmit={vi.fn()}
      />,
    );
    const pass = screen.getByLabelText("Password");
    const generate = within(pass.closest("[data-field]") as HTMLElement).getByRole("button", {
      name: "Generate",
    });
    const seen = new Set<string>();
    for (let i = 0; i < 20; i++) {
      await user.click(generate);
      const value = (pass as HTMLInputElement).value;
      expect(value).toHaveLength(32);
      for (const c of value) expect(URL_SAFE_ALPHABET).toContain(c);
      seen.add(value);
    }
    expect(seen.size).toBe(20);
    // Shown in clear, with a copy button.
    expect(pass).toHaveAttribute("type", "text");
    expect(screen.getByRole("button", { name: "Copy password" })).toBeInTheDocument();
    // Typing over it removes the copy button.
    await user.type(pass, "x");
    expect(screen.queryByRole("button", { name: "Copy password" })).not.toBeInTheDocument();
  });

  it("shows fixed fields read-only when editing and sends none of them changed", async () => {
    const user = userEvent.setup();
    const onSubmit = vi
      .fn()
      .mockRejectedValueOnce(
        violation(
          { field: "wrapped.remote_id", message: "is not one of your remotes" },
          { field: "wrapped.path", message: 'path must not contain ".." segments' },
        ),
      )
      .mockResolvedValue(undefined);
    const vault = create(RemoteSchema, {
      ...remoteById(fx.ids.vault),
      options: { filename_encryption: "obfuscate" },
    });
    renderWithProviders(
      <RemoteForm
        backend={crypt}
        remote={vault}
        remotes={fx.remotes(now)}
        backends={backendMap}
        onSubmit={onSubmit}
      />,
    );

    // No warning, generator or acknowledgement when editing.
    expect(screen.queryByRole("note")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Generate" })).not.toBeInTheDocument();
    expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();

    const pass = screen.getByLabelText("Password");
    expect(pass).toHaveAttribute("readonly");
    expect(pass).toHaveValue("Set");
    expect(screen.getByLabelText("File name encryption")).toHaveValue(
      "Obfuscate: hide the names, keep their length",
    );
    expect(screen.getByLabelText("Encrypt folder names")).toHaveValue("On");
    // One note says why, above the locked fields (the advanced ones are collapsed).
    const note = screen.getByText(FIXED_NOTE);
    expect(screen.getAllByText(FIXED_NOTE)).toHaveLength(1);
    expect(pass).toHaveAttribute("aria-describedby", note.parentElement?.id);

    // The vault itself is not offered as its own store.
    const store = screen.getByRole("group", { name: "Store in" });
    expect(within(store).getByDisplayValue("vault")).toBeInTheDocument();
    await user.click(within(store).getByRole("combobox", { name: "Remote" }));
    expect(screen.queryByRole("option", { name: /Vault/ })).not.toBeInTheDocument();
    await user.keyboard("{Escape}");

    await user.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByText("is not one of your remotes")).toBeInTheDocument();
    expect(screen.getByText('path must not contain ".." segments')).toBeInTheDocument();
    expect(within(store).getByLabelText("Folder")).toHaveAttribute("aria-invalid", "true");

    await user.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(2));
    const input = onSubmit.mock.calls[1][0] as RemoteInputInit;
    expect(input).toMatchObject({
      name: "Vault",
      wrapped: { remoteId: fx.ids.b2, path: "vault" },
      secrets: {},
      clearSecrets: [],
      options: { filename_encryption: "obfuscate" },
    });
    expect(Object.keys(input.options ?? {})).toEqual(["filename_encryption"]);
  });
});
