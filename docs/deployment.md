# Running the controller

The [quick start](../README.md#quick-start) gets a controller running with
Docker Compose. This page covers the rest: versions, HTTPS, backups,
upgrades and the container images. Every setting is listed in
[configuration](configuration.md).

## Versions and the compose file

The compose file runs `ghcr.io/zeptiny/syncr-controller:2`, the newest 2.x
release; `SYNCR_VERSION` in `.env` pins another tag, for example `2.0.0`.
To try a release candidate, take its files (`v=v2.0.0-rc.1`) and set
`SYNCR_VERSION=2.0.0-rc.1`. From a checkout, use
`docker compose -f deploy/docker-compose.yml ...` with `deploy/.env`, and
add `-f deploy/docker-compose.build.yml` to build the images from source.

## HTTPS and reverse proxies

The controller serves the web UI and the API with plain HTTP on port 8080.
Browsers should, and workers on other machines must, reach it over HTTPS:
put a TLS-terminating reverse proxy in front of it (read/idle timeout above
40 s, request bodies up to 16 MiB) and set `SYNCR_PUBLIC_URL` to its
`https://` address, or let the controller serve TLS itself
(`SYNCR_HTTP_TLS_CERT_FILE`, `SYNCR_HTTP_TLS_KEY_FILE`; see the comments in
[`deploy/docker-compose.yml`](../deploy/docker-compose.yml)). Behind a proxy,
also set `SYNCR_HTTP_CLIENT_IP_HEADER` and `SYNCR_HTTP_TRUSTED_PROXIES` so
the login and enrollment rate limits see clients, not the proxy.

## Backups

Back up the Postgres database (everything Syncr stores) and
`SYNCR_SECRETS_KEY` (it encrypts the stored credentials of remotes,
repositories and workers: a database backup without it cannot be used).
With the compose file:

```sh
docker compose exec -T postgres pg_dump -U syncr -d syncr -Fc > syncr-$(date +%F).dump
```

Workers keep their enrollment in their config file and can be set up again
from the UI; the backups themselves live in your restic repositories, whose
passwords you keep a copy of.

## Upgrading

Back up, upgrade the controller (`docker compose pull &&
docker compose up -d --wait`; it migrates the database at start), then each
worker with the command its page shows (`syncr-worker upgrade`, which puts
the old version back if the new one does not start). A worker older than
the controller shows as **Outdated**. Details, the manual steps and
containers: [upgrading](upgrade.md).

## One controller per database

A second controller started against the same database waits, logging
"another controller is running against this database", and takes over when
the first one stops, or about a minute after the first one's host crashed
(the log names the old session and how to end it at once,
`SELECT pg_terminate_backend(<pid>)`;
[ADR 0015](adr/0015-single-controller-guard.md)). Run one controller; a
standby is fine.

## Images

Releases publish `ghcr.io/zeptiny/syncr-controller` and
`ghcr.io/zeptiny/syncr-worker` for linux/amd64 and linux/arm64, tagged
`2.0.0`, `2.0`, `2` and `latest` (a release candidate only with its own
tag, such as `2.0.0-rc.1`). To build them from source:

```sh
docker build -f deploy/Dockerfile.controller -t syncr-controller .   # web UI embedded, distroless, non-root, HEALTHCHECK via -healthcheck
docker build -f deploy/Dockerfile.worker -t syncr-worker .           # Debian slim, rclone v1.75.2, restic 0.19.1 (with their licences), non-root
```

[`deploy/docker-compose.yml`](../deploy/docker-compose.yml) runs the
controller with Postgres;
[`deploy/docker-compose.build.yml`](../deploy/docker-compose.build.yml)
builds the images from the checkout instead. The compose file has an
optional, commented worker service that enrolls on first start and keeps its
configuration in a volume. The worker image's default command is
`run --config /var/lib/syncr/worker.yaml`; enroll it once with
`setup --no-service --config /var/lib/syncr/worker.yaml ...`. A worker in a
container is upgraded by pulling its new image.

Release files and how releases are versioned:
[ADR 0016](adr/0016-releases-versioning.md).
