import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";

import { NotificationEvent } from "@/gen/syncr/v1/notify_pb";
import { manualJobEvents } from "@/lib/status";
import * as fx from "@/test/fixtures";
import { renderWithProviders } from "@/test/render";

import { ContactLinksEditor, type ContactLink } from "../contact-links-editor";
import { EventChips } from "../event-chips";

const now = Date.parse("2026-11-08T12:00:00Z");

describe("EventChips", () => {
  it("toggles events and keeps them in display order", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    function Harness() {
      const [v, setV] = useState<NotificationEvent[]>([NotificationEvent.MISSED]);
      return (
        <EventChips
          label="Events"
          value={v}
          onChange={(x) => {
            onChange(x);
            setV(x);
          }}
        />
      );
    }
    renderWithProviders(<Harness />);
    const missed = screen.getByRole("checkbox", { name: "Missed" });
    expect(missed).toHaveAttribute("aria-checked", "true");
    await user.click(screen.getByRole("checkbox", { name: "Failed" }));
    expect(onChange).toHaveBeenLastCalledWith([NotificationEvent.FAILED, NotificationEvent.MISSED]);
    await user.click(screen.getByRole("checkbox", { name: "Succeeded" }));
    expect(onChange).toHaveBeenLastCalledWith([
      NotificationEvent.SUCCEEDED,
      NotificationEvent.FAILED,
      NotificationEvent.MISSED,
    ]);
    await user.click(missed);
    expect(onChange).toHaveBeenLastCalledWith([
      NotificationEvent.SUCCEEDED,
      NotificationEvent.FAILED,
    ]);
  });

  it("offers only the given events", () => {
    renderWithProviders(
      <EventChips label="Manual" value={[]} onChange={() => {}} options={manualJobEvents} />,
    );
    expect(screen.getAllByRole("checkbox")).toHaveLength(3);
    expect(screen.queryByRole("checkbox", { name: "Missed" })).not.toBeInTheDocument();
  });
});

describe("ContactLinksEditor", () => {
  const contacts = fx.contacts(now);

  function Harness({ initial = [] as ContactLink[], onChange = vi.fn() }) {
    const [v, setV] = useState(initial);
    return (
      <ContactLinksEditor
        contacts={contacts}
        value={v}
        onChange={(x) => {
          onChange(x);
          setV(x);
        }}
        errors={{}}
      />
    );
  }

  it("adds a contact with failed, interrupted and missed pre-filled", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    renderWithProviders(<Harness onChange={onChange} />);
    await user.click(screen.getByRole("combobox", { name: "Notify a contact" }));
    await user.click(screen.getByRole("option", { name: "Ops Discord" }));
    expect(onChange).toHaveBeenLastCalledWith([
      {
        contactId: fx.ids.opsDiscord,
        events: [NotificationEvent.FAILED, NotificationEvent.INTERRUPTED, NotificationEvent.MISSED],
      },
    ]);
    expect(screen.getByText("Ops Discord")).toBeInTheDocument();
    // A linked contact is no longer offered.
    await user.click(screen.getByRole("combobox", { name: "Notify another contact" }));
    expect(screen.queryByRole("option", { name: "Ops Discord" })).not.toBeInTheDocument();
  });

  it("edits a link's events and removes it", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    renderWithProviders(
      <Harness
        onChange={onChange}
        initial={[{ contactId: fx.ids.adminMail, events: [NotificationEvent.FAILED] }]}
      />,
    );
    await user.click(screen.getByRole("checkbox", { name: "Succeeded" }));
    expect(onChange).toHaveBeenLastCalledWith([
      {
        contactId: fx.ids.adminMail,
        events: [NotificationEvent.SUCCEEDED, NotificationEvent.FAILED],
      },
    ]);
    await user.click(screen.getByRole("button", { name: "Stop notifying Admin mail" }));
    expect(onChange).toHaveBeenLastCalledWith([]);
  });
});
