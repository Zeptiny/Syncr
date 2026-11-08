import { durationFromMs } from "@bufbuild/protobuf/wkt";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { ContactService, NotificationEvent, type ContactInput } from "@/gen/syncr/v1/notify_pb";
import * as fx from "@/test/fixtures";
import { mockTransport, renderWithProviders } from "@/test/render";

import {
  ContactForm,
  contactDefaults,
  toContactInput,
  type ContactInputInit,
} from "./contact-form";

const now = Date.parse("2026-11-08T12:00:00Z");
const channel = (id: string) => fx.channels.find((c) => c.id === id)!;
const contact = (id: string) => fx.contacts(now).find((c) => c.id === id)!;

function api(ok = true) {
  const tests: { contactId: string; input: ContactInput | undefined }[] = [];
  const transport = mockTransport(({ service }) => {
    service(ContactService, {
      sendTestNotification: (req) => {
        tests.push({ contactId: req.contactId, input: req.input });
        return ok
          ? { ok: true, status: 250, latency: durationFromMs(412) }
          : { ok: false, status: 535, error: "SMTP 535: authentication failed" };
      },
    });
  });
  return { transport, tests };
}

describe("contact form model", () => {
  it("fills defaults from the channel and keeps secrets empty", () => {
    const v = contactDefaults(channel("email"), contact(fx.ids.adminMail));
    expect(v.values).toMatchObject({ host: "smtp.example.net", port: "587", password: "" });
    expect(contactDefaults(channel("email")).values).toMatchObject({
      port: "587",
      security: "starttls",
    });
    expect(
      toContactInput(channel("webhook"), {
        ...contactDefaults(channel("webhook")),
        name: " Hook ",
        values: {
          url: "https://h.example/x",
          signing_secret: "",
          format: "syncr",
          authorization: "",
        },
        jobEvents: [NotificationEvent.FAILED],
      }),
    ).toEqual({
      name: "Hook",
      channelType: "webhook",
      enabled: true,
      jobEvents: [NotificationEvent.FAILED],
      options: { format: "syncr" },
      secrets: { url: "https://h.example/x" },
      clearSecrets: [],
    });
  });
});

describe("ContactForm", () => {
  it("keeps a stored secret when left empty and clears it on request", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn().mockResolvedValue(undefined);
    renderWithProviders(
      <ContactForm
        channel={channel("email")}
        contact={contact(fx.ids.adminMail)}
        onSubmit={onSubmit}
      />,
      { transport: api().transport },
    );
    const password = screen.getByLabelText(/^Password/);
    expect(password).toHaveValue("");
    expect(password).toHaveAttribute("placeholder", "Set. Leave empty to keep it.");
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
    expect(onSubmit.mock.calls[0][0]).toMatchObject({
      name: "Admin mail",
      options: { host: "smtp.example.net", to: "dana@example.net, ops@example.net" },
      secrets: {},
      clearSecrets: [],
    });

    await user.click(screen.getByRole("button", { name: "Clear Password" }));
    expect(screen.getByText("Removed when you save.")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(2));
    expect(onSubmit.mock.calls[1][0]).toMatchObject({ secrets: {}, clearSecrets: ["password"] });
  });

  it("requires a secret again only on a new contact", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn().mockResolvedValue(undefined);
    renderWithProviders(<ContactForm channel={channel("discord")} onSubmit={onSubmit} />, {
      transport: api().transport,
    });
    await user.type(screen.getByLabelText("Name"), "Team");
    await user.click(screen.getByRole("button", { name: "Add contact" }));
    expect(await screen.findByText("Webhook URL is required.")).toBeInTheDocument();
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it("sends a test with the unsaved values and shows the result", async () => {
    const user = userEvent.setup();
    const { transport, tests } = api();
    renderWithProviders(
      <ContactForm
        channel={channel("email")}
        contact={contact(fx.ids.adminMail)}
        onSubmit={vi.fn()}
      />,
      { transport },
    );
    const to = screen.getByLabelText("To");
    await user.clear(to);
    await user.type(to, "me@example.net");
    await user.click(screen.getByRole("button", { name: "Send test" }));
    expect(await screen.findByText("Test notification sent")).toBeInTheDocument();
    expect(screen.getByText(/The server replied 250 in 412 ms/)).toBeInTheDocument();
    expect(tests).toHaveLength(1);
    expect(tests[0].contactId).toBe(fx.ids.adminMail);
    expect(tests[0].input?.options.to).toBe("me@example.net");
    // The stored password is kept by the server: nothing secret is sent.
    expect(tests[0].input?.secrets).toEqual({});
  });

  it("shows why a test failed", async () => {
    const user = userEvent.setup();
    renderWithProviders(
      <ContactForm
        channel={channel("email")}
        contact={contact(fx.ids.adminMail)}
        onSubmit={vi.fn()}
      />,
      { transport: api(false).transport },
    );
    await user.click(screen.getByRole("button", { name: "Send test" }));
    expect(await screen.findByText("The test notification was not delivered")).toBeInTheDocument();
    expect(screen.getByText(/authentication failed \(status 535\)/)).toBeInTheDocument();
  });

  it("generates a webhook signing secret", async () => {
    const user = userEvent.setup();
    renderWithProviders(<ContactForm channel={channel("webhook")} onSubmit={vi.fn()} />, {
      transport: api().transport,
    });
    await user.click(screen.getByRole("button", { name: "Generate" }));
    expect(screen.getByLabelText<HTMLInputElement>(/^Signing secret/).value).toMatch(
      /^whsec_[A-Za-z0-9+/]{32}$/,
    );
  });

  it("sets the events of jobs run by hand", async () => {
    const user = userEvent.setup();
    const onSubmit = vi
      .fn<(input: ContactInputInit) => Promise<unknown>>()
      .mockResolvedValue(undefined);
    renderWithProviders(
      <ContactForm
        channel={channel("webhook")}
        contact={contact(fx.ids.homeAssistant)}
        onSubmit={onSubmit}
      />,
      { transport: api().transport },
    );
    expect(screen.getByRole("checkbox", { name: "Failed" })).toHaveAttribute(
      "aria-checked",
      "true",
    );
    await user.click(screen.getByRole("checkbox", { name: "Interrupted" }));
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalled());
    expect(onSubmit.mock.calls[0][0].jobEvents).toEqual([
      NotificationEvent.FAILED,
      NotificationEvent.INTERRUPTED,
    ]);
  });
});
