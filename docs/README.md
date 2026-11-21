# Syncr documentation

How to run and use Syncr. The controller's web UI shows these same pages
under **Docs**, for the version it runs. They are listed below in reading
order.

## Get started

- [Running the controller](deployment.md): Docker Compose, the first start, versions, HTTPS, backups, upgrades and the images
- [Configuration](configuration.md): every controller setting
- [A worker on Linux](setup-linux.md): install, user mode, passive workers, troubleshooting
- [A worker on Windows](setup-windows.md): the same on Windows
- [Worker configuration](worker-config.md): `worker.yaml`, local folders, tools, hooks
- [Upgrading](upgrade.md): the controller and every worker

## Using Syncr

- [Remotes](remotes.md): every storage type and what to enter
- [Encrypted remotes](encrypted-remotes.md): rclone crypt over another remote
- [Browsing files](browsing.md): the file pages, copy and move, folder sizes
- [Two-way sync](bisync.md): keeping two folders in step
- [Restic backups](restic.md): repositories, backups (of worker folders and of remotes), snapshots and restores
- [Hooks](hooks.md): commands around jobs
- [Notifications](notifications.md): Discord, email and webhooks

## Reference

- [Scripting the API](api.md): API tokens and examples
- [Metrics for Prometheus](metrics.md): `/metrics`, scraping with an API token, every metric and example alerts
- [Release notes](releases/README.md)
  - [2.3.0](releases/v2.3.0.md)
  - [2.2.0](releases/v2.2.0.md)
  - [2.1.0](releases/v2.1.0.md)
  - [2.0.0](releases/v2.0.0.md)

## For contributors (on GitHub)

- [Development](development.md): building, testing and the architecture
- [Architecture decisions](adr/)
