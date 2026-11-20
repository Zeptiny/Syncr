import { create } from "@bufbuild/protobuf";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it } from "vitest";

import { FieldViolationSchema } from "@/gen/syncr/v1/api_pb";
import { BackendSchema, RemoteSchema } from "@/gen/syncr/v1/remotes_pb";
import * as fx from "@/test/fixtures";
import { renderWithProviders } from "@/test/render";

import { emptyBisync, type BisyncState } from "./bisync-definition";
import { BisyncEditor } from "./bisync-editor";
import { noChecksumLookup, noChecksumNames } from "./checksums";
import {
  definitionError,
  emptyTimeLimit,
  emptyTransfer,
  validateDefinition,
  type DefinitionErrors,
  type TransferState,
} from "./definition";
import { emptyHooks } from "./hooks";
import { TransferEditor } from "./transfer-editor";

const now = Date.parse("2026-03-12T12:00:00Z");
const remotes = fx.remotes(now);
const backends = new Map(fx.backends.map((b) => [b.id, b]));
const lookup = noChecksumLookup(remotes, backends);

// Another type that keeps no checksums, with another ID: the editors look
// at the capability only.
const ftpBackend = create(BackendSchema, {
  id: "ftp",
  label: "FTP",
  capabilities: { unionMember: true, noChecksum: true },
});
const ftp = create(RemoteSchema, { id: "ftp-1", name: "Old FTP", backendType: "ftp" });

function TransferHarness({
  initial,
  errors = {},
  extra = false,
}: {
  initial: TransferState;
  errors?: DefinitionErrors;
  extra?: boolean;
}) {
  const [s, setS] = useState(initial);
  return (
    <TransferEditor
      value={s}
      onChange={setS}
      errors={errors}
      remotes={extra ? [...remotes, ftp] : remotes}
      backends={extra ? new Map([...backends, ["ftp", ftpBackend]]) : backends}
    />
  );
}

function BisyncHarness({ initial }: { initial: BisyncState }) {
  const [s, setS] = useState(initial);
  return (
    <BisyncEditor value={s} onChange={setS} errors={{}} remotes={remotes} backends={backends} />
  );
}

function transfer(src: string, dst: string, compare: TransferState["compare"] = "default") {
  return {
    ...emptyTransfer(),
    source: { remoteId: src, path: "docs" },
    destination: { remoteId: dst, path: "docs" },
    compare,
  };
}

