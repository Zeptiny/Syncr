import { Code, ConnectError } from "@connectrpc/connect";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const isEqual = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);

import { SystemService } from "@/gen/syncr/v1/api_pb";
import { AuthMethod, AuthService, SetupService } from "@/gen/syncr/v1/auth_pb";
import { copyText } from "@/lib/clipboard";
import * as fx from "@/test/fixtures";
import { mockTransport, renderApp } from "@/test/render";

import { manifest } from "./manifest";
import { prepareIndex, searchDocs } from "./search";

vi.mock("@/lib/clipboard", () => ({ copyText: vi.fn(() => Promise.resolve()) }));

const now = Date.parse("2026-03-12T12:00:00Z");

function api(opts: { signedIn?: boolean; session?: "error"; version?: string } = {}) {
  const calls: string[] = [];
  const transport = mockTransport(({ service }) => {
    service(SetupService, { getSetupStatus: () => ({ setupRequired: false }) });
    service(AuthService, {
      getSession: () => {
        calls.push("GetSession");
        if (opts.session === "error") throw new ConnectError("boom", Code.Internal);
        if (!opts.signedIn) throw new ConnectError("no session", Code.Unauthenticated);
        return { user: fx.users(now)[0], method: AuthMethod.SESSION };
      },
    });
    service(SystemService, {
      getInfo: () => {
        calls.push("GetInfo");
        return { version: opts.version ?? "v2.3.0" };
      },
    });
  });
  return { transport, calls };
}

const title = (name: string) => screen.findByRole("heading", { level: 1, name });

beforeEach(() => {
  vi.spyOn(window, "scrollTo").mockImplementation(() => {});
});

describe("docs layouts", () => {
  it("shows the docs signed out, without sending the reader to /login", async () => {
    const { transport, calls } = api();
    const { router } = renderApp("/docs", transport);
    expect(await title("Syncr documentation")).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/docs");
    expect(screen.getByRole("link", { name: "Sign in" })).toHaveAttribute(
      "href",
      "/login?redirect=%2Fdocs",
    );
    expect(screen.queryByRole("link", { name: "Run a job" })).not.toBeInTheDocument();
    // Next to the navigation, and in the bar above the page on small screens.
    expect(await screen.findAllByText("Docs for v2.3.0")).toHaveLength(2);
    expect(new Set(calls)).toEqual(new Set(["GetSession", "GetInfo"]));
    expect(document.title).toBe("Syncr documentation · Syncr docs");
  });

  it("stays public when the session check fails", async () => {
    const { transport } = api({ session: "error" });
    const { router } = renderApp("/docs/restic", transport);
    expect(await title("Restic backups")).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/docs/restic");
    expect(screen.getByRole("link", { name: "Sign in" })).toBeInTheDocument();
  });

  it("shows the docs inside the app shell when signed in", async () => {
    const user = userEvent.setup();
    const { transport } = api({ signedIn: true });
    renderApp("/docs/restic", transport);
    expect(await title("Restic backups")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Run a job" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Docs" })).toHaveAttribute("data-active", "true");
    expect(screen.queryByRole("link", { name: "Sign in" })).not.toBeInTheDocument();
    // The release notes of the running version are new to this browser.
    expect(await screen.findByRole("region", { name: "What's new" })).toBeInTheDocument();
    // The skip link is the first stop and lands in the page.
    await user.tab();
    expect(screen.getByRole("link", { name: "Skip to content" })).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(document.activeElement?.id).toBe("main-content");
  });

  it("marks the open page in the navigation and links the file on GitHub", async () => {
    const { transport } = api({ version: "v2.3.0-rc.1" });
    renderApp("/docs/releases/v2.2.0", transport);
    expect(await title("Syncr 2.2.0")).toBeInTheDocument();
    const nav = screen.getByRole("navigation", { name: "Documentation" });
    expect(within(nav).getByRole("link", { name: "2.2.0" })).toHaveAttribute(
      "aria-current",
      "page",
    );
    // The parent of the open page is expanded.
    expect(within(nav).getByRole("button", { name: /Release notes/ })).toHaveAttribute(
      "aria-expanded",
      "true",
    );
    await waitFor(() =>
      expect(screen.getByRole("link", { name: /View on GitHub/ })).toHaveAttribute(
        "href",
        `${manifest.githubRepo}/blob/v2.3.0-rc.1/docs/releases/v2.2.0.md`,
      ),
    );
    // GitHub-only entries and links in the page are pinned to the same tag.
    expect(within(nav).getByRole("link", { name: /Development/ })).toHaveAttribute(
      "href",
      `${manifest.githubRepo}/blob/v2.3.0-rc.1/docs/development.md`,
    );
    expect(document.body.innerHTML).not.toContain(manifest.githubRefToken);
    const pager = screen.getByRole("navigation", { name: "Previous and next page" });
    expect(within(pager).getByRole("link", { name: /Previous/ })).toHaveAttribute(
      "href",
      "/docs/releases/v2.3.0",
    );
  });

  it("asks for the session once while a signed-out reader moves between pages", async () => {
    const user = userEvent.setup();
    const { transport, calls } = api();
    renderApp("/docs/restic", transport);
    await title("Restic backups");
    await user.click(
      document.querySelector<HTMLAnchorElement>('.doc-prose a[href="/docs/browsing"]')!,
    );
    await title("Browsing files");
    const nav = screen.getByRole("navigation", { name: "Documentation" });
    await user.click(within(nav).getByRole("link", { name: "Two-way sync" }));
    await title("Two-way sync");
    expect(calls.filter((c) => c === "GetSession")).toHaveLength(1);
  });

  it("keeps the sidebar collapsed between the app and the docs", async () => {
    const { transport } = api({ signedIn: true });
    const { router } = renderApp("/docs/restic", transport);
    await title("Restic backups");
    const sidebar = () => document.querySelector('[data-slot="sidebar"][data-state]');
    expect(sidebar()).toHaveAttribute("data-state", "expanded");
    fireEvent.keyDown(window, { key: "b", ctrlKey: true });
    expect(sidebar()).toHaveAttribute("data-state", "collapsed");
    // Another layout, another sidebar: it starts the way the reader left it.
    await router.navigate({ to: "/settings" });
    await waitFor(() => expect(router.state.location.pathname).toBe("/settings"));
    await waitFor(() =>
      expect(screen.queryByRole("heading", { name: "Restic backups" })).toBeNull(),
    );
    expect(sidebar()).toHaveAttribute("data-state", "collapsed");
    fireEvent.keyDown(window, { key: "b", ctrlKey: true });
    await router.navigate({ to: "/docs" });
    expect(await title("Syncr documentation")).toBeInTheDocument();
    expect(sidebar()).toHaveAttribute("data-state", "expanded");
    document.cookie = "sidebar_state=; path=/; max-age=0";
  });

  it("asks for a reload when a page's chunk is gone after an upgrade", async () => {
    vi.spyOn(manifest.loaders, "bisync").mockRejectedValue(
      new TypeError("Failed to fetch dynamically imported module: /assets/doc-bisync-old.js"),
    );
    const { transport } = api();
    renderApp("/docs/bisync", transport);
    expect(await screen.findByText("This page needs a reload")).toBeInTheDocument();
    expect(
      screen.getByText("Syncr may have been updated since this tab was opened."),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Reload" })).toBeInTheDocument();
    // Still inside the docs, with their navigation.
    expect(screen.getByRole("navigation", { name: "Documentation" })).toBeInTheDocument();
  });

  it("points GitHub links at main for a development build", async () => {
    const { transport } = api({ version: "dev" });
    renderApp("/docs/upgrade", transport);
    expect(await title("Upgrading Syncr")).toBeInTheDocument();
    expect((await screen.findAllByText("Docs for a development build"))[0]).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /View on GitHub/ })).toHaveAttribute(
      "href",
      `${manifest.githubRepo}/blob/main/docs/upgrade.md`,
    );
  });
});

