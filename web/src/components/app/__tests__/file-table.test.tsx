import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { act, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ValidationErrorSchema } from "@/gen/syncr/v1/api_pb";
import { ProbeEntrySchema, ProbeEntryType, type ProbeEntry } from "@/gen/syncr/v1/link_pb";
import { renderWithProviders } from "@/test/render";

import { FileTable, type FileTableProps } from "../file-table";

const entry = (name: string, type: ProbeEntryType, size: number, ms: number): ProbeEntry =>
  create(ProbeEntrySchema, {
    name,
    path: `/srv/${name}`,
    type,
    size: BigInt(size),
    modTime: timestampFromMs(ms),
  });
const files = [
  entry("photos", ProbeEntryType.DIR, 0, 1),
  entry("docs", ProbeEntryType.DIR, 0, 2),
  entry("b.txt", ProbeEntryType.FILE, 300, 30),
  entry("a.txt", ProbeEntryType.FILE, 100, 10),
  entry("c.log", ProbeEntryType.FILE, 200, 20),
];

function Harness(props: Partial<FileTableProps> & { initial?: string[] }) {
  const [selected, setSelected] = useState<string[]>(props.initial ?? []);
  return (
    <>
      <FileTable
        entries={files}
        selection={{ selected, onChange: setSelected, multiple: true }}
        aria-label="Files here"
        {...props}
      />
      <output data-testid="selected">{selected.join(",")}</output>
    </>
  );
}

function show(props: Partial<FileTableProps> & { initial?: string[] } = {}) {
  renderWithProviders(<Harness {...props} />);
  return userEvent.setup();
}

const grid = () => screen.getByRole("grid", { name: "Files here" });
const bodyRows = () => within(grid()).getAllByRole("row").slice(1);
const rowNames = () => bodyRows().map((r) => r.querySelector("td:nth-child(2)")?.textContent);
const selectedText = () => screen.getByTestId("selected").textContent;

afterEach(() => window.localStorage.clear());

