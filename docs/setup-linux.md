# Setting up a worker on Linux

A worker is one binary plus a service. `syncr-worker setup` does everything from
a clean machine: it provisions the pinned `rclone` and `restic` (verified
download), enrolls the worker with the controller, writes the config, installs
and starts a systemd unit, and checks that it runs.

Requirements: Linux with systemd, root (or `--user`, below), and HTTPS access
to the controller and to `github.com` (for the tool downloads).

## Quick path (one command)

In the controller's web UI open **Workers, Add worker**. The **Active** tab
creates a single-use enrollment token (it expires, and works once) and shows
the install command for each platform and scope. For a Linux system service
it looks like this:

```sh
curl -fsSL "https://github.com/Zeptiny/Syncr/releases/latest/download/syncr-worker_linux_$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/').tar.gz" | tar -xz syncr-worker \
  && sudo install -m 0755 syncr-worker /usr/local/bin/syncr-worker \
  && sudo /usr/local/bin/syncr-worker setup --controller 'https://syncr.example.com' --token 'syncr_et_...'
```

It downloads the release archive, installs the binary where the service
runs it, and runs setup. The UI's command downloads the controller's own
release (`.../releases/download/v2.0.0/...` for a v2.0.0 controller), so a
new worker runs the controller's version; the `latest` URL above, which a
development build of the controller shows, installs the newest release.
Release candidates (such as `v2.0.0-rc.1`) are GitHub pre-releases, which
`latest` never points to: a release candidate controller's command
downloads its own release as above, and to install one by hand, use its tag
(`.../releases/download/v2.0.0-rc.1/...`). If the controller sets `workers.download_base_url` (empty by default), the
command downloads from that mirror instead (a value of
`https://github.com/Zeptiny/Syncr/releases/latest/download`, the default of
builds before v2.0.0, counts as unset; the controller warns at start that it
should be removed). The controller URL is its `http.public_url`, or the
address your browser uses when that is unset. If `syncr-worker` is already
installed, run only the last line:

```sh
# put the binary where it will live; the service runs exactly this path
sudo install -m 0755 syncr-worker /usr/local/bin/syncr-worker
sudo syncr-worker setup --controller https://syncr.example.com --token syncr_et_...
```

The page that showed the command follows the worker: it shows when the
command has run and the worker enrolled, then, once it has connected, its
hostname, versions and allowed folders (`local_roots`), with a link to its
page. An empty list of allowed folders is flagged there: jobs could not
touch the machine's own files.

### Verifying a download

The command trusts HTTPS to GitHub. To install by hand and check the
archive against the release's `checksums.txt` first (and, optionally, its
GitHub build provenance):

```sh
base=https://github.com/Zeptiny/Syncr/releases/latest/download   # or releases/download/<tag>, e.g. v2.0.0 or v2.0.0-rc.1
arch=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
curl -fsSLO "$base/checksums.txt"
curl -fsSLO "$base/syncr-worker_linux_$arch.tar.gz"
sha256sum --ignore-missing -c checksums.txt     # must print: syncr-worker_linux_<arch>.tar.gz: OK
gh attestation verify "syncr-worker_linux_$arch.tar.gz" --owner Zeptiny   # optional, needs the GitHub CLI
tar -xzf "syncr-worker_linux_$arch.tar.gz" syncr-worker
sudo install -m 0755 syncr-worker /usr/local/bin/syncr-worker
syncr-worker version
```

Then run setup as above.

Sample output:

```
Syncr worker setup: active worker, system scope

==> Preparing directories
==> Provisioning rclone and restic (pinned versions, checksum-verified)
    rclone 1.75.2 (managed) /var/lib/syncr/tools/rclone/1.75.2/linux-amd64/rclone
    restic 0.19.1 (managed) /var/lib/syncr/tools/restic/0.19.1/linux-amd64/restic
==> Enrolling with https://syncr.example.com
    enrolled as "myhost", worker ID 0b6f8f0e-52a5-4a3f-9a5d-2f2a7c9f1e11
==> Writing the configuration
    /etc/syncr/worker.yaml (private to the worker)
==> Installing the service (systemd system unit)
    syncr-worker: /etc/systemd/system/syncr-worker.service
==> Starting the service
==> Checking the setup
    ok    configuration /etc/syncr/worker.yaml is valid (link mode: active)
    ok    service syncr-worker is running

Setup complete.
```

Useful flags (all optional):

