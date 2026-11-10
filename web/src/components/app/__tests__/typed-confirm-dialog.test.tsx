import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { TypedConfirmDialog, type TypedConfirmDialogProps } from "../typed-confirm-dialog";

function open(props: Partial<TypedConfirmDialogProps> = {}) {
  const onConfirm = vi.fn();
  const onOpenChange = vi.fn();
  render(
    <TypedConfirmDialog
      open
      onOpenChange={onOpenChange}
      title="Delete Photos vault?"
      name="Photos vault"
      confirmLabel="Delete it"
      onConfirm={onConfirm}
      {...props}
    />,
  );
  return { onConfirm, onOpenChange, user: userEvent.setup() };
}

describe("TypedConfirmDialog", () => {
  it("enables the button only while the typed name matches exactly", async () => {
    const { user, onConfirm, onOpenChange } = open();
    const button = screen.getByRole("button", { name: "Delete it" });
    const input = screen.getByLabelText(/Type Photos vault to confirm/);
    expect(button).toBeDisabled();

    for (const wrong of [
      "photos vault",
      "Photos vault ",
      "Photos",
      " Photos vault",
      "Photos  vault",
    ]) {
      await user.clear(input);
      await user.type(input, wrong);
      expect(button).toBeDisabled();
    }
    await user.clear(input);
    await user.type(input, "Photos vault");
    expect(button).toBeEnabled();
    // One more character and it is off again.
    await user.type(input, "!");
    expect(button).toBeDisabled();
    await user.type(input, "{Backspace}");

    await user.click(button);
    expect(onConfirm).toHaveBeenCalledWith("Photos vault");
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
  });

  it("stays disabled for another reason even when the name matches", async () => {
    const { user } = open({ disabled: true });
    await user.type(screen.getByLabelText(/to confirm/), "Photos vault");
    expect(screen.getByRole("button", { name: "Delete it" })).toBeDisabled();
  });

  it("asks for no name when requireName is off", async () => {
    const { user, onConfirm } = open({ requireName: false, confirmLabel: "Forget it" });
    expect(screen.queryByLabelText(/to confirm/)).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Forget it" }));
    expect(onConfirm).toHaveBeenCalledWith("");
  });

  it("shows a failure and stays open", async () => {
    const onConfirm = vi.fn().mockRejectedValue(new Error("The worker is offline."));
    const { user, onOpenChange } = open({ onConfirm, nameError: "Type the name." });
    expect(screen.getByText("Type the name.")).toBeInTheDocument();
    await user.type(screen.getByLabelText(/to confirm/), "Photos vault");
    await user.click(screen.getByRole("button", { name: "Delete it" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("The worker is offline.");
    expect(onOpenChange).not.toHaveBeenCalledWith(false);
  });
});