describe("FileTable", () => {
  it("sorts by a column, folders first, and remembers the choice", async () => {
    const user = show();
    expect(rowNames()).toEqual(["docs", "photos", "a.txt", "b.txt", "c.log"]);
    await user.click(screen.getByRole("button", { name: "Size" }));
    expect(rowNames()).toEqual(["docs", "photos", "b.txt", "c.log", "a.txt"]);
    expect(screen.getByRole("columnheader", { name: "Size" })).toHaveAttribute(
      "aria-sort",
      "descending",
    );
    expect(window.localStorage.getItem("syncr-file-sort")).toBe('{"key":"size","desc":true}');
    await user.click(screen.getByRole("button", { name: "Modified" }));
    expect(rowNames()).toEqual(["docs", "photos", "b.txt", "c.log", "a.txt"]);
  });

  it("starts with the remembered sort", () => {
    window.localStorage.setItem("syncr-file-sort", '{"key":"name","desc":true}');
    show();
    expect(rowNames()).toEqual(["photos", "docs", "c.log", "b.txt", "a.txt"]);
  });

  it("moves, opens, ticks and goes up with the keyboard", async () => {
    const onOpen = vi.fn();
    const onUp = vi.fn();
    const user = show({ onOpen, onUp });
    const rows = bodyRows();
    // One row is the tab stop.
    expect(rows.map((r) => r.tabIndex)).toEqual([0, -1, -1, -1, -1]);
    act(() => rows[0].focus());
    await user.keyboard("{ArrowDown}");
    expect(rows[1]).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(onOpen).toHaveBeenCalledWith(expect.objectContaining({ name: "photos" }));
    await user.keyboard("{End}");
    expect(rows[4]).toHaveFocus();
    await user.keyboard(" ");
    expect(selectedText()).toBe("/srv/c.log");
    expect(rows[4]).toHaveAttribute("aria-selected", "true");
    expect(rows[3]).toHaveAttribute("aria-selected", "false");
    expect(await screen.findByText("1 selected")).toBeInTheDocument();
    await user.keyboard("{Home}{Backspace}");
    expect(rows[0]).toHaveFocus();
    expect(onUp).toHaveBeenCalledTimes(1);
    await user.keyboard("{Alt>}{ArrowUp}{/Alt}");
    expect(onUp).toHaveBeenCalledTimes(2);
  });

  it("ticks ranges in the shown order with Shift+Space and Shift+click", async () => {
    const user = show();
    await user.click(screen.getByRole("button", { name: "Size" })); // b, c, a
    const rows = bodyRows();
    act(() => rows[2].focus()); // b.txt
    await user.keyboard(" ");
    await user.keyboard("{ArrowDown}{ArrowDown}{Shift>} {/Shift}");
    expect(selectedText()).toBe("/srv/b.txt,/srv/c.log,/srv/a.txt");
    // Shift-click from the anchor (a.txt) unticks like the anchor.
    await user.click(screen.getByRole("checkbox", { name: "Select a.txt" }));
    await user.keyboard("{Shift>}");
    await user.click(screen.getByRole("checkbox", { name: "Select c.log" }));
    await user.keyboard("{/Shift}");
    expect(selectedText()).toBe("/srv/b.txt");
  });

  it("selects every row matching the filter, not only the shown page", async () => {
    const user = show({ pageSize: 2 });
    await user.type(screen.getByLabelText("Filter this folder"), "o");
    // docs, photos and c.log match; two are shown.
    expect(rowNames()).toEqual(["docs", "photos"]);
    await user.click(screen.getByRole("checkbox", { name: "Select all" }));
    expect(selectedText()).toBe("/srv/docs,/srv/photos,/srv/c.log");
    expect(
      screen.getByText("Selected all 3 matching the filter, not only the 2 shown."),
    ).toBeInTheDocument();
    // Escape clears the filter first, then the selection.
    act(() => bodyRows()[0].focus());
    await user.keyboard("{Escape}");
    expect(screen.getByLabelText("Filter this folder")).toHaveValue("");
    act(() => bodyRows()[0].focus());
    await user.keyboard("{Escape}");
    expect(selectedText()).toBe("");
    // Ctrl+A ticks every row.
    act(() => bodyRows()[0].focus());
    await user.keyboard("{Control>}a{/Control}");
    expect(selectedText()?.split(",")).toHaveLength(5);
  });

  it("says a truncated listing's select-all covers only what was listed", async () => {
    const user = show({ truncated: true, pageSize: 2 });
    await user.click(screen.getByRole("checkbox", { name: "Select all" }));
    expect(
      screen.getByText("Selected the first 5 entries, all that could be listed."),
    ).toBeInTheDocument();
  });

  it("renders the page's row menu and folder size cells", async () => {
    const onSelect = vi.fn<(path: string) => void>();
    const user = show({
      rowMenu: (e) =>
        e.name === "a.txt" ? [] : [{ label: "Copy path", onSelect: () => onSelect(e.path) }],
      renderFolderSize: (e) => (e.name === "docs" ? "4 KB" : undefined),
    });
    const docs = bodyRows()[0];
    expect(docs.querySelector("td:nth-child(3)")).toHaveTextContent("4 KB");
    expect(bodyRows()[1].querySelector("td:nth-child(3)")).toHaveTextContent("–");
    expect(screen.queryByRole("button", { name: "Actions for a.txt" })).toBeNull();
    await user.click(screen.getByRole("button", { name: "Actions for docs" }));
    await user.click(await screen.findByRole("menuitem", { name: "Copy path" }));
    expect(onSelect).toHaveBeenCalledWith("/srv/docs");
  });

  it("explains a folder that isn't there and offers its parent", async () => {
    const onGoToParent = vi.fn();
    const error = new ConnectError("invalid argument", Code.InvalidArgument, undefined, [
      {
        desc: ValidationErrorSchema,
        value: { violations: [{ field: "path", message: "does not exist" }] },
      },
    ]);
    const user = show({ error, onGoToParent });
    expect(screen.getByRole("alert")).toHaveTextContent("This folder isn't here");
    await user.click(screen.getByRole("button", { name: "Go to the nearest parent" }));
    expect(onGoToParent).toHaveBeenCalled();
  });
});
