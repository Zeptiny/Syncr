import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { ValidationErrorSchema } from "@/gen/syncr/v1/api_pb";
import { UnionUpstreamMode } from "@/gen/syncr/v1/link_pb";
import {
  BackendFieldSchema,
  BackendSchema,
  RemoteSchema,
  type Remote,
} from "@/gen/syncr/v1/remotes_pb";
import * as fx from "@/test/fixtures";
import { renderWithProviders } from "@/test/render";

import { DescriptorForm } from "./descriptor-form";
import { buildSchema, defaultValues, formPath, toRemoteInput } from "./schema";

const now = Date.parse("2026-03-12T12:00:00Z");
const backendMap = new Map(fx.backends.map((b) => [b.id, b]));

function violation(...v: { field: string; message: string }[]): ConnectError {
  return new ConnectError("invalid", Code.InvalidArgument, undefined, [
    { desc: ValidationErrorSchema, value: { violations: v } },
  ]);
}

function remoteById(id: string): Remote {
  const r = fx.remotes(now).find((x) => x.id === id);
  if (!r) throw new Error(id);
  return r;
}

describe("DescriptorForm schema", () => {
  it("requires fields from the descriptor and keeps stored secrets", () => {
    const s3 = fx.backend("s3");
    const create = buildSchema(s3);
    const v = defaultValues(s3);
    const res = create.safeParse({ ...v, name: "x" });
    expect(res.success).toBe(false);
    const paths = res.error?.issues.map((i) => i.path.join(".")) ?? [];
    expect(paths).toContain("values.access_key_id");
    expect(paths).toContain("values.secret_access_key");

    const edit = buildSchema(s3, { secretsSet: ["secret_access_key"] });
    const ok = edit.safeParse({ ...v, name: "x", values: { ...v.values, access_key_id: "k" } });
    expect(ok.success).toBe(true);
  });

  it("parses ints and enums", () => {
    const sftp = fx.backend("sftp");
    const v = defaultValues(sftp);
    const res = buildSchema(sftp).safeParse({
      ...v,
      name: "n",
      values: { ...v.values, host: "h", user: "u", port: "twenty", shell_type: "bash" },
    });
    const paths = res.error?.issues.map((i) => i.path.join(".")) ?? [];
    expect(paths).toEqual(expect.arrayContaining(["values.port", "values.shell_type"]));
  });

  it("converts form values to RemoteInput", () => {
    const s3 = fx.backend("s3");
    const v = defaultValues(s3);
    v.name = "  Archive ";
    v.values.access_key_id = " AK ";
    v.values.secret_access_key = "s3cret";
    v.values.no_check_bucket = true; // default false → sent
    v.values.force_path_style = true; // equals default "true" → omitted
    const input = toRemoteInput(s3, v);
    expect(input).toMatchObject({
      name: "Archive",
      backendType: "s3",
      options: { provider: "AWS", access_key_id: "AK", no_check_bucket: "true" },
      secrets: { secret_access_key: "s3cret" },
      clearSecrets: [],
      members: [],
    });
    expect(input.options).not.toHaveProperty("force_path_style");
    expect(input.options).not.toHaveProperty("secret_access_key");
  });

  it("falls back to descriptor defaults for options the remote lacks", () => {
    const union = fx.backend("union");
    const remote = create(RemoteSchema, { ...remoteById(fx.ids.pooled), options: {} });
    expect(defaultValues(union, remote).values).toEqual({
      action_policy: "epall",
      create_policy: "epmfs",
      search_policy: "ff",
      cache_time: "120",
    });

    // An enum default outside the choices would be sent unseen, so it starts empty.
    const odd = create(BackendSchema, {
      ...union,
      fields: union.fields.map((f) =>
        f.name === "search_policy" ? create(BackendFieldSchema, { ...f, defaultValue: "gone" }) : f,
      ),
    });
    expect(defaultValues(odd).values.search_policy).toBe("");
    expect(defaultValues(odd, remote).values.search_policy).toBe("");
    expect(defaultValues(odd, remoteById(fx.ids.pooled)).values.search_policy).toBe("ff");
  });

  it.each([
    ["name", "name"],
    ["options.endpoint", "values.endpoint"],
    ["secrets.secret_access_key", "values.secret_access_key"],
    ["options.nope", undefined],
    ["members[1].path", "members.1.path"],
    ["members[0].remote_id", "members.0.remoteId"],
    ["members", "members.root"],
  ])("maps violation %s to %s", (field, want) => {
    const b = field.startsWith("members") ? fx.backend("union") : fx.backend("s3");
    expect(formPath(b, field)).toBe(want);
  });
});