describe("docs addresses", () => {
  it("shows a not-found page inside the docs for an unknown page", async () => {
    const user = userEvent.setup();
    const { transport } = api();
    renderApp("/docs/no/such-page", transport);
    expect(await screen.findByText("No page at this address")).toBeInTheDocument();
    expect(screen.getByRole("navigation", { name: "Documentation" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "All documentation" })).toHaveAttribute(
      "href",
      "/docs",
    );
    await user.click(screen.getByRole("button", { name: "Search the docs" }));
    expect(await screen.findByRole("combobox", { name: "Search the docs" })).toBeInTheDocument();
  });

  it.each([
    ["/docs/restic.md", "/docs/restic"],
    ["/docs/releases/README.md", "/docs/releases"],
    ["/docs/README.md", "/docs"],
  ])("redirects %s to %s", async (from, to) => {
    const { transport } = api();
    const { router } = renderApp(from, transport);
    await waitFor(() => expect(router.state.location.pathname).toBe(to));
  });

  it("keeps the section when redirecting a GitHub path", async () => {
    const { transport } = api();
    const { router } = renderApp("/docs/bisync.md#conflicts", transport);
    await waitFor(() => expect(router.state.location.href).toBe("/docs/bisync#conflicts"));
  });
});

describe("docs pages", () => {
  it("follows links in a page without reloading, and focuses the new title", async () => {
    const user = userEvent.setup();
    const { transport } = api();
    const { router } = renderApp("/docs/restic", transport);
    await title("Restic backups");
    const nav = screen.getByRole("navigation", { name: "Documentation" });
    const link = document.querySelector<HTMLAnchorElement>('.doc-prose a[href="/docs/browsing"]');
    expect(link).not.toBeNull();
    await user.click(link!);
    expect(await title("Browsing files")).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/docs/browsing");
    // Same layout, no reload: the navigation is the same element.
    expect(screen.getByRole("navigation", { name: "Documentation" })).toBe(nav);
    await waitFor(() => expect(screen.getByRole("heading", { level: 1 })).toHaveFocus());
  });

  it("scrolls to and focuses the section in the address", async () => {
    const scroll = vi.spyOn(Element.prototype, "scrollIntoView");
    const { transport } = api();
    renderApp("/docs/restic#back-up-a-folder-of-a-remote", transport);
    await title("Restic backups");
    await waitFor(() => expect(document.activeElement?.id).toBe("back-up-a-folder-of-a-remote"));
    // focusSection's own scroll, which puts the heading under the sticky
    // header (the router also scrolls, with scrollIntoView(true)). jsdom has
    // no layout: keepInView's re-scroll after the fonts arrive is checked in
    // doc-navigation.test.ts and in a real browser.
    const section = document.activeElement;
    expect(
      scroll.mock.calls.filter(
        (args, i) => scroll.mock.contexts[i] === section && isEqual(args[0], { block: "start" }),
      ),
    ).toHaveLength(1);
  });

  it("moves to a section of the same page through its link", async () => {
    const user = userEvent.setup();
    const { transport } = api();
    const { router } = renderApp("/docs/bisync", transport);
    await title("Two-way sync");
    // A link in the text, not the heading's own link.
    const self = document.querySelector<HTMLAnchorElement>(
      '.doc-prose a[href="#conflicts"]:not(.doc-heading-link)',
    );
    expect(self).not.toBeNull();
    await user.click(self!);
    await waitFor(() => expect(router.state.location.hash).toBe("conflicts"));
    await waitFor(() => expect(document.activeElement?.id).toBe("conflicts"));
  });

  it("puts a copy button on every code block", async () => {
    const user = userEvent.setup();
    const { transport } = api();
    renderApp("/docs/setup-linux", transport);
    await title("Setting up a worker on Linux");
    const blocks = document.querySelectorAll(".doc-code");
    expect(blocks.length).toBeGreaterThan(1);
    await waitFor(() =>
      expect(document.querySelectorAll(".doc-code [data-copy-slot] button")).toHaveLength(
        blocks.length,
      ),
    );
    const second = blocks[1] as HTMLElement;
    await user.click(within(second).getByRole("button", { name: "Copy" }));
    expect(copyText).toHaveBeenCalledWith(second.querySelector("pre > code")?.textContent);
    expect(await within(second).findByText("Copied")).toBeInTheDocument();
  });

  it("does not let a click with a modifier key navigate in the app", async () => {
    const { transport } = api();
    const { router } = renderApp("/docs/restic", transport);
    await title("Restic backups");
    const link = document.querySelector<HTMLAnchorElement>('.doc-prose a[href="/docs/browsing"]')!;
    const event = new MouseEvent("click", { bubbles: true, cancelable: true, ctrlKey: true });
    link.addEventListener("click", (e) => e.preventDefault(), { once: true });
    fireEvent(link, event);
    expect(router.state.location.pathname).toBe("/docs/restic");
  });
});

