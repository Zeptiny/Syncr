import { Code, ConnectError } from "@connectrpc/connect";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { ValidationErrorSchema, WorkerAdminService } from "@/gen/syncr/v1/api_pb";
import * as fx from "@/test/fixtures";
import { mockTransport, renderWithProviders } from "@/test/render";

import { WorkerLabelsPanel } from "./worker-labels";

const now = Date.parse("2026-11-08T12:00:00Z");

function api(fail = false) {
  const calls: { workerId: string; labels: string[] }[] = [];
  const transport = mockTransport(({ service }) => {
    service(WorkerAdminService, {
      listWorkers: () => ({ workers: fx.workers(now) }),
      setWorkerLabels: (req) => {
        calls.push({ workerId: req.workerId, labels: [...req.labels] });
        if (fail)
          throw new ConnectError("invalid", Code.InvalidArgument, undefined, [
            {
              desc: ValidationErrorSchema,
              value: { violations: [{ field: "labels[0]", message: "Label taken by policy." }] },
            },
          ]);
        const w = fx.workers(now).find((x) => x.id === req.workerId)!;
        w.labels = req.labels;
        return { worker: w };
      },
    });
  });
  return { transport, calls };
}

const nas = () => fx.workers(now).find((w) => w.id === fx.ids.nas)!;

describe("WorkerLabelsPanel", () => {
  it("shows labels read-only to users", () => {
    renderWithProviders(<WorkerLabelsPanel worker={nas()} canEdit={false} />);
    expect(screen.getByText("linux")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Edit labels" })).not.toBeInTheDocument();
  });

  it("lets an admin edit and save normalized labels", async () => {
    const user = userEvent.setup();
    const { transport, calls } = api();
    const onSaved = vi.fn();
    renderWithProviders(<WorkerLabelsPanel worker={nas()} canEdit />, { transport });
    await user.click(screen.getByRole("button", { name: "Edit labels" }));
    const save = screen.getByRole("button", { name: "Save labels" });
    expect(save).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Remove label linux" }));
    await user.type(screen.getByRole("textbox"), "Media{Enter}");
    await user.click(save);
    await waitFor(() =>
      expect(calls).toEqual([{ workerId: fx.ids.nas, labels: ["media", "nas"] }]),
    );
    onSaved();
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Save labels" })).not.toBeInTheDocument(),
    );
  });

  it("shows the server's error", async () => {
    const user = userEvent.setup();
    const { transport } = api(true);
    renderWithProviders(<WorkerLabelsPanel worker={nas()} canEdit />, { transport });
    await user.click(screen.getByRole("button", { name: "Edit labels" }));
    await user.type(screen.getByRole("textbox"), "x{Enter}");
    await user.click(screen.getByRole("button", { name: "Save labels" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Label taken by policy.");
  });
});
