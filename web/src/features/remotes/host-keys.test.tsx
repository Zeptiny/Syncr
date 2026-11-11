import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import type { GetSftpHostKeysRequest } from "@/gen/syncr/v1/probe_pb";
import type { CreateRemoteRequest } from "@/gen/syncr/v1/remotes_pb";
import * as m5 from "@/mock/m5";
import * as m6 from "@/mock/m6";
import * as fx from "@/test/fixtures";
import { m5Api } from "@/test/m5-api";
import { renderApp } from "@/test/render";

import { hasHostKeys, hostKeysValue, isUnverified, sameKeys } from "./host-keys";

const now = Date.now();
const lines = hostKeysValue(m6.hostKeys);

describe("host key helpers", () => {
  it("fills one algorithm and key per line", () => {
    expect(lines.split("\n")).toEqual([
      `ssh-ed25519 ${m6.hostKeys[0].key}`,
      `ecdsa-sha2-nistp256 ${m6.hostKeys[1].key}`,
    ]);
  });

  it("knows when a remote has no host keys", () => {
    const storageBox = fx.remotes(now).find((r) => r.id === fx.ids.storageBox)!;
    expect(isUnverified(storageBox)).toBe(true);
    expect(isUnverified(m6.verifiedRemote(now))).toBe(false);
    expect(isUnverified({ backendType: "sftp", options: { host_keys: " \n , " } })).toBe(true);
    // Only SSH servers can be verified this way.
    expect(isUnverified({ backendType: "s3", options: {} })).toBe(false);
    expect(hasHostKeys("ssh-ed25519 AAAA")).toBe(true);
  });

  it("compares keys whatever the separators and spacing", () => {
    const typed = `  ssh-ed25519   ${m6.hostKeys[0].key}\r\n,ecdsa-sha2-nistp256 ${m6.hostKeys[1].key}\n`;
    expect(sameKeys(typed.replaceAll("\r", ""), m6.hostKeys)).toBe(true);
    expect(sameKeys(`ssh-ed25519 ${m6.hostKeys[0].key}`, m6.hostKeys)).toBe(false);
    expect(sameKeys("", [])).toBe(false);
  });
});

/** sftpForm opens a new SFTP remote with its host filled in. */
async function sftpForm(host: string, port = "") {
  const user = userEvent.setup();
  const api = m5Api(now);
  renderApp("/remotes/new?type=sftp", api.transport);
  await user.type(await screen.findByLabelText("Name"), "NAS over SSH");
  await user.type(screen.getByLabelText("Host"), host);
  if (port) {
    await user.clear(screen.getByLabelText("Port"));
    await user.type(screen.getByLabelText("Port"), port);
  }
  await user.type(screen.getByLabelText("User"), "backup");
  return { user, api };
}

