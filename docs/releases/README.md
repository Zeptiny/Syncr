# Releases

Each release has its notes here, as `<tag>.md` (for example
[v2.0.0.md](v2.0.0.md)); the release workflow publishes them as the GitHub
release's text. How releases are built, named and tagged:
[ADR 0016](../adr/0016-releases-versioning.md). How to upgrade:
[upgrading](../upgrade.md).

## Making a release

Tags are pushed by a maintainer, by hand; everything else is
`.github/workflows/release.yml`.

1. Write `docs/releases/vX.Y.Z.md` and merge it. A release candidate uses
   the notes of its version unless `docs/releases/vX.Y.Z-rc.N.md` exists.
2. Wait for CI to pass on the commit to tag, Windows included.
3. Tag the release candidate and push the tag:
   `git tag -a vX.Y.Z-rc.N -m 'Syncr vX.Y.Z-rc.N' && git push origin vX.Y.Z-rc.N`.
   The workflow publishes a GitHub pre-release with the six archives,
   `checksums.txt` and their build provenance, pushes the images tagged
   `X.Y.Z-rc.N`, and smoke-tests what it published.
4. **Once, after the very first release:** make the packages
   `syncr-controller` and `syncr-worker` public (GitHub, Zeptiny, Packages,
   the package, Package settings, Change visibility). GHCR creates them
   private. Until then, the images can only be pulled with a token.
5. Run the [release candidate checklist](#release-candidate-checklist)
   against the published pre-release. A problem means a fix and the next
   `rc.N`.
6. Tag `vX.Y.Z` on the commit to release (annotated, as above). The
   workflow publishes it as the latest release and moves the image tags
   `X.Y`, `X` and `latest`.
7. Check the result: the `releases/latest/download/...` URLs of the
   [Linux](../setup-linux.md) and [Windows](../setup-windows.md) guides
   resolve, the README's quick start works as written, and
   `syncr-controller -version` and `syncr-worker version` print the tag.

A failed workflow run can be re-run. If GoReleaser already created the
release, delete that release (not the tag) first.

## Release candidate checklist

CI builds, tests and installs a lot on every push, but not everything a
user does. These steps use the published release candidate, on real
machines.

- [ ] **Quick start** from the README in an empty directory, with
      `SYNCR_VERSION=X.Y.Z-rc.N` in `.env`: the controller starts, logs the
      setup code, and the first admin can be created.
- [ ] **Install commands from the UI**, each ending with an online worker:
  - [ ] Linux, system service.
  - [ ] Linux, `--user` (a systemd user unit; also after a logout and a
        login, and with `loginctl enable-linger` after a reboot).
  - [ ] Windows, service (elevated PowerShell).
  - [ ] Windows, `--user` (a Scheduled Task), then **log off and on
        again** and check that the worker comes back by itself. CI installs
        and starts the task on demand in the runner's logged-on session
        (`TestIntegrationRealUserTask`) but cannot log a user off and on,
        so the logon trigger itself is only checked here. If the CI log
        shows that test skipped (a runner without a desktop session), the
        whole Windows `--user` path is only checked here.
- [ ] Through the installed workers: an rclone transfer, a restic backup,
      browsing a snapshot, and a restore.
- [ ] A passive worker (the Passive tab's install command and registration).
- [ ] **Upgrade rehearsal with the real release assets**: on Linux and on
      Windows, a worker of `rc.N` upgraded with the command pasted from the
      worker's **Upgrade this worker** card, to the next tag (`rc.N+1`, or
      `vX.Y.Z` itself when there is no further candidate: then keep some
      workers on `rc.N` for this step after the final tag).
- [ ] `gh attestation verify syncr-worker_linux_amd64.tar.gz --owner Zeptiny`
      on a downloaded archive.
