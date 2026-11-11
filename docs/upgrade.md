# Upgrading Syncr

Upgrade the **controller first**, then each worker. A worker that is older
than the controller keeps working and is marked **Outdated** until it is
upgraded; a worker newer than its controller is not a supported state.

Every release has its notes in [docs/releases/](releases/); read the ones of
each version you skip. Syncr 2 does not read the data of the old Django
version: there is no upgrade path from it (see the
[v2.0.0 notes](releases/v2.0.0.md)).

## 1. The controller

**Back up the database first.** The controller migrates the schema when it
starts, and a migrated database cannot be used by the older version. With
the quick start's compose file:

```sh
docker compose exec -T postgres pg_dump -U syncr -d syncr -Fc > syncr-$(date +%F).dump
```

Keep `SYNCR_SECRETS_KEY` with the backup: without it the stored secrets of
remotes, repositories and workers cannot be read. To restore, start an
empty Postgres and use `pg_restore -U syncr -d syncr --clean --if-exists`
before starting the controller of that backup's version.

**Containers** (the compose file pulls `ghcr.io/zeptiny/syncr-controller`):

```sh
docker compose pull
docker compose up -d --wait
docker compose exec controller /usr/local/bin/syncr-controller -version
```

`SYNCR_VERSION` in `.env` picks the image tag: `2` (the default) follows
the newest 2.x release, `2.0` its patch releases, and an exact version such
as `2.0.1` stays where it is until you change it.