| Flag | Meaning |
| --- | --- |
| `--name NAME` | display name (default: the host name) |
| `--local-root PATH` | a directory jobs may read and write; repeat for several. Without one, jobs cannot touch local files |
| `--ca-file FILE` | PEM CA (or self-signed certificate) of the controller |
| `--data-dir DIR`, `--config FILE` | override `/var/lib/syncr` and `/etc/syncr/worker.yaml` (relative paths are resolved against the current directory, and the service gets the absolute ones) |
| `--no-service` | write the config only; run the worker yourself |
| `--dry-run` | print the plan, change nothing |
| `--force` | replace an existing setup (new enrollment, new certificate); the old config, certificate and CA copy stay in place until the new enrollment succeeds |

Set `SYNCR_ENROLLMENT_TOKEN` instead of `--token` to keep the token out of your
shell history.

Setup refuses to run twice: if the config or the service exists it says so and
stops, before it spends a token. Use `--force` to start over. The old worker
entry stays on the controller; delete it there.

## User mode (no root)

```sh
syncr-worker setup --user --controller https://syncr.example.com --token syncr_et_...
```

Config goes to `~/.config/syncr/worker.yaml`, data to `~/.local/share/syncr`,
and the service is a **systemd user unit** (`~/.config/systemd/user/syncr-worker.service`).
It needs a login session of your own (`XDG_RUNTIME_DIR` must be set; plain `su`
or `sudo -u` do not provide it). A user unit stops at logout. To keep it
running without being logged in, and to start it at boot, run once:

```sh
sudo loginctl enable-linger "$USER"
```

Add `--user` to the service commands too: `syncr-worker service status --user`.

## Passive worker (the controller calls the worker)

Use this for a server with a reachable port. The worker serves TLS with a
self-signed certificate; the controller pins its fingerprint.

The **Passive** tab of **Workers, Add worker** shows the install command. Or
set it up by hand (no controller needed during setup):

```sh
sudo syncr-worker setup --passive --listen :7443 --advertise-url https://nas.example.com:7443 \
     --tls-name nas.lan --tls-name 192.0.2.10
```

It prints the values to paste into the registration form below the
commands:

```
  URL:             https://nas.example.com:7443
  Fingerprint:     3f1c...64 hex digits...
  Pairing secret:  Zm9v...
```

Or let setup register it with a token:

```sh
sudo syncr-worker setup --passive --listen :7443 --advertise-url https://nas.example.com:7443 \
     --controller https://syncr.example.com --token syncr_et_...
```

The certificate lists the host of `--advertise-url`, the machine's host name
and every `--tls-name` as subject alternative names, and is valid for 10 years
(rotating it means registering the worker again). `--tls-cert`/`--tls-key` use
a certificate you already have. Open the port in your firewall
(`sudo ufw allow 7443/tcp`, or the firewalld equivalent).

## Managing the service

```sh
sudo syncr-worker service status      # exit status 1 unless it runs
sudo syncr-worker service restart
sudo syncr-worker service stop
sudo syncr-worker service uninstall   # keeps the config and data
```

The unit is `/etc/systemd/system/syncr-worker.service`: it restarts the worker
5 seconds after a failure (and gives up after 10 failures in 5 minutes), waits
up to 30 seconds on stop, and sets `KillMode=mixed` (a stop or restart signals
only the worker, which cancels its jobs and stops rclone and restic itself; what
is left when it exits is killed), `NoNewPrivileges`, `ProtectKernelTunables`,
`ProtectKernelModules`, `ProtectControlGroups`, `RestrictRealtime` and
`LockPersonality`. It runs as root, because backup jobs read whatever you list
in `local_roots`; `ProtectSystem`, `ProtectHome` and `PrivateTmp` are left out
on purpose since they would hide parts of the file system from jobs. To tighten
it, add a drop-in: `sudo systemctl edit syncr-worker`, for example
`User=syncr` (then `chown -R syncr /var/lib/syncr` and make the config
readable by that user) or `ReadWritePaths=` with `ProtectSystem=strict`.

## Upgrading

Upgrade the controller first. An older worker then shows as **Outdated** in
the web UI, and its page shows the command that upgrades it, for example:

```sh
sudo /usr/local/bin/syncr-worker upgrade --version 'v2.0.1'
"$HOME/.local/bin/syncr-worker" upgrade --user --version 'v2.0.1'   # --user workers
```

It verifies the download, fetches the new version's rclone and restic,
replaces the binary, restarts the service, and puts the old version back if
the new one does not stay up. Running jobs are interrupted by the restart.
It keeps running if your SSH connection drops; run it inside `tmux` or
`screen` (or with `nohup`) to see how it ended after reconnecting. Details,
the manual steps and containers: [upgrading](upgrade.md).

