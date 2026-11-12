import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { ResticRestoreOverwrite } from "@/gen/syncr/v1/link_pb";

import { OverwriteChoice } from "../overwrite-choice";

const options: [string, ResticRestoreOverwrite][] = [
  ["Keep existing", ResticRestoreOverwrite.NEVER],
  ["Replace files that differ", ResticRestoreOverwrite.IF_CHANGED],
  ["Replace older files", ResticRestoreOverwrite.IF_NEWER],
  ["Replace all", ResticRestoreOverwrite.ALWAYS],
];

describe("OverwriteChoice", () => {
  it("offers four choices, in order from safest to most destructive", () => {
    render(<OverwriteChoice value={ResticRestoreOverwrite.NEVER} onChange={() => {}} />);
    const radios = screen.getAllByRole("radio");
    expect(radios.map((r) => r.textContent?.match(/^[A-Z][a-z ]+?(?=[A-Z]|$)/)?.[0])).toEqual(
      options.map(([label]) => label),
    );
    expect(screen.getByRole("radiogroup", { name: "If a file already exists" })).toBeVisible();
  });

  it.each(options)("maps %s to its overwrite value", async (label, value) => {
    const onChange = vi.fn();
    render(<OverwriteChoice value={ResticRestoreOverwrite.NEVER} onChange={onChange} />);
    await userEvent.setup().click(screen.getByRole("radio", { name: new RegExp(`^${label}`) }));
    expect(onChange).toHaveBeenCalledWith(value);
  });

  it.each(options)("shows %s as chosen for its value", (label, value) => {
    render(<OverwriteChoice value={value} onChange={() => {}} />);
    const checked = screen
      .getAllByRole("radio")
      .filter((r) => r.getAttribute("aria-checked") === "true");
    expect(checked).toHaveLength(1);
    expect(checked[0]).toHaveTextContent(label);
  });

  it("shows an unset value as Keep existing", () => {
    render(<OverwriteChoice value={ResticRestoreOverwrite.UNSPECIFIED} onChange={() => {}} />);
    expect(screen.getByRole("radio", { name: /^Keep existing/ })).toHaveAttribute(
      "aria-checked",
      "true",
    );
  });
});
