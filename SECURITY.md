# Security policy

## Reporting a vulnerability

Please report vulnerabilities **privately**, through GitHub:
[Report a vulnerability](https://github.com/Zeptiny/Syncr/security/advisories/new)
(the repository's **Security** tab, **Report a vulnerability**). Do not
open a public issue, discussion or pull request for it.

Useful to include: the version (`syncr-controller -version`,
`syncr-worker version`), how it is deployed (compose, binary, worker as a
service, per user or in a container), the steps to reproduce, and what an
attacker gains. Leave out real credentials, repository passwords and
`SYNCR_SECRETS_KEY`.

The report stays private between you and the maintainer while it is looked
at and fixed. Once a fixed release is out, the advisory is published, with
credit to you unless you prefer otherwise.

## Supported versions

| Version | Security fixes |
|---|---|
| 2.x, newest minor release | yes |
| older 2.x minor releases | no: upgrade ([upgrading](docs/upgrade.md)) |
| release candidates (`-rc.N`) | no, once the release is out |
| the Django version (tag `django-final`) | no |

Fixes come as patch releases. Upgrade the controller first, then each
worker with `syncr-worker upgrade`.

## Verifying a release

Every release lists the SHA-256 of its archives in `checksums.txt`, and the
archives and `checksums.txt` carry GitHub build provenance:

```sh
sha256sum --ignore-missing -c checksums.txt
gh attestation verify syncr-worker_linux_amd64.tar.gz --owner Zeptiny
```

`syncr-worker upgrade` always checks the archive against `checksums.txt`.
That catches a damaged download, not a compromised release; releases are
not signed yet.

## Scope

In scope: the controller (API, web UI, worker link, stored secrets),
the worker (its link, the jobs and probes it runs, `local_roots`, setup and
upgrade), the release files and the images.

The design records in [docs/adr/](docs/adr/) describe the security
decisions: users and sessions (0006), how remote credentials are stored and
handed to workers (0007), the worker link and passive workers' pinned TLS
(0002), worker probes (0012), releases (0016) and the commands workers run
for jobs as hooks (0017). Behaviour they describe as intended (for example
that an administrator can see and cancel every user's jobs, that fetched
SFTP host keys are trusted on first use, or that every user can run each
hook a worker's operator defined) is not a vulnerability, but a report that
it is unsafe is welcome.
