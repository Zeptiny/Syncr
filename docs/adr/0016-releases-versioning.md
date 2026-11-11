# ADR 0016: Releases and versioning

- Status: accepted (unreleased until v2.0.0)
- Date: 2026-11-11

## Context

Syncr is installed in three ways, and each one names release files:

- The UI's install commands download `syncr-worker_<os>_<arch>.tar.gz`
  (`.zip` on Windows) and run `tar -xz syncr-worker` or `Expand-Archive`
  (`internal/adapters/api/install.go`).
- `syncr-worker upgrade` downloads the same archive and `checksums.txt`,
  verifies the archive's SHA-256 and extracts `syncr-worker`
  (`internal/worker/upgrade`; [upgrading](../upgrade.md)).
- The quick start runs the controller image from a compose file.

The controller compares versions too. A version that matches
`vX.Y.Z` or `vX.Y.Z-rc.N` is a *release version*: install commands then
download that release instead of the latest one, upgrade commands ask for
it, and a worker of an older release version shows as outdated. Release
versions are ordered as semantic versions, a release candidate before its
release (`v2.0.0-rc.1` < `v2.0.0-rc.2` < `v2.0.0` < `v2.0.1`); a worker
newer than the controller is not outdated, as its upgrade command would
downgrade it. Every other version (`dev`, a Go pseudo-version, a
snapshot's `dev-<commit>`) is a development build.

Before this, there were no releases, no published images and no `v*` tag.

## Decision

### One build definition: GoReleaser

`.goreleaser.yaml` (GoReleaser v2, the version pinned as
`GORELEASER_VERSION` in the Makefile and both workflows) builds both
binaries for linux/amd64, linux/arm64 and windows/amd64 with
`CGO_ENABLED=0`, `-trimpath`, the commit time as the files' modification
time, and the controller with `-tags webui` after building `web/`. CI builds
a snapshot of it on every push, so a release uses exactly the build CI
checked.

The three `-X` values of `internal/platform/version`:

| Variable | Release | Snapshot |
|---|---|---|
| `version` | the tag, with its `v` (`{{ .Tag }}`; `{{ .Version }}` drops it) | `dev-<short commit>` |
| `commit` | the full commit | the full commit |
| `buildTime` | the commit date | the commit date |

### Asset names are fixed and carry no version

| Asset | Content |
|---|---|
| `syncr-worker_linux_amd64.tar.gz`, `syncr-worker_linux_arm64.tar.gz` | `syncr-worker` at the root |
| `syncr-worker_windows_amd64.zip` | `syncr-worker.exe` at the root |
| `syncr-controller_linux_amd64.tar.gz`, `syncr-controller_linux_arm64.tar.gz` | `syncr-controller` at the root |
| `syncr-controller_windows_amd64.zip` | `syncr-controller.exe` at the root |
| `checksums.txt` | `<sha256>  <name>` for the six archives |

No wrapping directory and no other files. Because the names carry no
version, `releases/latest/download/<name>` and
`releases/download/<tag>/<name>` both work, and an install command only
changes its base URL. CI asserts the six names and the layout, and two
tests (`TestReleaseAssets` in `internal/adapters/api` and
`internal/worker/upgrade`) read the names from the code that downloads them
and check them against the snapshot. Renaming an asset breaks every
released worker's `upgrade`, so it would need a new name *in addition*.

### Tags and versions

- A release is a tag `vX.Y.Z`, or `vX.Y.Z-rc.N` for a release candidate,
  pushed on a commit CI passed. The release workflow refuses any other `v*`
  tag before it builds anything.
- A release candidate is published as a GitHub pre-release: it never
  becomes "latest", so `releases/latest/download` keeps serving the last
  final release.
- Release notes are hand-written: `docs/releases/<tag>.md`, or for a
  release candidate without its own file, the notes of its version with a
  line saying it is a release candidate. They are not generated from the
  commit log.
- The first release is `v2.0.0`, preceded by `v2.0.0-rc.1`. The Django
  version's last tag, `django-final`, is not a version of this project;
  GoReleaser ignores it.
- **No backports.** Releases are made from the newest line only: a fix
  goes into the next version, never into a patch release of an older
  minor or major version. So every final tag is the highest version
  released so far, and the workflow can mark each final release "latest"
  (GitHub's `make_latest`, which `releases/latest/download` follows) and
  move the image tags `X.Y`, `X` and `latest` without comparing versions.
  A release of an older line would move them backwards and make every new
  install, `docker compose pull` and `syncr-worker upgrade` without
  `--version` downgrade. Before anyone tags one, the workflow must first
  learn to compare the tag with the highest released version
  (`make_latest: false` and no `latest` image tag when it is lower).

### Images

`ghcr.io/zeptiny/syncr-controller` and `ghcr.io/zeptiny/syncr-worker`, for
linux/amd64 and linux/arm64, built from `deploy/Dockerfile.*` with the same
version, commit and commit date as the binaries.

| Tag pushed | Image tags |
|---|---|
| `v2.0.0` | `2.0.0`, `2.0`, `2`, `latest` |
| `v2.0.0-rc.1` | `2.0.0-rc.1` only |

The quick start's compose file uses `${SYNCR_VERSION:-2}`: the newest 2.x
release by default, an exact version when set. The worker image
redistributes rclone and restic and carries their licence texts.

### Provenance and verification

The release workflow attests the six archives and `checksums.txt` with
GitHub build provenance (`gh attestation verify <file> --owner Zeptiny`).
The checksum file comes from the same release as the archives: it catches a
corrupted or truncated download, not a compromised release. Signed releases
are not part of 2.0.

### Install commands follow the controller

A release build of the controller renders install commands for its own
release (`releases/download/<its tag>`) and upgrade commands with
`--version <its tag>`, unless `workers.download_base_url` names a mirror.
New workers therefore run the controller's version, and a release
candidate's commands install the release candidate. Development builds use
`releases/latest/download`. The setup guides use the `latest` URLs, plus a
checksum step.

### What is frozen at a tag

From `v2.0.0` on:

- **Migrations** `00001`–`00009` never change; a schema change is a new
  numbered migration ([ADR 0004](0004-postgres-store.md)).
- **Protos:** fields are added, never renumbered or removed. Workers and
  controllers of different versions talk over the link, and link features
  (`internal/link/features.go`) gate what an older worker cannot do.
- **Asset names and archive layout** (above), and the `syncr-worker upgrade`
  flags the UI renders.
- **Image names and the tag scheme.**
- **The command-line surface a released upgrader relies on in later
  binaries.** `syncr-worker upgrade` of one release runs the binary of the
  *next* release before installing it, and that release's worker then
  starts next to the files the upgrader keeps. So every later
  `syncr-worker` keeps:
  - **`version`:** prints a line whose first two whitespace-separated
    fields are `syncr-worker` and the version exactly as tagged
    (`syncr-worker v2.1.0 (commit ..., built ..., go...) linux/amd64`);
    the upgrader compares that version with `--version` and the installed
    one.
  - **`tools fetch --dir <dir>`:** installs the pinned tools into `<dir>`,
    exits 0 once they are all there, and prints one line per tool with the
    whitespace-separated columns `<tool> <version> <os>/<arch>
    <installed|up-to-date> <path>`, the path last. The upgrader reads the
    first, second and fourth columns to know which tool versions to keep
    when it prunes (and prunes nothing if it cannot read them).
  - **`tools status --dir <dir>`:** exits 0 if and only if every pinned
    tool resolves, and takes the configured binaries from
    `SYNCR_<TOOL>_PATH` (`SYNCR_RCLONE_PATH`, `SYNCR_RESTIC_PATH`), which
    the upgrader sets from the worker's `tools.rclone_path` and
    `tools.restic_path`.
  - **The files next to the installed binary,** and what a starting worker
    may delete: `<binary>.old` (the previous version, kept until the
    upgrade succeeded and put back by a rollback), the temporary files
    `.syncr-worker-download-*` and `.syncr-worker-new-*`, and
    `.syncr-worker-upgrade.lock`, which an upgrade holds from its start to
    its end. A starting worker deletes nothing while another process holds
    that lock. Otherwise it may delete the temporary files once they are a
    day old and, on Windows only, `<binary>.old` (a successful upgrade
    cannot delete it there, as it runs from that file). On every other
    system it never deletes `<binary>.old`: the upgrade does, once it
    succeeded.

### The release flow

1. CI passes on the commit (Windows included).
2. Tag `vX.Y.Z-rc.N`; the workflow publishes the pre-release and its
   images; the GHCR packages are made public once, by hand, after the
   first push.
3. The release candidate checklist (`docs/releases/README.md`) runs
   against the published release; problems lead to `rc.N+1`.
4. Tag `vX.Y.Z`; the workflow publishes the release as latest and moves
   `X.Y`, `X` and `latest`.

The workflow's own smoke job checks what it published: the release's
pre-release flag, the documented Linux install command, `checksums.txt`,
both images' versions and platforms, and for a final release that `X.Y`,
`X` and `latest` point at the new images.

## Consequences

- One definition builds CI snapshots and releases; an archive layout that
  would break `tar -xz syncr-worker` or `syncr-worker upgrade` fails CI on
  the pull request that causes it.
- A tag is all a release needs; nothing is uploaded by hand.
- The first push of each image creates a private GHCR package. Until it is
  made public, `docker compose pull` fails for everyone else.
- Release candidates are real, installable releases. Their images are
  never `latest`, and their binaries are never `releases/latest`.
- Dependabot (`.github/dependabot.yml`) proposes updates against `main`;
  the actions are pinned to commit SHAs.

## Alternatives considered

- **Versions in asset names** (GoReleaser's default
  `syncr_2.0.0_linux_amd64.tar.gz`): `releases/latest/download` could not
  name the file, so every install command would need the version.
- **`{{ .Version }}` in the binaries:** `2.0.0` instead of `v2.0.0`, which the
  controller would not recognize as a release version.
- **Release notes from the commit log:** v2's history is one squash commit
  per milestone; the notes would say nothing useful.
- **Docker Hub:** needs credentials in the repository; GHCR works with the
  workflow token.
- **Building the images with GoReleaser:** two build definitions for the
  images (the Dockerfiles are also used from source); the Dockerfiles stay
  the single definition, built by `docker/build-push-action`.