describe("docs search", () => {
  it("opens with /, moves with the arrows and opens a result with Enter", async () => {
    const user = userEvent.setup();
    const { transport } = api();
    const { router } = renderApp("/docs/remotes", transport);
    await title("Remotes");
    await user.keyboard("/");
    const input = await screen.findByRole("combobox", { name: "Search the docs" });
    expect(input).toHaveFocus();
    await user.type(input, "worker upgrade");
    const options = await screen.findAllByRole("option");
    expect(input).toHaveAttribute("aria-expanded", "true");
    expect(input).toHaveAttribute("aria-activedescendant", options[0].id);
    expect(options[0]).toHaveAttribute("aria-selected", "true");

    await user.keyboard("{ArrowDown}");
    expect(input).toHaveAttribute("aria-activedescendant", options[1].id);
    await user.keyboard("{ArrowUp}{ArrowUp}");
    expect(input).toHaveAttribute("aria-activedescendant", options[options.length - 1].id);
    await user.keyboard("{ArrowDown}{ArrowDown}");

    const { default: raw } = await import("virtual:syncr-docs/search");
    const expected = searchDocs(prepareIndex(raw), "worker upgrade")[1];
    await user.keyboard("{Enter}");
    await waitFor(() => expect(router.state.location.pathname).toBe(`/docs/${expected.slug}`));
    expect(router.state.location.hash).toBe(expected.anchor);
    expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
  });

  it("opens with Ctrl+K and says when nothing matches", async () => {
    const user = userEvent.setup();
    const { transport } = api({ signedIn: true });
    renderApp("/docs", transport);
    await title("Syncr documentation");
    await user.keyboard("{Control>}k{/Control}");
    const input = await screen.findByRole("combobox", { name: "Search the docs" });
    await user.type(input, "qqqzzz");
    expect(await screen.findByText(/No results for/)).toHaveTextContent("No results for “qqqzzz”.");
    expect(input).toHaveAttribute("aria-expanded", "false");
    expect(input).not.toHaveAttribute("aria-activedescendant");
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("combobox")).not.toBeInTheDocument());
  });

  it("does not take / from a text field", async () => {
    const user = userEvent.setup();
    const { transport } = api();
    renderApp("/docs/remotes", transport);
    await title("Remotes");
    const field = document.createElement("input");
    document.body.append(field);
    field.focus();
    await user.keyboard("/");
    expect(field).toHaveValue("/");
    expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
    field.remove();
  });
});