**A binary** (`syncr-controller_<os>_<arch>` from the
[release page](https://github.com/Zeptiny/Syncr/releases)): stop the
controller, replace the binary (verify it as in
[the setup guide](setup-linux.md#verifying-a-download)), start it again.

At start, the controller takes the database's controller lock, then runs
the migrations, then serves. One controller runs per database
([ADR 0015](adr/0015-single-controller-guard.md)): if you start the new
one before the old one has stopped, it waits and logs "another controller
is running against this database" until the old one exits, then takes over.
Workers reconnect by themselves; jobs that were running on workers carry
on, and their results are reconciled when the controller is back.

## 2. Each worker: `syncr-worker upgrade`

On the **Workers** page, an outdated worker has the **Outdated** badge. Its
page has an **Upgrade this worker** card with the command for that machine,
for example, with a controller of version v2.0.1:

```sh
sudo /usr/local/bin/syncr-worker upgrade --version 'v2.0.1'                 # Linux, system service
"$HOME/.local/bin/syncr-worker" upgrade --user --version 'v2.0.1'           # Linux, --user
```

```powershell
# Windows service, in an elevated PowerShell
$d = Join-Path $env:ProgramFiles 'Syncr'; & "$d\syncr-worker.exe" upgrade --version 'v2.0.1'
# Windows --user (Scheduled Task), in a normal PowerShell
$d = Join-Path $env:LOCALAPPDATA 'Programs\Syncr'; & "$d\syncr-worker.exe" upgrade --user --version 'v2.0.1'
```

The card always asks for the controller's own version. When the controller
has a download mirror (`workers.download_base_url`), the commands add
`--from '<mirror>'`.

What `upgrade` does, in this order; nothing on the machine changes before
step 4. It replaces the binary it runs from, so run the copy the service
runs: before anything else it reads the program from the installed service
(the systemd unit's `ExecStart`, the Windows Service's command line, or the
Scheduled Task's action) and refuses, naming the right binary, if that is
another file.

1. Downloads `checksums.txt` and this platform's archive
   (`syncr-worker_<os>_<arch>.tar.gz`, `.zip` on Windows), requires the
   archive's SHA-256 to match its line, and extracts the new
   `syncr-worker` next to the installed one. Nothing skips the checksum.
2. Runs the new binary's `version`: it must be the version asked for. If it
   is the version already installed, it says so and stops.
3. Fetches the rclone and restic the new version pins (old versions stay)
   and checks that the new version finds them with this worker's
   configuration.
4. Swaps the binary (on Windows, the old one is renamed to
   `syncr-worker.exe.old`).
5. Restarts the service and watches it for about 20 seconds.
6. If the new version does not stay up, it stops the service, puts the old
   binary back, starts it again and exits with an error that says where
   the service's log is. Otherwise it removes the old tool versions and
   prints the old and the new version.

| Flag | Meaning |
|---|---|
| `--version vX.Y.Z` | the release to install (default: the latest release) |
| `--from URL` | where the release assets are (a mirror); https, or http only on this machine |
| `--user` | the per-user service (systemd user unit, or the Windows Scheduled Task) |
| `--config FILE` | the worker's config, if not at the default place |
| `--no-restart` | swap the binary only, for a worker set up with `--no-service`; restart it yourself |
| `--dry-run` | download, verify and check the new version, change nothing |

Exit status: 0 when the new version runs (or was already installed, or the
dry run passed), 1 when the upgrade was refused or failed (and rolled back),
2 for invalid arguments.

Good to know:

- **Running jobs are interrupted** by the restart, as with any restart of
  the worker. Schedules see them as interrupted and follow their missed-run
  policy. Upgrade when nothing important runs.
- The health check sees that the service stays up, not that the worker
  reached the controller. Check that it is online on the **Workers** page.
- Run the command as the account that installed the worker (root or an
  Administrator for the system service, the user for `--user`).
- The checksum catches a damaged download, not a compromised release: the
  trust is the same as for the install command (HTTPS to GitHub, or your
  mirror). The release files also carry GitHub build provenance:
  `gh attestation verify syncr-worker_linux_amd64.tar.gz --owner Zeptiny`.

## 3. Workers in containers

A worker in a container is upgraded by pulling its new image
(`ghcr.io/zeptiny/syncr-worker`, the same tags as the controller); the image
brings the rclone and restic it pins. `syncr-worker upgrade` refuses inside
the image because it cannot replace its own binary there.

```sh
docker compose pull worker && docker compose up -d worker
```

The enrollment and configuration stay in the worker's volume.

## 4. By hand (fallback)

If `upgrade` cannot run (no outbound HTTPS from the worker, for example), do
its steps yourself. Fetch the new release's tools **before** swapping the
binary: a worker refuses to start without the tools its version pins.

Linux (system service; for `--user`, drop `sudo` and use
`~/.local/share/syncr/tools`, `~/.local/bin` and `service restart --user`):

```sh
v=v2.0.1
base="https://github.com/Zeptiny/Syncr/releases/download/$v"
curl -fsSLO "$base/checksums.txt"
curl -fsSLO "$base/syncr-worker_linux_amd64.tar.gz"     # or _linux_arm64
sha256sum --ignore-missing -c checksums.txt               # must say OK
tar -xzf syncr-worker_linux_amd64.tar.gz syncr-worker
sudo ./syncr-worker tools fetch --dir /var/lib/syncr/tools
sudo install -m 0755 syncr-worker /usr/local/bin/syncr-worker
sudo syncr-worker service restart
syncr-worker version
```

Windows (system service, elevated PowerShell; for `--user`, use
`%LocalAppData%\Programs\Syncr`, `%LocalAppData%\Syncr\tools` and
`--user` on the service commands):

```powershell
$v = 'v2.0.1'
$base = "https://github.com/Zeptiny/Syncr/releases/download/$v"
$t = Join-Path $env:TEMP 'syncr-upgrade'; New-Item -ItemType Directory -Force $t | Out-Null
Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile "$t\checksums.txt"
Invoke-WebRequest -UseBasicParsing -Uri "$base/syncr-worker_windows_amd64.zip" -OutFile "$t\syncr-worker_windows_amd64.zip"
$want = (Select-String -Path "$t\checksums.txt" -Pattern ' syncr-worker_windows_amd64.zip$').Line.Split(' ')[0]
$got = (Get-FileHash "$t\syncr-worker_windows_amd64.zip" -Algorithm SHA256).Hash.ToLower()
if ($got -ne $want) { throw "checksum mismatch: $got, checksums.txt says $want" }
Expand-Archive -Path "$t\syncr-worker_windows_amd64.zip" -DestinationPath $t -Force
& "$t\syncr-worker.exe" tools fetch --dir "$env:ProgramData\Syncr\tools"
$d = Join-Path $env:ProgramFiles 'Syncr'
& "$d\syncr-worker.exe" service stop
Copy-Item "$t\syncr-worker.exe" "$d\syncr-worker.exe" -Force
& "$d\syncr-worker.exe" service start
& "$d\syncr-worker.exe" version
```

If the new version does not start, put the old binary back the same way
and start the service again; its log says why
(`journalctl -u syncr-worker -n 50`, or the Event Log; see the setup
guides).

## What a version mismatch looks like

- **The Outdated badge** on the Workers page and the worker's page. A
  worker is outdated when it lacks a feature this controller uses, or when
  both run release versions and they differ. A development build (`dev`)
  is compared by features only.
- **`worker_outdated` jobs.** A job or a probe (browsing, a remote test,
  restic snapshots) that needs a feature the worker lacks fails at once
  with the error code `worker_outdated` and a message naming the missing
  feature, instead of misbehaving on the worker. A job with a time limit
  on a worker from before 2.0 is one example.
- An outdated worker that has every feature keeps running jobs normally;
  the badge only says that it should be upgraded.

New workers always get the controller's version: the install commands of
a release build download that release, not the latest one.
