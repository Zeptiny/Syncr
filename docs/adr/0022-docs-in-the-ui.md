# ADR 0022: Documentation in the web UI

- Status: accepted (v2.3.0)
- Date: 2026-11-21

## Context

The user documentation lives in `docs/` as markdown and was only readable
on GitHub, at whatever `main` said that day, while a controller runs one
release. People asked where the setup code is, what a remote type's fields
mean and what a two-way sync's safety check does, from screens that had no
way to point at the answer.

What shaped the design:

- **The controller is self-hosted and often offline from the internet.**
  The docs must come with the controller and describe the version it runs.
- **The UI is an embedded SPA** (ADR 0008) served by `spa.go` with a strict
  CSP: `script-src 'self'`, `img-src 'self' data:`, no inline scripts.
  Every path that is not a file or an API route already gets
  `index.html`, so `/docs/...` reaches the client router without a Go
  change, dots in `/docs/releases/v2.3.0` included.
- **The docs are written for GitHub:** relative links between pages,
  `#anchors` from GitHub's heading IDs, links into `adr/`, `deploy/` and
  `proto/`, and PNG screenshots. 98 anchors and about 200 links exist; none
  was checked by anything.
- **`SystemService.GetInfo` is public** and returns the version: a release
  tag (`v2.3.0`, `v2.3.0-rc.1`), `dev-<sha>` from snapshots, or
  `git describe` output and Go pseudo-versions from other builds.
- **The bundle is lazy per route.** `dist` was 2.1 MB; the docs are 268 KB
  of markdown and 1.4 MB of referenced screenshots.

## Decision

### Rendered when the UI is built, no parser in the browser

A Vite plugin (`web/docs-plugin/`) reads `../docs` with `marked` and
`github-slugger` (pinned devDependencies) and emits one lazy module per
page (`{title, html, headings}`), a manifest (navigation, reading order,
loaders) and a search index loaded on first use. Images become module
imports, so Vite emits them hashed under `assets/` (immutable caching,
same origin). `npm run generate:docs` writes `docs.gen.ts`: every page
and its heading IDs as TypeScript types.

### `docs/README.md` is the index

Its H1 is the title, each `##` a group holding one bullet list, each item
a link with an optional `: description` and one nested list. The order is
the reading order. Every `.md` file under `docs/` must be listed, except
the GitHub-only ones (`githubOnly` in `vite.config.ts`: `development.md`
and `adr/**`). URLs mirror the files: `docs/restic.md` is `/docs/restic`,
a folder's `README.md` is the folder, the index is `/docs`.

### GitHub-compatible heading IDs and checked links

Heading IDs are `github-slugger`'s, one slugger per page in document
order, so `-1`/`-2` suffixes match and a link written against GitHub
works in the app. Links to shipped pages become `/docs/...` routes; links
to GitHub-only files and to files outside `docs/` become GitHub URLs;
external links open in a new tab. The build fails, listing every problem as
`docs/<file>:<line>: <message>`, on a missing file, page or heading, a
link leaving the repository or using another scheme than https, http or
mailto, raw HTML, an image that is not a png, webp or svg inside `docs/`
or that has no alt text, a page without exactly one H1, a heading ID the
app uses, a GitHub alert, a broken index and a file missing from it. In
the Docker web stage, which holds only `web/` and `docs/`, the existence
of files outside `docs/` is not checked (`SYNCR_DOCS_REPO_LINKS=skip`);
CI's full-checkout jobs check it.

### Public routes, inside the app shell when signed in

`/docs` sits outside the signed-in layout. Signed out, a slim header with
**Sign in** frames it; signed in, the normal app shell does. The pages
contain nothing GitHub does not publish, and the only API calls are
`GetSession` (failing quietly) and the public `GetInfo`.

### Links pinned to the running version

GitHub links carry a placeholder that the viewer replaces with the running
version's tag when `GetInfo` returns a release (the same pattern as the
server's release check), and with `main` otherwise. The ref is validated
before it is inserted. The notes of the running release (`vX.Y.Z`, also for
its release candidates) drive the **What's new** notice: shown to
signed-in users until dismissed, stored per browser in
`localStorage["syncr-whats-new"]`, never for a development build.

### Typed help links

App pages link into the docs only through `DocLink`, `HelpLink` and
`docTarget` (`web/src/features/docs/doc-link.tsx`), typed by
`docs.gen.ts`: a page or anchor that does not exist fails the typecheck,
so renaming a heading the UI links to fails CI twice (the drift check,
then the typecheck). Maps from API IDs to sections (remote types,
notification channels) are data with checked targets, and an unknown ID
falls back to the page. Help links open a new tab, so a half-filled form
is never lost.

## Consequences

- The docs a controller shows always match its version, offline.
- Writing docs gets stricter: a new page needs its index entry, a renamed
  heading may need a UI change, and raw HTML is refused. The release
  runbook adds the notes to the index.
- `dist` and the controller grow by about 2 MB, two thirds of it the
  screenshots; the entry chunk grows by a few KB (routes and the typed
  titles), the pages and search load on demand.
- The rendered HTML is set with `dangerouslySetInnerHTML`. It comes from
  the repository at build time, with the trust of the JavaScript bundle:
  raw HTML is a build error, text and code are escaped, link schemes are
  allow-listed, and the only runtime substitution is the validated ref.
  The CSP is unchanged: copy buttons are React portals, images are
  same-origin.
- What's new is per browser: one user's dismissal hides it for everyone on
  a shared browser.

## Alternatives considered

- **A markdown parser in the browser** (fetching `.md` files at runtime):
  a larger bundle, link and anchor errors found by readers instead of the
  build, and no typed anchors for help links.
- **Compiling pages to React elements:** about twice the page payload, for
  no gain when no input comes from users.
- **markdown-it:** 2 MB and six dependencies against marked's one package
  with none; marked's token tree is enough for the checks.
- **Linking to GitHub instead of shipping the docs:** needs internet
  access, and `main` describes the next release, not the running one.
- **Docs behind sign-in:** the setup wizard and the sign-in page need them
  most, and they hold nothing private.
- **An index derived from file names or front matter:** a separate,
  ordered `docs/README.md` reads well on GitHub too and keeps the order in
  one place.
- **Help in a side panel:** keeps the reader on the page, but needs a
  section renderer of its own; a new tab is enough for now.
