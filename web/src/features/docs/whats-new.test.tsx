import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { SystemService } from "@/gen/syncr/v1/api_pb";
import { mockTransport, renderInRouter } from "@/test/render";

import {
  acknowledged,
  acknowledgeRelease,
  WHATS_NEW_KEY,
  whatsNew,
  WhatsNewNotice,
} from "./whats-new";

/** memoryStorage is a Storage over a Map (failing: every call throws). */
function memoryStorage(initial?: string, failing = false): Storage {
  const data = new Map<string, string>(initial === undefined ? [] : [[WHATS_NEW_KEY, initial]]);
  const fail = () => {
    throw new DOMException("denied", "SecurityError");
  };
  return {
    get length() {
      return data.size;
    },
    clear: () => (failing ? fail() : data.clear()),
    getItem: (k) => (failing ? fail() : (data.get(k) ?? null)),
    key: (i) => [...data.keys()][i] ?? null,
    removeItem: (k) => (failing ? fail() : void data.delete(k)),
    setItem: (k, v) => (failing ? fail() : void data.set(k, v)),
  };
}

describe("whatsNew", () => {
  it.each([
    ["first visit", "v2.3.0", undefined, "v2.3.0"],
    ["an upgrade", "v2.3.0", "v2.2.0", "v2.3.0"],
    ["the same release", "v2.3.0", "v2.3.0", undefined],
    ["a downgrade", "v2.2.0", "v2.3.0", undefined],
    ["a release candidate, first visit", "v2.3.0-rc.1", undefined, "v2.3.0"],
    ["the final release after its candidate", "v2.3.0", "v2.3.0", undefined],
    ["a later candidate", "v2.3.0-rc.2", "v2.3.0", undefined],
    ["an unreadable stored value", "v2.3.0", "yesterday", "v2.3.0"],
    ["a stored candidate (never written)", "v2.3.0", "v2.3.0-rc.1", "v2.3.0"],
    ["a development build", "dev", undefined, undefined],
    ["a git describe build", "v2.3.0-rc.1-4-gabc1234", undefined, undefined],
    ["a version without notes", "v9.9.9", undefined, undefined],
    ["no version yet", undefined, undefined, undefined],
  ])("%s", (_, version, stored, shown) => {
    expect(whatsNew(version, stored)?.base).toBe(shown);
  });

  it("names the running version and its notes page", () => {
    expect(whatsNew("v2.3.0-rc.1", undefined)).toEqual({
      version: "v2.3.0-rc.1",
      base: "v2.3.0",
      notes: "releases/v2.3.0",
    });
  });
});

describe("acknowledgeRelease", () => {
  it("stores the newest final version only", () => {
    const storage = memoryStorage();
    acknowledgeRelease("v2.2.0", storage);
    expect(storage.getItem(WHATS_NEW_KEY)).toBe("v2.2.0");
    acknowledgeRelease("v2.3.0", storage);
    acknowledgeRelease("v2.1.0", storage);
    acknowledgeRelease("v2.4.0-rc.1", storage);
    expect(storage.getItem(WHATS_NEW_KEY)).toBe("v2.3.0");
  });

  it("replaces an unreadable value", () => {
    const storage = memoryStorage("garbage");
    acknowledgeRelease("v2.0.0", storage);
    expect(storage.getItem(WHATS_NEW_KEY)).toBe("v2.0.0");
  });

  it("survives a storage that refuses every call", () => {
    const storage = memoryStorage(undefined, true);
    expect(acknowledged(storage)).toBeUndefined();
    expect(() => acknowledgeRelease("v2.3.0", storage)).not.toThrow();
    expect(acknowledged(storage)).toBe("v2.3.0");
  });
});

describe("WhatsNewNotice", () => {
  const api = (version: string) =>
    mockTransport(({ service }) => {
      service(SystemService, { getInfo: () => ({ version }) });
    });

  it("shows after an upgrade and goes away for good when dismissed", async () => {
    const user = userEvent.setup();
    const storage = memoryStorage("v2.2.0");
    renderInRouter(<WhatsNewNotice storage={storage} />, { transport: api("v2.3.0") });
    const region = await screen.findByRole("region", { name: "What's new" });
    expect(region).toHaveTextContent("Syncr was updated to v2.3.0.");
    expect(screen.getByRole("link", { name: "Read the release notes" })).toHaveAttribute(
      "href",
      "/docs/releases/v2.3.0",
    );
    await user.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByRole("region", { name: "What's new" })).not.toBeInTheDocument();
    expect(storage.getItem(WHATS_NEW_KEY)).toBe("v2.3.0");
  });

  it("records the release when the notes are opened", async () => {
    const user = userEvent.setup();
    const storage = memoryStorage();
    const { location } = renderInRouter(<WhatsNewNotice storage={storage} />, {
      transport: api("v2.3.0-rc.2"),
    });
    expect(await screen.findByRole("region", { name: "What's new" })).toHaveTextContent(
      "Syncr was updated to v2.3.0-rc.2.",
    );
    await user.click(screen.getByRole("link", { name: "Read the release notes" }));
    await waitFor(() => expect(location().pathname).toBe("/docs/releases/v2.3.0"));
    expect(storage.getItem(WHATS_NEW_KEY)).toBe("v2.3.0");
    expect(screen.queryByRole("region", { name: "What's new" })).not.toBeInTheDocument();
  });

  it("hides when the release was opened elsewhere (another tab, the notes page)", async () => {
    const storage = memoryStorage();
    renderInRouter(<WhatsNewNotice storage={storage} />, { transport: api("v2.3.0") });
    await screen.findByRole("region", { name: "What's new" });
    acknowledgeRelease("v2.3.0", storage);
    await waitFor(() =>
      expect(screen.queryByRole("region", { name: "What's new" })).not.toBeInTheDocument(),
    );
  });

  it("dismisses even when the browser refuses storage", async () => {
    const user = userEvent.setup();
    renderInRouter(<WhatsNewNotice storage={memoryStorage(undefined, true)} />, {
      transport: api("v2.3.0"),
    });
    await user.click(await screen.findByRole("button", { name: "Dismiss" }));
    expect(screen.queryByRole("region", { name: "What's new" })).not.toBeInTheDocument();
  });

  it("shows nothing for a development build or without GetInfo", async () => {
    const storage = memoryStorage();
    const dev = renderInRouter(<WhatsNewNotice storage={storage} />, { transport: api("dev") });
    await waitFor(() =>
      expect(dev.queryClient.getQueryCache().getAll()[0]?.state.status).toBe("success"),
    );
    expect(screen.queryByRole("region", { name: "What's new" })).not.toBeInTheDocument();
    dev.unmount();
    const none = renderInRouter(<WhatsNewNotice storage={storage} />);
    await waitFor(() =>
      expect(none.queryClient.getQueryCache().getAll()[0]?.state.status).toBe("error"),
    );
    expect(screen.queryByRole("region", { name: "What's new" })).not.toBeInTheDocument();
    expect(storage.length).toBe(0);
  });
});
