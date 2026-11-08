import { Code, ConnectError } from "@connectrpc/connect";
import { createColumnHelper } from "@tanstack/react-table";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { DataTable } from "../data-table";

interface Row {
  id: string;
  name: string;
}
const col = createColumnHelper<Row>();
const columns = [col.accessor("name", { header: "Name" })];
const rows: Row[] = [
  { id: "1", name: "alpha" },
  { id: "2", name: "beta" },
];

describe("DataTable", () => {
  it("shows skeleton rows while loading", () => {
    render(<DataTable columns={columns} data={undefined} isLoading skeletonRows={3} />);
    expect(screen.getAllByTestId("skeleton-row")).toHaveLength(3);
    expect(screen.getByRole("table")).toHaveAttribute("aria-busy", "true");
  });

  it("shows the empty state for an empty list", () => {
    render(<DataTable columns={columns} data={[]} empty={<p>No remotes yet</p>} />);
    expect(screen.getByText("No remotes yet")).toBeInTheDocument();
  });

  it("shows the error with a retry", async () => {
    const onRetry = vi.fn();
    render(
      <DataTable
        columns={columns}
        data={undefined}
        error={new ConnectError("boom", Code.Unavailable)}
        onRetry={onRetry}
      />,
    );
    expect(screen.getByRole("alert")).toHaveTextContent("Boom");
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(onRetry).toHaveBeenCalled();
  });

  it("renders rows that open on click and Enter, and pages", async () => {
    const user = userEvent.setup();
    const onRowClick = vi.fn();
    const onNext = vi.fn();
    const onPrev = vi.fn();
    render(
      <DataTable
        columns={columns}
        data={rows}
        getRowId={(r) => r.id}
        rowLabel={(r) => r.name}
        onRowClick={onRowClick}
        pager={{ page: 2, canPrev: true, canNext: false, onNext, onPrev }}
      />,
    );
    await user.click(screen.getByRole("row", { name: "beta" }));
    expect(onRowClick).toHaveBeenCalledWith(rows[1]);
    screen.getByRole("row", { name: "alpha" }).focus();
    await user.keyboard("{Enter}");
    expect(onRowClick).toHaveBeenCalledWith(rows[0]);

    expect(screen.getByText("Page 2")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Older" })).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Newer" }));
    expect(onPrev).toHaveBeenCalled();
  });

  it("hides the pager on a single page", () => {
    render(
      <DataTable
        columns={columns}
        data={rows}
        pager={{ page: 1, canPrev: false, canNext: false, onNext: () => {}, onPrev: () => {} }}
      />,
    );
    expect(screen.queryByRole("navigation")).not.toBeInTheDocument();
  });
});
