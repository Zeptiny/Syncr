# Setting up a worker on Windows

`syncr-worker setup` provisions the pinned `rclone` and `restic` (verified
download), enrolls the worker with the controller, writes the config, installs
and starts the worker, and checks that it runs. With administrator rights the
worker is a **Windows Service**; without (`--user`) it is a **per-user
Scheduled Task** that starts at logon.

## Quick path (one command)

In the controller's web UI open **Workers, Add worker**. The **Active** tab
creates a single-use enrollment token and shows the install command for each
platform and scope. For a Windows Service, paste it into an **elevated**
PowerShell (Run as administrator):

```powershell
$ErrorActionPreference = 'Stop'
$d = Join-Path $env:ProgramFiles 'Syncr'
$z = Join-Path $env:TEMP 'syncr-worker.zip'
Invoke-WebRequest -UseBasicParsing -Uri 'https://github.com/Zeptiny/Syncr/releases/latest/download/syncr-worker_windows_amd64.zip' -OutFile $z
Expand-Archive -Path $z -DestinationPath $d -Force
& "$d\syncr-worker.exe" setup --controller 'https://syncr.example.com' --token 'syncr_et_...'
```

It downloads the release archive into `C:\Program Files\Syncr` (the
service runs exactly that path) and runs setup. The UI's command downloads
the controller's own release (`.../releases/download/v2.0.0/...` for a
v2.0.0 controller), so a new worker runs the controller's version; the
`latest` URL above, which a development build of the controller shows,
installs the newest release. If the controller sets
`workers.download_base_url` (empty by default), the command downloads from
that mirror instead (a value of
`https://github.com/Zeptiny/Syncr/releases/latest/download`, the default of
builds before v2.0.0, counts as unset; the controller warns at start that it
should be removed). If `syncr-worker.exe` is already there, run only the
last line:

```powershell
& 'C:\Program Files\Syncr\syncr-worker.exe' setup --controller https://syncr.example.com --token syncr_et_...
```

To check the archive against the release's `checksums.txt` before
installing it by hand (elevated PowerShell):

```powershell
$base = 'https://github.com/Zeptiny/Syncr/releases/latest/download'   # or releases/download/v2.0.0
$t = Join-Path $env:TEMP 'syncr-download'; New-Item -ItemType Directory -Force $t | Out-Null
Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile "$t\checksums.txt"
Invoke-WebRequest -UseBasicParsing -Uri "$base/syncr-worker_windows_amd64.zip" -OutFile "$t\syncr-worker_windows_amd64.zip"
$want = (Select-String -Path "$t\checksums.txt" -Pattern ' syncr-worker_windows_amd64.zip$').Line.Split(' ')[0]
$got = (Get-FileHash "$t\syncr-worker_windows_amd64.zip" -Algorithm SHA256).Hash.ToLower()
if ($got -ne $want) { throw "checksum mismatch: $got, checksums.txt says $want" }
Expand-Archive -Path "$t\syncr-worker_windows_amd64.zip" -DestinationPath (Join-Path $env:ProgramFiles 'Syncr') -Force
```

With the GitHub CLI, `gh attestation verify "$t\syncr-worker_windows_amd64.zip" --owner Zeptiny`
also checks the archive's build provenance.

Useful flags: `--name`, `--local-root 'D:\Shares'` (repeatable; without one jobs
cannot touch local files), `--ca-file C:\path\ca.pem`, `--dry-run`, `--force`,
`--no-service`. To keep the token out of the history, set
`$env:SYNCR_ENROLLMENT_TOKEN` and omit `--token`.

The service is called `syncr-worker`, starts automatically (delayed start, so
the network is up), runs as `LocalSystem`, and is restarted 10 seconds after a
crash. Setup refuses to run twice; use `--force` to start over (the old worker
entry stays on the controller; delete it there).

## User mode (no administrator)