describe("checksums and remotes that keep none", () => {
  it("finds the remotes by capability", () => {
    expect(noChecksumNames(lookup, [fx.ids.vault, "", fx.ids.b2, fx.ids.vault])).toEqual(["Vault"]);
    expect(noChecksumNames(lookup, [fx.ids.minio, fx.ids.b2])).toEqual([]);
    expect(noChecksumNames(undefined, [fx.ids.vault])).toEqual([]);
  });

  it("refuses checksums with such a side before sending", () => {
    const common = { timeLimit: emptyTimeLimit(), hooks: emptyHooks() };
    const def = {
      kind: "rclone_transfer" as const,
      transfer: transfer("", fx.ids.vault, "checksum"),
      ...common,
    };
    def.transfer.source.path = "/srv/docs";
    expect(validateDefinition(def, lookup)).toEqual({
      compare:
        "Checksum comparison is not available: Vault keeps no checksums. Choose another way to compare.",
    });
    expect(validateDefinition(def)).toEqual({});

    const bisync = {
      ...emptyBisync(),
      path1: { remoteId: fx.ids.vault, path: "a" },
      path2: { remoteId: "", path: "/srv/a" },
      compare: { size: true, modtime: false, checksum: true },
    };
    expect(
      validateDefinition({ kind: "rclone_bisync", bisync, ...common }, lookup).compare,
    ).toMatch(/Vault keeps no checksums/);
  });

  it.each([
    ["definition.rclone_transfer.options.checksum"],
    ["definition.rclone_bisync.options.compare[1]"],
  ])("puts the server's refusal at %s on the comparison", (field) => {
    const errors: DefinitionErrors = {};
    const v = create(FieldViolationSchema, {
      field,
      message: "is not available: a remote that keeps no checksums",
    });
    expect(definitionError(v, errors)).toBe(true);
    expect(errors).toEqual({
      compare: "Checksum comparison is not available: a remote that keeps no checksums",
    });
  });

  it("hides checksum in a transfer with such a side, with a note", async () => {
    const user = userEvent.setup();
    renderWithProviders(<TransferHarness initial={transfer(fx.ids.vault, fx.ids.minio)} />);
    expect(
      screen.getByText(
        "This remote keeps no checksums (Vault), so checksum comparison is not offered.",
      ),
    ).toBeInTheDocument();
    await user.click(screen.getByRole("combobox", { name: "Decide what changed by" }));
    const options = screen.getAllByRole("option").map((o) => o.textContent);
    expect(options).toEqual(["Modification time and size", "Size only"]);
  });

  it("offers checksum between remotes that keep them", async () => {
    const user = userEvent.setup();
    renderWithProviders(<TransferHarness initial={transfer(fx.ids.minio, fx.ids.b2)} />);
    expect(screen.queryByText(/keeps no checksums/)).not.toBeInTheDocument();
    await user.click(screen.getByRole("combobox", { name: "Decide what changed by" }));
    expect(screen.getByRole("option", { name: "Checksum (slower, exact)" })).toBeInTheDocument();
  });

  it("keys on the capability, not on a type", () => {
    renderWithProviders(<TransferHarness initial={transfer("ftp-1", fx.ids.minio)} extra />);
    expect(screen.getByText(/This remote keeps no checksums \(Old FTP\)/)).toBeInTheDocument();
  });

  it("shows a stored checksum on such a side as an error until it is changed", async () => {
    const user = userEvent.setup();
    renderWithProviders(
      <TransferHarness initial={transfer(fx.ids.minio, fx.ids.vault, "checksum")} />,
    );
    const select = screen.getByRole("combobox", { name: "Decide what changed by" });
    expect(select).toHaveTextContent("Checksum (slower, exact)");
    expect(select).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByRole("alert")).toHaveTextContent(
      "Checksum comparison is not available: Vault keeps no checksums. Choose another way to compare.",
    );

    await user.click(select);
    await user.click(screen.getByRole("option", { name: "Size only" }));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByText(/This remote keeps no checksums \(Vault\)/)).toBeInTheDocument();
    await user.click(select);
    expect(screen.queryByRole("option", { name: /Checksum/ })).not.toBeInTheDocument();
  });

  it("shows the server's refusal on the comparison", () => {
    renderWithProviders(
      <TransferHarness
        initial={transfer(fx.ids.minio, fx.ids.b2)}
        errors={{
          compare: "Checksum comparison is not available: a remote that keeps no checksums",
        }}
      />,
    );
    const shell = screen
      .getByRole("combobox", { name: "Decide what changed by" })
      .closest("[data-field]") as HTMLElement;
    expect(within(shell).getByRole("alert")).toHaveTextContent(/keeps no checksums/);
  });

  it("hides the checksum box of a two-way sync with such a side", async () => {
    const user = userEvent.setup();
    const sides = {
      path1: { remoteId: fx.ids.vault, path: "shared" },
      path2: { remoteId: "", path: "/srv/shared" },
    };
    const { unmount } = renderWithProviders(
      <BisyncHarness initial={{ ...emptyBisync(), ...sides }} />,
    );
    expect(screen.queryByRole("checkbox", { name: "Checksum" })).not.toBeInTheDocument();
    expect(screen.getByText(/This remote keeps no checksums \(Vault\)/)).toBeInTheDocument();
    unmount();

    // A stored definition that compares checksums shows the box, with the error.
    renderWithProviders(
      <BisyncHarness
        initial={{
          ...emptyBisync(),
          ...sides,
          compare: { size: true, modtime: true, checksum: true },
        }}
      />,
    );
    const box = screen.getByRole("checkbox", { name: "Checksum" });
    expect(box).toBeChecked();
    expect(screen.getByRole("alert")).toHaveTextContent(/Vault keeps no checksums/);
    await user.click(box);
    expect(screen.queryByRole("checkbox", { name: "Checksum" })).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});
