import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import type { CompareSnapshotsRequest, FindInRepositoryRequest } from "@/gen/syncr/v1/restic_pb";
import * as m5 from "@/mock/m5";
import { m5Api } from "@/test/m5-api";
import { renderApp } from "@/test/render";

const now = Date.UTC(2026, 10, 10, 12);
const vault = m5.ids.photosVault;
const short = (id: string) => id.slice(0, 8);

describe("search", { timeout: 30_000 }, () => {
  it("groups matches by path and opens into versions, each opened or restored", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    const snaps = api.repo(vault).snapshots;
    const { router } = renderApp(`/repositories/${vault}/search?q=albums.json`, api.transport);
    const results = await screen.findByRole("region", { name: "Search results" });
    expect(
      within(results).getByText(/^30 matches, 1 path\. 30 of 30 snapshots searched\.$/),
    ).toBeInTheDocument();
    expect(api.of<FindInRepositoryRequest>("FindInRepository")).toMatchObject([
      { repositoryId: vault, patterns: ["albums.json"], ignoreCase: true, snapshotIds: [] },
    ]);
    const group = within(results).getByRole("button", {
      name: /\/srv\/photos\/2026\/albums\.json/,
    });
    expect(group).toHaveTextContent("In 30 snapshots, 9 versions · newest 18.0 KiB");
    expect(group).toHaveAttribute("aria-expanded", "false");
    await user.click(group);
    expect(group).toHaveAttribute("aria-expanded", "true");
    const versions = within(results).getAllByRole("list", { name: "Versions" })[0];
    const items = within(versions)
      .getAllByRole("listitem")
      .filter((li) => li.parentElement === versions);
    expect(items).toHaveLength(9);
    expect(items[0]).toHaveTextContent(/^Newest version18\.0 KiB, modified .*In 1 snapshot/);
    expect(items[1]).toHaveTextContent(/^Version 817\.3 KiB.*In 4 snapshots/);

    // Restore the second version from the newest snapshot holding it.
    await user.click(
      within(items[1]).getByRole("button", {
        name: `Restore… from snapshot ${short(snaps[1].id)}`,
      }),
    );
    const sheet = await screen.findByRole("dialog", {
      name: `Restore from snapshot ${short(snaps[1].id)}`,
    });
    expect(within(sheet).getByText("1 item")).toBeInTheDocument();
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

    await user.click(
      within(items[0]).getByRole("button", {
        name: `Open folder in snapshot ${short(snaps[0].id)}`,
      }),
    );
    await waitFor(() =>
      expect(router.state.location.pathname).toBe(
        `/repositories/${vault}/snapshots/${snaps[0].id}`,
      ),
    );
    expect(router.state.location.search).toEqual({ path: "/srv/photos/2026" });
    expect(await screen.findByText("albums.json")).toBeInTheDocument();
  });

  it("says when the search was cut at its most matches", async () => {
    const api = m5Api(now);
    renderApp(`/repositories/${vault}/search?q=*.jpg`, api.transport);
    const results = await screen.findByRole("region", { name: "Search results" });
    expect(
      within(results).getByText(
        /^1,000 matches, \d+ different paths\. 5 of 30 snapshots searched\.$/,
      ),
    ).toBeInTheDocument();
    expect(within(results).getByText(/stopped at its most matches/)).toBeInTheDocument();
    expect(within(results).getByRole("button", { name: /^Show 100 more$/ })).toBeInTheDocument();
  });

  it("searches one snapshot, with case, from the form", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    const snaps = api.repo(vault).snapshots;
    const { router } = renderApp(`/repositories/${vault}/search`, api.transport);
    const box = await screen.findByRole("textbox", { name: "Pattern" });
    expect(box).toHaveAccessibleDescription(/matches a file or folder of that name anywhere/);
    expect(api.of("FindInRepository")).toHaveLength(0);
    await user.click(screen.getByRole("switch", { name: "Ignore case" }));
    await user.click(screen.getByRole("button", { name: "Snapshots searched: All snapshots" }));
    const when = within(await screen.findByRole("menu")).getAllByRole("menuitemradio");
    await user.click(when[2]); // All snapshots, then the newest: the second newest
    await user.type(box, "README.TXT{Enter}");
    await waitFor(() =>
      expect(router.state.location.search).toEqual({
        q: "README.TXT",
        icase: false,
        snapshot: snaps[1].id,
      }),
    );
    await waitFor(() =>
      expect(api.of<FindInRepositoryRequest>("FindInRepository").at(-1)).toMatchObject({
        patterns: ["README.TXT"],
        ignoreCase: false,
        snapshotIds: [snaps[1].id],
      }),
    );
    expect(await screen.findByText("Nothing matches")).toBeInTheDocument();
    expect(screen.getByText(/^No matches\. 1 of 1 snapshot/)).toBeInTheDocument();
  });

  it("shows a bad pattern at the box", async () => {
    renderApp(`/repositories/${vault}/search?q=${"é".repeat(200)}`, m5Api(now).transport);
    expect(await screen.findByRole("alert")).toHaveTextContent("Must be at most 256 bytes.");
    expect(screen.getByRole("textbox", { name: "Pattern" })).toHaveAttribute(
      "aria-invalid",
      "true",
    );
  });

  it("is reached from the repository's page and the snapshot browser", async () => {
    const api = m5Api(now);
    const view = renderApp(`/repositories/${vault}`, api.transport);
    expect(await screen.findByRole("link", { name: "Search" })).toHaveAttribute(
      "href",
      `/repositories/${vault}/search`,
    );
    view.unmount();
    const snaps = api.repo(vault).snapshots;
    renderApp(`/repositories/${vault}/snapshots/${snaps[0].id}`, api.transport);
    expect(await screen.findByRole("link", { name: "Search" })).toHaveAttribute(
      "href",
      `/repositories/${vault}/search`,
    );
  });
});