```powershell
syncr-worker.exe setup --user --controller https://syncr.example.com --token syncr_et_...
```

Windows only lets administrators create services, so `--user` registers a
Scheduled Task named `syncr-worker` instead. It runs as you, with your normal
rights, **when you log on**, and is restarted by Task Scheduler if it fails.
The worker does not run while nobody is logged on, and it stops at logoff or
when the task is ended (jobs running then become "interrupted"). The console
window is closed at start (`run --hide-console`), so the log goes to a file.
Config and data live under `%LocalAppData%\Syncr`. To manage it, open Task
Scheduler or use `syncr-worker service status --user` and friends.

A worker in user mode cannot create volume snapshots (VSS): restic backups
with **Use a volume snapshot** need the worker installed as a service. See
[restic backups](restic.md#windows-volume-snapshots-vss).

## Passive worker

```powershell
syncr-worker.exe setup --passive --listen :7443 --advertise-url https://pc.example.com:7443
```

prints the URL, the certificate fingerprint and a pairing secret to paste into
the registration form of **Workers, Add worker, Passive** (or add
`--controller` and `--token` to register it automatically); that tab also
shows the install command. Allow the port through the firewall:

```powershell
New-NetFirewallRule -DisplayName 'Syncr worker' -Direction Inbound -Protocol TCP -LocalPort 7443 -Action Allow
```

## Managing the service

```powershell
syncr-worker.exe service status      # exit status 1 unless it runs
syncr-worker.exe service restart
syncr-worker.exe service stop
syncr-worker.exe service uninstall   # keeps the config and data
```

Add `--user` for the scheduled task. `status`, `start`, `stop` and `uninstall`
of the system service need an elevated terminal.

## Upgrading

Upgrade the controller first. An older worker then shows as **Outdated** in
the web UI, and its page shows the command that upgrades it, for example:

```powershell
# Windows service, in an elevated PowerShell
$d = Join-Path $env:ProgramFiles 'Syncr'; & "$d\syncr-worker.exe" upgrade --version 'v2.0.1'
# --user (Scheduled Task), in a normal PowerShell
$d = Join-Path $env:LOCALAPPDATA 'Programs\Syncr'; & "$d\syncr-worker.exe" upgrade --user --version 'v2.0.1'
```

It verifies the download, fetches the new version's rclone and restic,
replaces `syncr-worker.exe` (the old one is renamed to
`syncr-worker.exe.old` and removed at the worker's next start), restarts the
service or task, and puts the old version back if the new one does not stay
up. Running jobs are interrupted by the restart. Keep the PowerShell window
open until it has finished: closing it ends the command. Details and the
manual steps: [upgrading](upgrade.md).

## Labels and schedules

Schedules run on one worker or on any worker with a label. An admin sets a
worker's labels on its page in the web UI (or with
`WorkerAdminService.SetWorkerLabels`), e.g. `nas` and `linux`: lower-case
letters, digits, `-`, `_` and `.`. A schedule targeting a label picks the
least busy online worker that has it and can run the job. A schedule's run
waits for an offline worker as its missed-run policy says, so a desktop
that is switched off at night catches up when it is back (several missed
occurrences merge into one run while it may still wait; once its wait
limit has passed it is recorded as missed). A worker shows as offline about 45
seconds after its last contact; a run that comes due before that is still
queued on it, and fails if the worker does not pick it up within 15
minutes.

## Backups, browsing and updates

The folders in `local_roots` are also what the web UI can browse on this
worker, back up with restic and restore into
([restic backups](restic.md)). A drive is written `C:\`; a snapshot shows
`C:\Users\ada\Documents` as `/C/Users/ada/Documents`. Browsing, remote
tests and restic jobs need a current worker: one that shows as **Outdated**
in the web UI is upgraded with `syncr-worker upgrade` ([above](#upgrading)).

## Uninstall

```powershell
syncr-worker.exe service uninstall            # or: service uninstall --user
Remove-Item -Recurse 'C:\ProgramData\Syncr'   # optional: config, tools, certificate
```

Then remove the worker in the controller UI (**Workers**, the worker,
**Remove**; admins only).

## Where things are

| | Service (administrator) | `--user` task |
| --- | --- | --- |
| Config | `%ProgramData%\Syncr\worker.yaml` | `%LocalAppData%\Syncr\worker.yaml` |
| Data, tools, certificate | `%ProgramData%\Syncr` | `%LocalAppData%\Syncr` |
| Logs | Event Viewer, Windows Logs, Application, source `syncr-worker`; or `Get-WinEvent -FilterHashtable @{LogName='Application'; ProviderName='syncr-worker'} -MaxEvents 30` | `%LocalAppData%\Syncr\logs\worker.log` (rotated at 10 MB to `worker.log.1`) |

The tools directory (`<data dir>\tools`, or `tools.dir`) belongs to this
worker alone: setup and upgrades delete the rclone and restic versions in it
that the worker no longer pins. Do not point `tools.dir` at a directory
that other programs, or a worker of another version, use.

The config file holds secrets: setup restricts it to `SYSTEM` and
`Administrators` (plus you, for `--user`). This is best effort and setup warns if
it fails. To run `syncr-worker run` by hand against a system config, use an
elevated terminal.

The `Syncr` folder is restricted the same way, without inheriting from
`%ProgramData%` (where every user may create files): the service runs the tools
in it as `LocalSystem`. If the folder already exists, setup checks who owns it
and who can change it before using it; see
[worker-config.md](worker-config.md#secrets).

## Troubleshooting

- **`installing a system service needs elevated rights`**: start PowerShell with
  *Run as administrator*, or use `--user`.
- **`cannot verify the TLS certificate`**: the controller uses a private CA or a
  self-signed certificate; pass it as PEM with `--ca-file`. Certificate checks
  cannot be disabled.
- **Token rejected / cannot reach the controller / not the enrollment API**: same
  causes and fixes as on Linux, see [setup-linux.md](setup-linux.md#troubleshooting).
- **Service error 1053 ("did not respond in time")**: the binary at the service
  path is not this `syncr-worker.exe`, or was moved. Move it back or run
  `service uninstall` and setup again.
- **The service stops right after start**: read the Event Log (above). A missing
  tool shows as `required tools are not available`; run
  `syncr-worker.exe tools fetch --dir C:\ProgramData\Syncr\tools`.
- **Antivirus quarantines `rclone.exe`/`restic.exe`**: allow the folder
  `...\Syncr\tools`.
- **`... is owned by ..., not by SYSTEM, Administrators or the account running
  this`**: another account created the folder (or a file in it) before setup
  ran, possibly to plant programs the service would run. Check what it
  contains and delete it, or take it over as an administrator with
  `takeown /F <folder> /A /R` and `icacls <folder> /reset /T`, then run setup
  again.
- **`... can be changed by ..., who could then tamper with the worker's
  files`**: the folder setup should use is shared with other accounts and holds
  more than the worker's files, so setup will not restrict it. Choose a folder
  of its own with `--data-dir` (or `--config`).
- **`warning: ... The service runs syncr-worker.exe from there with full
  rights`**: other accounts can replace the binary or add files next to it.
  Move it to `C:\Program Files\Syncr` and run setup (or `service install`)
  again from there.
- **The task does not start after logon**: check it in Task Scheduler
  (`syncr-worker`, "Last Run Result") and read `worker.log`: a worker that
  stops writes why there (`the worker stopped with an error`), as its
  console is hidden.
- **`waiting for it to exit` in the log after a restart**: the new worker
  started while the ended one was still exiting, and waits up to 20
  seconds for its data directory. If the log then says `another worker is
  already running with this data directory`, a second worker (another task,
  service or `run`) uses the same data directory: stop it, or give each its
  own `--data-dir`.
