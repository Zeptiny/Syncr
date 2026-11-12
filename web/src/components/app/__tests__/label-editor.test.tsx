import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";

import { renderWithProviders } from "@/test/render";

import { LabelEditor } from "../label-editor";

function Harness({ initial = ["nas"], onChange = vi.fn(), suggestions = [] as string[] }) {
  const [v, setV] = useState(initial);
  return (
    <LabelEditor
      id="labels"
      value={v}
      suggestions={suggestions}
      onChange={(x) => {
        onChange(x);
        setV(x);
      }}
    />
  );
}

describe("LabelEditor", () => {
  it("adds labels on Enter, comma and space, lower-cased and sorted", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    renderWithProviders(<Harness onChange={onChange} />);
    const input = screen.getByRole("textbox");
    await user.type(input, "Linux{Enter}");
    expect(onChange).toHaveBeenLastCalledWith(["linux", "nas"]);
    await user.type(input, "offsite,");
    expect(onChange).toHaveBeenLastCalledWith(["linux", "nas", "offsite"]);
    await user.type(input, "nas ");
    expect(onChange).toHaveBeenLastCalledWith(["linux", "nas", "offsite"]);
    expect(input).toHaveValue("");
  });

  it("rejects an invalid label and keeps the text to fix", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    renderWithProviders(<Harness onChange={onChange} />);
    const input = screen.getByRole("textbox");
    await user.type(input, "-bad{Enter}");
    expect(screen.getByRole("alert")).toHaveTextContent("Start with a letter or digit");
    expect(input).toHaveValue("-bad");
    expect(onChange).not.toHaveBeenCalled();
  });

  it("removes labels with their button and with Backspace", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    renderWithProviders(<Harness initial={["linux", "nas"]} onChange={onChange} />);
    await user.click(screen.getByRole("button", { name: "Remove label linux" }));
    expect(onChange).toHaveBeenLastCalledWith(["nas"]);
    await user.click(screen.getByRole("textbox"));
    await user.keyboard("{Backspace}");
    expect(onChange).toHaveBeenLastCalledWith([]);
  });

  it("caps a worker at 16 labels", async () => {
    const user = userEvent.setup();
    const many = Array.from({ length: 16 }, (_, i) => `l${String(i).padStart(2, "0")}`);
    renderWithProviders(<Harness initial={many} />);
    await user.type(screen.getByRole("textbox"), "one-more{Enter}");
    expect(screen.getByRole("alert")).toHaveTextContent("at most 16 labels");
  });

  it("offers labels other workers use", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    renderWithProviders(
      <Harness initial={[]} onChange={onChange} suggestions={["offsite", "nas"]} />,
    );
    await user.type(screen.getByRole("textbox"), "o");
    await user.click(screen.getByRole("button", { name: "offsite" }));
    expect(onChange).toHaveBeenLastCalledWith(["offsite"]);
  });
});
