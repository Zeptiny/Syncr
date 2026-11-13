import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import * as fx from "@/test/fixtures";
import { m5Api } from "@/test/m5-api";
import { renderApp } from "@/test/render";

describe("RemotesPage", () => {
  it("browses a remote's files as well as its folders", async () => {
    const user = userEvent.setup();
    const api = m5Api(Date.now());
    renderApp("/remotes", api.transport);

    await user.click(await screen.findByRole("button", { name: "Actions for Storage box" }));
    await user.click(await screen.findByRole("menuitem", { name: "Browse" }));
    await user.click(await screen.findByRole("button", { name: "db-dumps" }));

    // Files are listed, not only folders.
    expect(await screen.findByText("dump-2026-11-09.sql.gz")).toBeInTheDocument();
    await waitFor(() =>
      expect(api.of("ListRemoteFiles").at(-1)).toMatchObject({
        remoteId: fx.ids.storageBox,
        path: "db-dumps",
        dirsOnly: false,
      }),
    );
  });
});
