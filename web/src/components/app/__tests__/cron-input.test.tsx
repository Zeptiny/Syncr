import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";

import { ValidationErrorSchema } from "@/gen/syncr/v1/api_pb";
import { ScheduleService } from "@/gen/syncr/v1/schedules_pb";
import { mockTransport, renderWithProviders } from "@/test/render";

import { CronInput } from "../cron-input";

function Harness({ initial = "30 2 * * *", timezone = "Europe/Berlin" }) {
  const [v, setV] = useState(initial);
  return <CronInput id="cron" value={v} onChange={setV} timezone={timezone} />;
}

function api(preview = vi.fn()) {
  const transport = mockTransport(({ service }) => {
    service(ScheduleService, {
      previewSchedule: (req) => {
        preview(req.cron, req.timezone, req.count);
        if (req.cron === "0 0 31 2 *") {
          throw new ConnectError("invalid", Code.InvalidArgument, undefined, [
            {
              desc: ValidationErrorSchema,
              value: { violations: [{ field: "cron", message: "It never runs." }] },
            },
          ]);
        }
        // Berlin, across the March 2027 clock change.
        const runs = ["2027-03-26T01:30:00Z", "2027-03-27T01:30:00Z", "2027-03-29T00:30:00Z"];
        return { nextRuns: runs.map((r) => timestampFromMs(Date.parse(r))), normalized: req.cron };
      },
    });
  });
  return { transport, preview };
}

describe("CronInput", () => {
  it("describes the expression and lists the server's next runs", async () => {
    const { transport, preview } = api();
    renderWithProviders(<Harness />, { transport });
    expect(screen.getByText("At 02:30")).toBeInTheDocument();
    const list = await screen.findByRole("list", { name: "Next runs" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(3);
    expect(within(list).getByText(/Fri, 26 Mar( 2027)?, 02:30/)).toBeInTheDocument();
    // The third run is past the clock change: its offset is marked.
    expect(within(list).getAllByText("UTC+01:00")).toHaveLength(2);
    expect(within(list).getByText("UTC+02:00")).toBeInTheDocument();
    expect(screen.getByText("Clocks change")).toBeInTheDocument();
    expect(preview).toHaveBeenCalledWith("30 2 * * *", "Europe/Berlin", 5);
  });

  it("shows a local error without asking the server", async () => {
    const user = userEvent.setup();
    const { transport, preview } = api();
    renderWithProviders(<Harness initial="" />, { transport });
    await user.type(screen.getByRole("textbox"), "0 0 * * * *");
    expect(await screen.findByRole("alert")).toHaveTextContent("Seconds are not supported");
    await new Promise((r) => setTimeout(r, 400));
    expect(preview).not.toHaveBeenCalled();
  });

  it("shows the server's error for an expression that never runs", async () => {
    const { transport } = api();
    renderWithProviders(<Harness initial="0 0 31 2 *" />, { transport });
    expect(await screen.findByRole("alert")).toHaveTextContent("It never runs.");
    expect(screen.queryByRole("list", { name: "Next runs" })).not.toBeInTheDocument();
  });

  it("fills in a preset", async () => {
    const user = userEvent.setup();
    const { transport, preview } = api();
    renderWithProviders(<Harness />, { transport });
    await user.click(screen.getByRole("button", { name: "Presets" }));
    await user.click(screen.getByRole("menuitem", { name: /Weekdays at 18:00/ }));
    expect(screen.getByRole("textbox")).toHaveValue("0 18 * * 1-5");
    expect(screen.getByText("At 18:00, Monday through Friday")).toBeInTheDocument();
    await waitFor(() => expect(preview).toHaveBeenCalledWith("0 18 * * 1-5", "Europe/Berlin", 5));
  });
});