describe("SFTP host keys in the remote form", { timeout: 30_000 }, () => {
  it("fetches the keys, shows their fingerprints and fills the field", async () => {
    const { user, api } = await sftpForm("nas.example.net", "2222");
    const keysField = screen.getByRole("textbox", { name: /^Server host keys/ });
    expect(screen.getByTestId("server-not-verified")).toHaveTextContent("Server not verified");

    await user.click(screen.getByRole("button", { name: "Fetch from server" }));
    const result = await screen.findByTestId("host-keys-result");
    expect(result).toHaveTextContent("The server offered 2 keys (through nas-01)");
    const list = within(result).getByRole("list", { name: "Fetched host keys" });
    expect(list).toHaveTextContent("ssh-ed25519");
    expect(list).toHaveTextContent("SHA256:dkA939jgBPkIxhIk1YrFbVK6S3sjJRKuDnTe/EF7P54");
    expect(list).toHaveTextContent("ecdsa-sha2-nistp256");
    expect(result).toHaveTextContent("ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub");
    expect(api.of<GetSftpHostKeysRequest>("GetSftpHostKeys")).toEqual([
      expect.objectContaining({ host: "nas.example.net", port: 2222, workerId: "" }),
    ]);
    // The field is untouched until the user takes the keys.
    expect(keysField).toHaveValue("");

    await user.click(within(result).getByRole("button", { name: "Use these keys" }));
    expect(keysField).toHaveValue(lines);
    expect(screen.queryByTestId("server-not-verified")).not.toBeInTheDocument();
    expect(within(result).getByRole("button", { name: "Use these keys" })).toBeDisabled();
    expect(result).toHaveTextContent("Save to pin them");

    await user.type(screen.getByLabelText(/^Password/), "s3cret-pass");
    await user.click(screen.getByRole("button", { name: "Create remote" }));
    // The test API answers CreateRemote with UNIMPLEMENTED; what counts is
    // that the request carried the keys as filled in.
    await waitFor(() => expect(api.of("CreateRemote")).toHaveLength(1));
    expect(api.of<CreateRemoteRequest>("CreateRemote")[0].input?.options.host_keys).toBe(lines);
  });

  it("uses port 22 when the port is the default", async () => {
    const { user, api } = await sftpForm("nas.example.net");
    await user.click(screen.getByRole("button", { name: "Fetch from server" }));
    await screen.findByTestId("host-keys-result");
    expect(api.of<GetSftpHostKeysRequest>("GetSftpHostKeys")[0].port).toBe(22);
  });

  it("asks for the host before fetching", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    renderApp("/remotes/new?type=sftp", api.transport);
    await user.click(await screen.findByRole("button", { name: "Fetch from server" }));
    expect(await screen.findByTestId("host-keys-error")).toHaveTextContent(
      "Check the host and port",
    );
    expect(screen.getByText("Host is required.")).toBeVisible();
    expect(api.of("GetSftpHostKeys")).toHaveLength(0);
  });

  it("shows the API's invalid host on the Host field", async () => {
    const { user } = await sftpForm("-oProxyCommand=evil");
    await user.click(screen.getByRole("button", { name: "Fetch from server" }));
    const error = await screen.findByTestId("host-keys-error");
    expect(error).toHaveTextContent("Check the host and port");
    expect(error).toHaveTextContent("Host: is not a valid host name or address");
    expect(screen.getByLabelText("Host")).toHaveAttribute("aria-invalid", "true");
  });

  it("explains an unreachable server and offers another worker", async () => {
    const { user, api } = await sftpForm("nas.invalid");
    await user.click(screen.getByRole("button", { name: "Fetch from server" }));
    const error = await screen.findByTestId("host-keys-error");
    expect(error).toHaveTextContent("Could not read the server's keys");
    expect(error).toHaveTextContent("no such host");
    expect(error).toHaveTextContent("try through another worker");
    // FAILED_PRECONDITION: the worker picker appears.
    await user.click(screen.getByRole("combobox", { name: "Fetch through" }));
    await user.click(await screen.findByRole("option", { name: /old-nas/ }));
    await user.click(screen.getByRole("button", { name: "Fetch from server" }));
    await waitFor(() =>
      expect(screen.getByTestId("host-keys-error")).toHaveTextContent("Worker old-nas is outdated"),
    );
    expect(api.of<GetSftpHostKeysRequest>("GetSftpHostKeys").at(-1)?.workerId).toBe(m5.ids.oldNas);
  });

  it("warns about an unverified server in the edit form, and does not block saving", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    renderApp(`/remotes/${fx.ids.storageBox}`, api.transport);
    expect(await screen.findByTestId("server-not-verified")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(api.of("UpdateRemote")).toHaveLength(1));
  });
});

describe("unverified servers in the remote list", { timeout: 30_000 }, () => {
  it("marks only the SFTP remote without host keys", async () => {
    const api = m5Api(now);
    renderApp("/remotes", api.transport);
    const row = await screen.findByRole("row", { name: /Storage box/ });
    expect(row).toHaveTextContent("Server not verified");
    expect(screen.getAllByText("Server not verified")).toHaveLength(1);
    expect(screen.getByRole("row", { name: /B2 offsite/ })).not.toHaveTextContent(
      "Server not verified",
    );
  });
});
