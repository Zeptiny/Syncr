# Setting up a worker on Windows

`syncr-worker setup` provisions the pinned `rclone` and `restic` (verified
download), enrolls the worker with the controller, writes the config, installs
and starts the worker, and checks that it runs. With administrator rights the
worker is a **Windows Service**; without (`--user`) it is a **per-user
Scheduled Task** that starts at logon.

## Quick path (one command)

In the controller UI open **Workers, Add worker**; it shows this command with a
single-use token. Put `syncr-worker.exe` where it will live (the service runs
exactly that path), for example `C:\Program Files\Syncr\`, then in an
**elevated** PowerShell (Run as administrator):

```powershell
& 'C:\Program Files\Syncr\syncr-worker.exe' setup --controller https://syncr.example.com --token wtok_...
```

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
syncr-worker.exe setup --user --controller https://syncr.example.com --token wtok_...
```

Windows only lets administrators create services, so `--user` registers a
Scheduled Task named `syncr-worker` instead. It runs as you, with your normal
rights, **when you log on**, and is restarted by Task Scheduler if it fails.
The worker does not run while nobody is logged on, and it stops at logoff or
when the task is ended (jobs running then become "interrupted"). The console
window is closed at start (`run --hide-console`), so the log goes to a file.
Config and data live under `%LocalAppData%\Syncr`. To manage it, open Task
Scheduler or use `syncr-worker service status --user` and friends.

## Passive worker

```powershell
syncr-worker.exe setup --passive --listen :7443 --advertise-url https://pc.example.com:7443
```

prints the URL, the certificate fingerprint and a pairing secret to paste into
**Workers, Add worker, Passive worker** (or add `--controller` and `--token` to
register it automatically). Allow the port through the firewall:

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

## Uninstall

```powershell
syncr-worker.exe service uninstall            # or: service uninstall --user
Remove-Item -Recurse 'C:\ProgramData\Syncr'   # optional: config, tools, certificate
```

Then delete the worker in the controller UI.

## Where things are

| | Service (administrator) | `--user` task |
| --- | --- | --- |
| Config | `%ProgramData%\Syncr\worker.yaml` | `%LocalAppData%\Syncr\worker.yaml` |
| Data, tools, certificate | `%ProgramData%\Syncr` | `%LocalAppData%\Syncr` |
| Logs | Event Viewer, Windows Logs, Application, source `syncr-worker`; or `Get-WinEvent -FilterHashtable @{LogName='Application'; ProviderName='syncr-worker'} -MaxEvents 30` | `%LocalAppData%\Syncr\logs\worker.log` (rotated at 10 MB to `worker.log.1`) |

The config file holds secrets: setup restricts it to `SYSTEM` and
`Administrators` (plus you, for `--user`). This is best effort and setup warns if
it fails. To run `syncr-worker run` by hand against a system config, use an
elevated terminal.

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
- **The task does not start after logon**: check it in Task Scheduler
  (`syncr-worker`, "Last Run Result") and read `worker.log`.
