import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { RemoteForm } from "@/components/app/descriptor-form";
import type { RemoteInputInit } from "@/components/app/descriptor-form/schema";
import * as fx from "@/test/fixtures";
import { renderWithProviders } from "@/test/render";

import { backendIcon } from "./backend-icon";

describe("backend icons", () => {
  it("gives every known type its own glyph", () => {
    const glyphs = fx.backends.map((b) => backendIcon(b.id));
    expect(glyphs).not.toContain(backendIcon("gopher"));
    expect(new Set(glyphs).size).toBe(glyphs.length);
  });

  it.each(["smb", "ftp", "azureblob", "gcs"])("has a glyph for %s", (id) => {
    expect(backendIcon(id)).not.toBe(backendIcon("not-a-type"));
  });
});

function renderForm(id: string) {
  const onSubmit = vi
    .fn<(input: RemoteInputInit) => Promise<unknown>>()
    .mockResolvedValue(undefined);
  renderWithProviders(<RemoteForm backend={fx.backend(id)} onSubmit={onSubmit} />);
  return onSubmit;
}

describe("forms of the remote types", () => {
  it("creates an SMB remote, with case-insensitive names on unless turned off", async () => {
    const user = userEvent.setup();
    const onSubmit = renderForm("smb");
    expect(screen.queryByLabelText(/^Domain/)).not.toBeInTheDocument();

    await user.type(screen.getByLabelText("Name"), "NAS share");
    await user.type(screen.getByLabelText("Host"), "nas.local");
    await user.type(screen.getByLabelText("User"), "guest");
    await user.type(screen.getByLabelText(/^Share/), "family/photos");
    await user.click(screen.getByRole("button", { name: "Create remote" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
    expect(onSubmit.mock.calls[0][0]).toMatchObject({
      backendType: "smb",
      options: { host: "nas.local", user: "guest", share: "family/photos" },
      secrets: {},
    });
    expect(onSubmit.mock.calls[0][0].options).not.toHaveProperty("case_insensitive");

    await user.click(screen.getByRole("button", { name: /Advanced options/ }));
    expect(screen.getByLabelText(/^Domain/)).toHaveValue("WORKGROUP");
    const caseless = screen.getByRole("switch", { name: /Case-insensitive names/ });
    expect(caseless).toBeChecked();
    await user.click(caseless);
    await user.click(screen.getByRole("button", { name: "Create remote" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(2));
    expect(onSubmit.mock.calls[1][0].options).toMatchObject({ case_insensitive: "false" });
  });

  it("creates an anonymous FTP remote with explicit TLS", async () => {
    const user = userEvent.setup();
    const onSubmit = renderForm("ftp");
    await user.click(screen.getByRole("button", { name: "Create remote" }));
    expect(await screen.findByText("Host is required.")).toBeInTheDocument();
    expect(screen.getByText("User is required.")).toBeInTheDocument();
    expect(screen.getByText(/the password crosses the network in clear/)).toBeInTheDocument();

    await user.type(screen.getByLabelText("Name"), "Mirror");
    await user.type(screen.getByLabelText("Host"), "ftp.example.org");
    await user.type(screen.getByLabelText("User"), "anonymous");
    await user.click(screen.getByRole("switch", { name: /Explicit FTPS/ }));
    await user.click(screen.getByRole("button", { name: "Create remote" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
    expect(onSubmit.mock.calls[0][0]).toMatchObject({
      backendType: "ftp",
      options: { host: "ftp.example.org", user: "anonymous", explicit_tls: "true" },
      secrets: {},
    });
  });

  it("shows the Azure sign-in choices, the service principal under advanced", async () => {
    const user = userEvent.setup();
    renderForm("azureblob");
    for (const label of [/^Storage account/, /^Account key/, /^SAS URL/, /^Container/])
      expect(screen.getByLabelText(label)).toBeInTheDocument();
    expect(screen.queryByLabelText(/^Tenant ID/)).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /Advanced options/ }));
    for (const label of [/^Tenant ID/, /^Client ID/, /^Client secret/, /^Endpoint/])
      expect(screen.getByLabelText(label)).toBeInTheDocument();
  });

  it("takes the GCS key as multi-line text and sends it as typed", async () => {
    const user = userEvent.setup();
    const onSubmit = renderForm("gcs");
    const key = screen.getByLabelText(/^Service account key \(JSON\)/);
    expect(key.tagName).toBe("TEXTAREA");

    await user.click(screen.getByRole("button", { name: "Create remote" }));
    expect(await screen.findByText("Service account key (JSON) is required.")).toBeInTheDocument();

    await user.type(screen.getByLabelText("Name"), "GCS backups");
    const json = '{{\n  "type": "service_account"\n}';
    await user.type(key, json);
    await user.type(screen.getByLabelText(/^Bucket/), "backups");
    await user.click(screen.getByRole("button", { name: "Create remote" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
    expect(onSubmit.mock.calls[0][0]).toMatchObject({
      backendType: "gcs",
      options: { bucket: "backups" },
      secrets: { service_account_credentials: '{\n  "type": "service_account"\n}' },
    });
  });

  it("lists the S3 providers, Other last", async () => {
    const user = userEvent.setup();
    renderForm("s3");
    await user.click(screen.getByRole("combobox", { name: "Provider" }));
    const offered = screen.getAllByRole("option").map((o) => o.textContent);
    expect(offered).toEqual(
      expect.arrayContaining([
        "Hetzner Object Storage",
        "IDrive e2",
        "Scaleway",
        "OVHcloud",
        "Akamai / Linode",
        "IONOS Cloud",
        "Synology C2",
        "Storj S3 gateway",
        "Alibaba Cloud OSS",
        "IBM Cloud Object Storage",
        "SeaweedFS",
        "MEGA S4",
      ]),
    );
    expect(offered).toHaveLength(19);
    expect(offered.at(-1)).toBe("Other S3-compatible");
  });
});