describe("file versions", { timeout: 30_000 }, () => {
  function browse(path: string) {
    const api = m5Api(now);
    const snaps = api.repo(m5.ids.nasDocs).snapshots;
    const view = renderApp(
      `/repositories/${m5.ids.nasDocs}/snapshots/${snaps[0].id}?path=${encodeURIComponent(path)}`,
      api.transport,
    );
    return { api, snaps, user: userEvent.setup(), ...view };
  }

  it("searches every snapshot for the exact path and lists its versions", async () => {
    const { api, snaps, user, router } = browse("/srv/docs");
    const grid = await screen.findByRole("grid", { name: "Contents of /srv/docs" });
    await user.click(await within(grid).findByRole("button", { name: "Actions for notes.md" }));
    const menu = await screen.findByRole("menu");
    expect(
      within(menu)
        .getAllByRole("menuitem")
        .map((m) => m.textContent),
    ).toEqual(["Restore…", "Restore to original location…", "Show versions", "Copy path"]);
    await user.click(within(menu).getByRole("menuitem", { name: "Show versions" }));
    const sheet = await screen.findByRole("dialog", { name: "Versions of notes.md" });
    expect(
      await within(sheet).findByText(
        /^6 versions in 14 snapshots\. 14 of 14 snapshots searched\.$/,
      ),
    ).toBeInTheDocument();
    expect(api.of<FindInRepositoryRequest>("FindInRepository")).toMatchObject([
      { patterns: ["/srv/docs/notes.md"], ignoreCase: false, snapshotIds: [] },
    ]);
    const versions = within(sheet).getByRole("list", { name: "Versions" });
    const items = within(versions)
      .getAllByRole("listitem")
      .filter((li) => li.parentElement === versions);
    expect(items).toHaveLength(6);

    // Restoring an older version restores from its snapshot.
    const older = within(items[1]).getByRole("button", { name: /^Restore… from snapshot / });
    const id = older.getAttribute("aria-label")!.slice(-8);
    expect(id).not.toBe(short(snaps[0].id));
    await user.click(older);
    expect(
      await screen.findByRole("dialog", { name: `Restore from snapshot ${id}` }),
    ).toBeInTheDocument();
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

    // Open the folder in another snapshot.
    await user.click(within(grid).getByRole("button", { name: "Actions for notes.md" }));
    await user.click(await screen.findByRole("menuitem", { name: "Show versions" }));
    const again = await screen.findByRole("dialog", { name: "Versions of notes.md" });
    const last = within(again)
      .getAllByRole("button", { name: /^Open folder in snapshot / })
      .at(-1)!;
    const lastId = last.getAttribute("aria-label")!.slice(-8);
    await user.click(last);
    await waitFor(() =>
      expect(router.state.location.pathname).toMatch(new RegExp(`/snapshots/${lastId}`)),
    );
    expect(router.state.location.search).toEqual({ path: "/srv/docs" });
    // Cached: the same search is not run again.
    expect(api.of("FindInRepository")).toHaveLength(1);
  });

  it("is offered for files, not folders", async () => {
    const { api, user } = browse("/srv/docs");
    const grid = await screen.findByRole("grid", { name: "Contents of /srv/docs" });
    await user.click(await within(grid).findByRole("button", { name: "Actions for contracts" }));
    const menu = await screen.findByRole("menu");
    expect(
      within(menu)
        .getAllByRole("menuitem")
        .map((m) => m.textContent),
    ).toEqual(["Restore…", "Restore to original location…", "Copy path"]);
    expect(api.of("FindInRepository")).toHaveLength(0);
  });
});