describe("DescriptorForm", () => {
  it("creates an s3 remote from the descriptor", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn().mockResolvedValue(undefined);
    renderWithProviders(<DescriptorForm backend={fx.backend("s3")} onSubmit={onSubmit} />);

    await user.click(screen.getByRole("button", { name: "Create remote" }));
    expect(await screen.findByText("Give the remote a name.")).toBeInTheDocument();
    expect(screen.getByText("Access key ID is required.")).toBeInTheDocument();
    expect(screen.getByText("Secret access key is required.")).toBeInTheDocument();
    expect(onSubmit).not.toHaveBeenCalled();

    // Advanced fields start collapsed.
    expect(screen.queryByLabelText(/Storage class/)).not.toBeInTheDocument();

    await user.type(screen.getByLabelText("Name"), "Archive");
    await user.type(screen.getByLabelText("Access key ID"), "AKIA1");
    await user.type(screen.getByLabelText("Secret access key"), "s3cret");
    await user.click(screen.getByRole("button", { name: "Create remote" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
    expect(onSubmit.mock.calls[0][0]).toMatchObject({
      name: "Archive",
      backendType: "s3",
      options: { provider: "AWS", access_key_id: "AKIA1" },
      secrets: { secret_access_key: "s3cret" },
    });
  });

  it("keeps a stored secret when left empty and clears it on request", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn().mockResolvedValue(undefined);
    const remote = remoteById(fx.ids.storageBox);
    const { unmount } = renderWithProviders(
      <DescriptorForm backend={fx.backend("sftp")} remote={remote} onSubmit={onSubmit} />,
    );
    const pass = screen.getByLabelText(/^Password/);
    expect(pass).toHaveValue("");
    expect(pass).toHaveAttribute("placeholder", "Set. Leave empty to keep it.");
    expect(screen.getByText("Saved")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
    expect(onSubmit.mock.calls[0][0]).toMatchObject({
      name: "Storage box",
      options: { host: "u123456.your-storagebox.de", port: "23", user: "u123456", path: "backups" },
      secrets: {},
      clearSecrets: [],
    });

    await user.click(screen.getByRole("button", { name: "Clear Password" }));
    expect(screen.getByText("Removed when you save.")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(2));
    expect(onSubmit.mock.calls[1][0]).toMatchObject({ secrets: {}, clearSecrets: ["pass"] });
    unmount();
  });

  it("replaces a stored secret when a new value is typed", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn().mockResolvedValue(undefined);
    renderWithProviders(
      <DescriptorForm
        backend={fx.backend("sftp")}
        remote={remoteById(fx.ids.storageBox)}
        onSubmit={onSubmit}
      />,
    );
    await user.type(screen.getByLabelText(/^Password/), "n3w");
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalled());
    expect(onSubmit.mock.calls[0][0]).toMatchObject({ secrets: { pass: "n3w" }, clearSecrets: [] });
  });

  it("edits union members and requires two", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn().mockResolvedValue(undefined);
    renderWithProviders(
      <DescriptorForm
        backend={fx.backend("union")}
        remote={remoteById(fx.ids.pooled)}
        remotes={fx.remotes(now)}
        backends={backendMap}
        onSubmit={onSubmit}
      />,
    );
    const members = screen.getByRole("group", { name: "Members" });
    expect(within(members).getAllByRole("listitem")).toHaveLength(2);
    expect(within(members).getByDisplayValue("library")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
    expect(onSubmit.mock.calls[0][0]).toMatchObject({
      backendType: "union",
      options: { action_policy: "epall", create_policy: "epmfs", search_policy: "ff" },
      members: [
        { remoteId: fx.ids.photos, path: "", mode: UnionUpstreamMode.READ_WRITE },
        { remoteId: fx.ids.media, path: "library", mode: UnionUpstreamMode.READ_ONLY },
      ],
    });

    await user.click(screen.getByRole("button", { name: "Remove member 2" }));
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByText("A union needs at least two members.")).toBeInTheDocument();
    expect(onSubmit).toHaveBeenCalledTimes(1);
  });

  it("shows and submits enum defaults when editing a remote that lacks them", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn().mockResolvedValue(undefined);
    const remote = create(RemoteSchema, { ...remoteById(fx.ids.pooled), options: {} });
    renderWithProviders(
      <DescriptorForm
        backend={fx.backend("union")}
        remote={remote}
        remotes={fx.remotes(now)}
        backends={backendMap}
        onSubmit={onSubmit}
      />,
    );
    expect(screen.getByLabelText("Action policy")).toHaveTextContent("epall");
    expect(screen.getByLabelText("Create policy")).toHaveTextContent("epmfs");
    expect(screen.getByLabelText("Search policy")).toHaveTextContent("ff");
    expect(screen.queryByText("Choose…")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
    expect(onSubmit.mock.calls[0][0]).toMatchObject({
      options: {
        action_policy: "epall",
        create_policy: "epmfs",
        search_policy: "ff",
        cache_time: "120",
      },
    });
  });

  it("shows server field violations on their fields", async () => {
    const user = userEvent.setup();
    const onSubmit = vi
      .fn()
      .mockRejectedValueOnce(
        violation(
          { field: "options.endpoint", message: "Required unless the provider is AWS." },
          { field: "options.storage_class", message: "Unknown storage class." },
        ),
      )
      .mockRejectedValueOnce(violation({ field: "backend_type", message: "unknown backend" }))
      .mockRejectedValueOnce(new ConnectError("name taken", Code.AlreadyExists));
    const remote = create(RemoteSchema, {
      ...remoteById(fx.ids.minio),
      options: { provider: "Minio", access_key_id: "k" },
    });
    renderWithProviders(
      <DescriptorForm backend={fx.backend("s3")} remote={remote} onSubmit={onSubmit} />,
    );

    await user.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByText("Required unless the provider is AWS.")).toBeInTheDocument();
    // The advanced section opens to show its error.
    expect(screen.getByText("Unknown storage class.")).toBeInTheDocument();
    expect(screen.getByLabelText(/Storage class/)).toHaveAttribute("aria-invalid", "true");

    await user.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByText("backend_type: unknown backend")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByText("Name taken")).toBeInTheDocument();
  });
});
