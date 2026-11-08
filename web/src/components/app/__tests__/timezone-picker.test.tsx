import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { renderWithProviders } from "@/test/render";

import { TimezonePicker } from "../timezone-picker";

describe("TimezonePicker", () => {
  it("filters zones as you type and picks one with the keyboard", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    renderWithProviders(<TimezonePicker id="tz" value="UTC" onChange={onChange} />);
    expect(screen.getByRole("combobox")).toHaveTextContent("UTC");
    await user.click(screen.getByRole("combobox"));
    await user.type(screen.getByLabelText("Search time zones"), "berlin");
    const options = screen.getAllByRole("option");
    expect(options).toHaveLength(1);
    expect(options[0]).toHaveTextContent("Europe / Berlin");
    expect(options[0]).toHaveTextContent(/\+0[12]:00/);
    await user.keyboard("{Enter}");
    expect(onChange).toHaveBeenCalledWith("Europe/Berlin");
  });

  it("finds zones by offset and says when nothing matches", async () => {
    const user = userEvent.setup();
    renderWithProviders(<TimezonePicker id="tz" value="UTC" onChange={() => {}} />);
    await user.click(screen.getByRole("combobox"));
    await user.type(screen.getByLabelText("Search time zones"), "+05:45");
    // Nepal is the only +05:45 zone (named Kathmandu or Katmandu by the ICU in use).
    expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual([
      expect.stringMatching(/Kat/),
    ]);
    await user.clear(screen.getByLabelText("Search time zones"));
    await user.type(screen.getByLabelText("Search time zones"), "atlantis");
    expect(screen.queryAllByRole("option")).toHaveLength(0);
    expect(screen.getByText(/No time zone matches/)).toBeInTheDocument();
  });
});