describe("compare", { timeout: 30_000 }, () => {
  it("is offered on every snapshot with a previous one", async () => {
    const api = m5Api(now);
    const snaps = api.repo(vault).snapshots;
    renderApp(`/repositories/${vault}`, api.transport);
    const link = await screen.findByRole("link", {
      name: `Compare ${short(snaps[0].id)} with previous`,
    });
    expect(link).toHaveAttribute(
      "href",
      `/repositories/${vault}/compare?from=${snaps[1].id}&to=${snaps[0].id}`,
    );
    // The oldest has none.
    expect(
      screen.queryByRole("link", { name: `Compare ${short(snaps.at(-1)!.id)} with previous` }),
    ).toBeNull();
  });

  it("shows the totals, then the changes by kind and text, each opening its folder", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    const snaps = api.repo(vault).snapshots;
    const { router } = renderApp(
      `/repositories/${vault}/compare?from=${snaps[1].id}&to=${snaps[0].id}`,
      api.transport,
    );
    const changes = await screen.findByRole("list", { name: "Changes" });
    expect(api.of<CompareSnapshotsRequest>("CompareSnapshots")).toMatchObject([
      { fromSnapshotId: snaps[1].id, toSnapshotId: snaps[0].id, path: "" },
    ]);
    const stat = (label: string) => screen.getByText(label).parentElement!;
    expect(stat("Files added")).toHaveTextContent("3and 0 folders");
    expect(stat("Files removed")).toHaveTextContent("1and 0 folders");
    expect(stat("Files changed")).toHaveTextContent("10");
    expect(within(changes).getAllByRole("listitem")).toHaveLength(15);

    // Only the kinds that occur: restic without --metadata never reports "Metadata only".
    const kinds = screen.getByRole("group", { name: "Kinds of change" });
    expect(
      within(kinds)
        .getAllByRole("button")
        .map((b) => b.textContent),
    ).toEqual(["Added 3", "Removed 1", "Changed 10", "Type changed 1"]);
    await user.click(within(kinds).getByRole("button", { name: "Changed 10" }));
    expect(within(kinds).getByRole("button", { name: "Changed 10" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(within(changes).getAllByRole("listitem")).toHaveLength(10);
    await user.type(screen.getByPlaceholderText("Filter paths"), "albums");
    await waitFor(() => expect(within(changes).getAllByRole("listitem")).toHaveLength(1));
    await user.click(within(kinds).getByRole("button", { name: "Changed 10" }));
    await user.clear(screen.getByPlaceholderText("Filter paths"));
    await waitFor(() => expect(within(changes).getAllByRole("listitem")).toHaveLength(15));

    // A removed file opens in the older snapshot.
    const removed = within(changes)
      .getByText("/srv/photos/2025/01/IMG_1037 (1).jpg")
      .closest("li")!;
    await user.click(within(removed).getByRole("button", { name: /in the older snapshot$/ }));
    await waitFor(() =>
      expect(router.state.location.pathname).toBe(
        `/repositories/${vault}/snapshots/${snaps[1].id}`,
      ),
    );
    expect(router.state.location.search).toEqual({ path: "/srv/photos/2025/01" });
  });

  it("says when it stopped early, without totals", async () => {
    const api = m5Api(now);
    const snaps = api.repo(vault).snapshots;
    renderApp(
      `/repositories/${vault}/compare?from=${snaps[29].id}&to=${snaps[0].id}`,
      api.transport,
    );
    expect(
      await screen.findByText(/stopped at the time limit, so there are no totals/),
    ).toBeInTheDocument();
    expect(screen.queryByText("Files added")).toBeNull();
    expect(await screen.findByRole("list", { name: "Changes" })).toBeInTheDocument();
  });

  it("proposes the previous snapshot when only the newer one is given", async () => {
    const api = m5Api(now);
    const snaps = api.repo(vault).snapshots;
    renderApp(`/repositories/${vault}/compare?to=${snaps[0].id}`, api.transport);
    await screen.findByRole("list", { name: "Changes" });
    expect(api.of<CompareSnapshotsRequest>("CompareSnapshots")[0]).toMatchObject({
      fromSnapshotId: snaps[1].id,
      toSnapshotId: snaps[0].id,
    });
  });

  it("compares the open folder with another snapshot from the browser", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    const snaps = api.repo(vault).snapshots;
    const { router } = renderApp(
      `/repositories/${vault}/snapshots/${snaps[0].id}?path=%2Fsrv%2Fphotos%2F2026`,
      api.transport,
    );
    await screen.findByRole("grid", { name: "Contents of /srv/photos/2026" });
    await user.click(screen.getByRole("button", { name: "Compare with…" }));
    const items = within(await screen.findByRole("menu")).getAllByRole("menuitemradio");
    await user.click(items[2]); // the third newest after this one
    await waitFor(() =>
      expect(router.state.location.pathname).toBe(`/repositories/${vault}/compare`),
    );
    expect(router.state.location.search).toEqual({
      from: snaps[3].id,
      to: snaps[0].id,
      path: "/srv/photos/2026",
    });
    expect(await screen.findByRole("list", { name: "Changes" })).toBeInTheDocument();
    expect(api.of<CompareSnapshotsRequest>("CompareSnapshots").at(-1)).toMatchObject({
      path: "/srv/photos/2026",
    });
  });
});
