import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { ProbeEntry } from "@/gen/syncr/v1/link_pb";
import * as m5 from "@/mock/m5";
import * as fx from "@/test/fixtures";
import { m5Api } from "@/test/m5-api";
import { renderWithProviders } from "@/test/render";

import { FileBrowser, type SelectionContext } from "../file-browser";
import { eligibleWorkers } from "../helpers";
import { emptySelection } from "../selection";

const now = Date.UTC(2026, 10, 10, 12);

function Harness({
  onCtx,
  folderSize,
}: {
  onCtx: (c: SelectionContext) => void;
  folderSize?: (e: ProbeEntry) => bigint | undefined;
}) {
  const [path, setPath] = useState("/srv/docs");
  const [selection, setSelection] = useState(emptySelection);
  return (
    <FileBrowser
      source={{ kind: "worker", workerId: fx.ids.nas }}
      path={path}
      onNavigate={setPath}
      selection={selection}
      onSelectionChange={setSelection}
      rootLabel="Allowed folders"
      folderSize={folderSize}
      renderFolderSize={(e) => (folderSize?.(e) === undefined ? undefined : "measured")}
      selectionActions={(ctx) => {
        onCtx(ctx);
        return <button type="button">Do it ({ctx.items.length})</button>;
      }}
    />
  );
}

const width = window.innerWidth;
afterEach(() => {
  window.innerWidth = width;
});

describe("FileBrowser", () => {
  it("hands the page's selection actions what they act on", async () => {
    const user = userEvent.setup();
    const onCtx = vi.fn<(c: SelectionContext) => void>();
    const sizes: Record<string, bigint> = { "/srv/docs/invoices": 1000n };
    renderWithProviders(<Harness onCtx={onCtx} folderSize={(e) => sizes[e.path]} />, {
      transport: m5Api(now).transport,
    });
    await screen.findByText("notes.md");
    expect(screen.getByText(/Tick files and folders to select them/)).toBeInTheDocument();
    // The size slot fills measured folders only.
    const invoices = screen.getByRole("row", { name: /invoices/ });
    expect(invoices.querySelector("td:nth-child(3)")).toHaveTextContent("measured");

    await user.click(screen.getByRole("checkbox", { name: "Select invoices" }));
    await user.click(screen.getByRole("checkbox", { name: "Select notes.md" }));
    const tray = screen.getByRole("region", { name: "Selection" });
    expect(within(tray).getByRole("button", { name: "Do it (2)" })).toBeInTheDocument();
    const ctx = onCtx.mock.lastCall![0];
    expect(ctx.items.map((i) => i.path)).toEqual(["/srv/docs/invoices", "/srv/docs/notes.md"]);
    expect(ctx.parent).toBe("/srv/docs");
    expect(ctx.size).toEqual({ bytes: 13_400n, atLeast: false, files: 1, folders: 1 });

    // An unmeasured folder makes the total a lower bound.
    await user.click(screen.getByRole("checkbox", { name: "Select taxes" }));
    expect(within(tray).getByText("at least 13.1 KiB")).toBeInTheDocument();
  });

  it("becomes a bottom bar with a sheet at phone width", async () => {
    window.innerWidth = 390;
    const user = userEvent.setup();
    renderWithProviders(<Harness onCtx={() => {}} />, { transport: m5Api(now).transport });
    await user.click(await screen.findByRole("checkbox", { name: "Select notes.md" }));
    const bar = screen.getByRole("region", { name: "Selection" });
    expect(within(bar).getByText("1 selected")).toBeInTheDocument();
    expect(within(bar).queryByRole("button", { name: "Copy paths" })).toBeNull();
    await user.click(within(bar).getByRole("button", { name: "Show selection" }));
    const sheet = await screen.findByRole("dialog", { name: "1 selected" });
    expect(within(sheet).getByRole("button", { name: "Do it (1)" })).toBeInTheDocument();
    await user.click(within(sheet).getByRole("button", { name: "Remove notes.md" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByRole("region", { name: "Selection" })).toBeNull();
  });
});

describe("eligibleWorkers", () => {
  it("offers online workers that can do the listing, by name", () => {
    const workers = m5.workers(now);
    expect(eligibleWorkers(workers, "list_remote").map((w) => w.name)).toEqual([
      "backup-host",
      "nas-01",
    ]);
    expect(eligibleWorkers(workers, "measure_remote")).toEqual([]);
  });
});