## Labels and schedules

Schedules run on one worker or on any worker with a label. An admin sets a
worker's labels on its page in the web UI (or with
`WorkerAdminService.SetWorkerLabels`), e.g. `nas` and `linux`: lower-case
letters, digits, `-`, `_` and `.`. A schedule targeting a label picks the
least busy online worker that has it and can run the job. A schedule's run
waits for an offline worker as its missed-run policy says, so a machine
that is switched off at night catches up when it is back (several missed
occurrences merge into one run while it may still wait; once its wait
limit has passed it is recorded as missed). A worker shows as offline about 45
seconds after its last contact; a run that comes due before that is still
queued on it, and fails if the worker does not pick it up within 15
minutes.

## Backups, browsing and updates

The folders in `local_roots` are also what the web UI can browse on this
worker, back up with restic and restore into
([restic backups](restic.md)). Browsing, remote tests and restic jobs need
a current worker: one that shows as **Outdated** in the web UI is upgraded
with `syncr-worker upgrade` ([above](#upgrading)).

## Hooks: commands around jobs

To let jobs dump a database, or stop and start containers, around a backup,
define the commands as `hooks` in `/etc/syncr/worker.yaml` and restart the
worker (`sudo syncr-worker service restart`). Users then choose them by
name in a job's form. See [hooks](hooks.md).

## Uninstall

```sh
sudo syncr-worker service uninstall
sudo rm -r /etc/syncr /var/lib/syncr      # optional: config, tools, certificate
```

Then remove the worker in the controller UI (**Workers**, the worker,
**Remove**; admins only).

## Where things are

| | System | `--user` |
| --- | --- | --- |
| Config | `/etc/syncr/worker.yaml` (0600) | `~/.config/syncr/worker.yaml` |
| Data, tools, certificate | `/var/lib/syncr` | `~/.local/share/syncr` |
| Logs | `journalctl -u syncr-worker -f` | `journalctl --user -u syncr-worker -f` |

The tools directory (`<data dir>/tools`, or `tools.dir`) belongs to this
worker alone: setup and upgrades delete the rclone and restic versions in it
that the worker no longer pins. Do not point `tools.dir` at a directory
that other programs, or a worker of another version, use.

## Troubleshooting

- **`cannot verify the TLS certificate`**: the controller uses a private CA or a
  self-signed certificate. Copy its CA (or certificate) as PEM and add
  `--ca-file ca.pem`. Certificate checks cannot be turned off.
- **`the controller rejected the enrollment token`**: tokens are single use and
  expire. Create a new one in the UI. If setup failed after enrolling, delete
  the half-created worker there first.
- **`cannot reach the controller`**: check the URL, DNS, firewall and proxy.
  `curl -v https://syncr.example.com` from the same machine shows the cause.
- **`does not answer the Syncr enrollment API`**: `--controller` must be the
  base URL, without `/api`; a reverse proxy must forward `/syncr.v1.EnrollmentService/` and
  `/syncr.v1.ControllerLinkService/`, with a read/idle timeout above 40 s (active workers
  long-poll for 25 s) and request bodies of up to 16 MiB.
- **`installing a system service needs elevated rights`**: use `sudo`, or `--user`.
- **`the service did not stay running`**: read the log with `journalctl -u syncr-worker -n 50`.
  A missing tool shows as `required tools are not available`; fix with
  `sudo syncr-worker tools fetch --dir /var/lib/syncr/tools`.
- **`every user can write to ...`**: setup will not keep the worker's data,
  tools or config in a directory anyone can change. Remove that permission
  (`chmod o-w`) or choose another directory with `--data-dir` / `--config`. A
  directory owned by another account, or writable by its group, only gets a
  warning (it may belong to a dedicated service account).
- **`warning: ... The service runs syncr-worker from there with full rights`**:
  other accounts can replace the binary (or add files next to it, where it
  looks for rclone and restic). Install it with
  `sudo install -m 0755 syncr-worker /usr/local/bin/` and run setup from there.
- **No internet access**: place `rclone` and `restic` of the pinned versions
  next to `syncr-worker` (or set `SYNCR_RCLONE_PATH` / `SYNCR_RESTIC_PATH` when
  running setup) and run setup again.
- **Setup stopped after enrolling** (for example the service failed): the config
  is saved; do not enroll again. Fix the cause and run
  `sudo syncr-worker service install --start`.
