import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { renderInRouter } from "@/test/render";

import { DocLink, docHref, docTarget, HelpLink, isDocPage } from "./doc-link";

describe("docHref and docTarget", () => {
  it("builds app URLs from checked targets", () => {
    expect(docHref(docTarget(""))).toBe("/docs");
    expect(docHref(docTarget("remotes", "sftp"))).toBe("/docs/remotes#sftp");
    expect(docHref(docTarget("releases/v2.3.0"))).toBe("/docs/releases/v2.3.0");
    expect(docTarget("restic")).toEqual({ page: "restic" });
    expect(isDocPage("restic")).toBe(true);
    expect(isDocPage("development")).toBe(false);
    expect(isDocPage("toString")).toBe(false);
  });

  it("rejects unknown pages and anchors at compile time", () => {
    // Never called: the checks are the @ts-expect-error lines.
    const typeChecks = () => [
      // @ts-expect-error not a shipped page
      docTarget("development"),
      // @ts-expect-error not a heading of remotes
      docTarget("remotes", "no-such-heading"),
      // @ts-expect-error configuration has no headings
      docTarget("configuration", "anything"),
      // @ts-expect-error an anchor of another page
      <DocLink page="hooks" anchor="sftp">
        x
      </DocLink>,
      // @ts-expect-error not a shipped page
      <HelpLink page="adr/0018-two-way-sync" />,
    ];
    expect(typeof typeChecks).toBe("function");
  });
});

describe("DocLink", () => {
  it("links pages, sections and nested pages", async () => {
    renderInRouter(
      <>
        <DocLink page="">Index</DocLink>
        <DocLink page="remotes" anchor="sftp">
          SFTP
        </DocLink>
        <DocLink page="releases/v2.3.0">Notes</DocLink>
      </>,
    );
    expect(await screen.findByRole("link", { name: "Index" })).toHaveAttribute("href", "/docs");
    expect(screen.getByRole("link", { name: "SFTP" })).toHaveAttribute(
      "href",
      "/docs/remotes#sftp",
    );
    expect(screen.getByRole("link", { name: "Notes" })).toHaveAttribute(
      "href",
      "/docs/releases/v2.3.0",
    );
    expect(screen.getByRole("link", { name: "Notes" })).not.toHaveAttribute("target");
  });

  it("opens a new tab on request and says so", async () => {
    renderInRouter(
      <DocLink page="hooks" newTab>
        Hooks
      </DocLink>,
    );
    const link = await screen.findByRole("link", { name: /^Hooks\s*\(opens in a new tab\)$/ });
    expect(link).toHaveAttribute("target", "_blank");
    expect(link).toHaveAttribute("rel", "noopener noreferrer");
  });
});

describe("HelpLink", () => {
  it("is a Help button that opens the section in a new tab", async () => {
    renderInRouter(<HelpLink page="bisync" anchor="conflicts" />);
    const link = await screen.findByRole("link", {
      name: "Help: Two-way sync (opens in a new tab)",
    });
    expect(link).toHaveTextContent("Help");
    expect(link).toHaveAttribute("href", "/docs/bisync#conflicts");
    expect(link).toHaveAttribute("target", "_blank");
  });

  it("takes another label", async () => {
    renderInRouter(<HelpLink page="setup-linux" label="Linux guide" />);
    expect(
      await screen.findByRole("link", {
        name: "Linux guide: Setting up a worker on Linux (opens in a new tab)",
      }),
    ).toHaveTextContent("Linux guide");
  });
});
