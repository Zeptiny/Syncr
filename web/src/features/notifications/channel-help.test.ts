import { describe, expect, it } from "vitest";

import { docHref } from "@/features/docs/doc-link";

import { channelHelpFor } from "./channel-help";

describe("channelHelpFor", () => {
  it("links each channel to its section", () => {
    expect(docHref(channelHelpFor("discord"))).toBe("/docs/notifications#discord");
    expect(docHref(channelHelpFor("email"))).toBe("/docs/notifications#email");
    expect(docHref(channelHelpFor("webhook"))).toBe("/docs/notifications#webhook");
  });

  it("falls back to the notifications page for an unknown channel", () => {
    expect(channelHelpFor("matrix")).toEqual({ page: "notifications" });
    expect(channelHelpFor(undefined)).toEqual({ page: "notifications" });
    expect(channelHelpFor("constructor")).toEqual({ page: "notifications" });
  });
});
