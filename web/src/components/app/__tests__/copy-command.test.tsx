import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import * as fx from "@/test/fixtures";
import { renderWithProviders } from "@/test/render";

import { CopyCommand } from "../copy-command";

describe("CopyCommand", () => {
  it("switches platform and scope and copies the selected command", async () => {
    const user = userEvent.setup();
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    const commands = fx.installCommands("tok_123");
    renderWithProviders(
      <CopyCommand commands={commands} shownOnce="This command is shown only once." />,
    );

    expect(screen.getByRole("note")).toHaveTextContent("shown only once");
    expect(screen.getByText(/sudo syncr-worker setup --controller/)).toBeInTheDocument();

    await user.click(screen.getByRole("radio", { name: "Current user" }));
    expect(screen.getByText(/setup --user --controller/)).toBeInTheDocument();

    await user.click(screen.getByRole("tab", { name: "Windows" }));
    // The chosen scope carries over to the other platform.
    expect(screen.getByText(/syncr-worker.exe" setup --user/)).toBeInTheDocument();
    expect(screen.getByText("PowerShell")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Copy command" }));
    expect(writeText).toHaveBeenCalledWith(commands[3].command);
    expect(await screen.findByText("Copied")).toBeInTheDocument();
  });

  it("hides the scope switch when a platform has one scope", () => {
    renderWithProviders(<CopyCommand commands={fx.passiveCommands()} />);
    expect(screen.queryByRole("radiogroup")).not.toBeInTheDocument();
    expect(screen.queryByRole("note")).not.toBeInTheDocument();
  });

  it("renders nothing without commands", () => {
    const { container } = renderWithProviders(<CopyCommand commands={[]} />);
    expect(container).toBeEmptyDOMElement();
  });
});
