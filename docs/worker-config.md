# Worker configuration

`syncr-worker run` reads an optional YAML file (`--config path` or
`SYNCR_CONFIG`), applies `SYNCR_*` environment overrides, then validates.
Unknown keys are rejected and all invalid settings are reported together.
Paths must be absolute on the machine the worker runs on.

You normally do not write this file by hand: `syncr-worker setup` creates it
(see [setup-linux.md](setup-linux.md) and [setup-windows.md](setup-windows.md)).

```yaml
# worker.yaml
log:
  level: info                # SYNCR_LOG_LEVEL: debug, info, warn, error
  format: text               # SYNCR_LOG_FORMAT: text or json

# Where scratch space, the restic cache and managed tools live.
# Default: /var/lib/syncr (Linux, root), ~/.local/share/syncr (Linux, user),
# %ProgramData%\Syncr (Windows service), %LocalAppData%\Syncr (Windows user).
data_dir: /var/lib/syncr     # SYNCR_DATA_DIR

# Directories jobs may read and write locally: backup sources, restore
# targets, local repositories and local transfer endpoints. Symlinks (and
# Windows junctions) are resolved before the check. An empty list denies all
# local access; the worker logs a warning at startup. In the environment the
# list is comma-separated.
local_roots:                 # SYNCR_LOCAL_ROOTS
  - /srv/data
  - /home/alice/documents
  # Windows: use single quotes so backslashes are literal
  # - 'D:\Shares\Projects'

tools:
  # Explicit binaries, tried first. They must report the pinned version.
  rclone_path: ""            # SYNCR_RCLONE_PATH
  restic_path: ""            # SYNCR_RESTIC_PATH
  # Managed tools directory; default <data_dir>/tools. It must be this
  # worker's own: setup and upgrades delete the tool versions in it that
  # the worker no longer pins.
  dir: ""                    # SYNCR_TOOLS_DIR
  # Let the worker download missing pinned tools at startup
  # (checksum-verified). Off by default.
  download: false            # SYNCR_TOOLS_DOWNLOAD

# How many jobs run at once (1-64). Further jobs wait in a local queue and
# start in the order the controller sent them.
max_concurrent_jobs: 2       # SYNCR_MAX_CONCURRENT_JOBS

# Minimum time between two progress reports of one job (100ms-1m). The last
# sample of a job is always reported before its result.
progress_interval: 1s        # SYNCR_PROGRESS_INTERVAL

# The link to the controller; written by `syncr-worker setup`. Without a
# mode the worker is not enrolled: it starts, but has no controller.
link:
  mode: active               # SYNCR_LINK_MODE: active or passive
  worker_id: 0b6f8f0e-...    # SYNCR_WORKER_ID (active: required; passive: optional)

  # Active mode: the worker calls the controller.
  controller_url: https://syncr.example.com   # SYNCR_CONTROLLER_URL
  controller_ca_file: ""     # SYNCR_CONTROLLER_CA_FILE (optional)
  worker_secret: syncr_ws_...    # SYNCR_WORKER_SECRET

  # Passive mode: the controller calls the worker over TLS.
  # listen_addr: ":7443"                      # SYNCR_LISTEN_ADDR
  # advertise_url: https://nas.lan:7443       # SYNCR_ADVERTISE_URL
  # tls_cert_file: /var/lib/syncr/tls/worker.crt   # SYNCR_TLS_CERT_FILE
  # tls_key_file: /var/lib/syncr/tls/worker.key    # SYNCR_TLS_KEY_FILE
  # pairing_secret: ...                       # SYNCR_PAIRING_SECRET
```

## Link settings

Settings of the other mode must stay empty, and the mode itself is required as
soon as any link setting is present. Validation, per mode:

| Setting | Active | Passive |
| --- | --- | --- |
| `mode` | `active` | `passive` |
| `worker_id` | required, a UUID | optional, a UUID |
| `controller_url` | required: `https://`, or `http://` for `localhost` and loopback addresses only; no credentials, query or fragment; a path prefix is allowed | not allowed |
| `controller_ca_file` | optional absolute path to a PEM file | not allowed |
| `worker_secret` | required | not allowed |
| `listen_addr` | not allowed | required `host:port`, port 1-65535 (`:7443` listens everywhere) |
| `advertise_url` | not allowed | required `https://` URL as the controller reaches the worker |
| `tls_cert_file`, `tls_key_file` | not allowed | required absolute paths (PEM) |
| `pairing_secret` | not allowed | required: 32-512 printable ASCII characters, no spaces (the controller enforces the same) |

**There is no option to skip TLS verification.** A controller with a private CA
or a self-signed certificate is trusted by pointing `controller_ca_file` at
its CA (or at the certificate itself). The CA is added to the system roots. A
passive worker is authenticated differently: the controller pins the SHA-256
fingerprint of the certificate in `tls_cert_file` (the 64 hex digits that setup
prints), so a self-signed certificate is the normal case.

### At runtime

`syncr-worker run` starts the link of `mode` once the tools are resolved:

- **active**: the worker reports its events about once a second while jobs
  run (a heartbeat every 15 s otherwise) and long-polls the controller for
  commands (25 s). It needs no open port. When the controller is unreachable it
  retries with backoff (up to 30 s) and keeps every result until the
  controller acknowledges it, so a controller restart loses nothing.
- **passive**: the worker serves TLS on `listen_addr` and the controller calls
  it (about once a second while busy, every 15 s while idle). A port already in
  use fails the start.
- **no mode**: the worker starts, logs that it is not enrolled and waits.

On stop (service stop, Ctrl-C, SIGTERM) the worker cancels its running jobs
(rclone and restic get 10 s to exit, then they are killed), reports them as
cancelled by the shutdown (the controller shows them `interrupted`, code
`worker_shutdown`), and gives the controller up to 5 s to collect those
results. Jobs still waiting in the worker's queue never started, so the
controller queues them again for the worker's next start. The whole stop
stays under 20 s, the time a service manager allows. If the worker dies
instead (crash, SIGKILL, out of memory), its rclone and restic processes are
killed with it (on Linux and Windows; on other systems they finish or fail on
their own), and the controller marks its running jobs `interrupted`
(`worker_restarted`) when it comes back.

### Secrets

`worker_secret` and `pairing_secret` are stored in clear text in the config
file, so the file must be private:

- Linux: setup writes it with mode `0600` (owner only), atomically, in a
  directory of mode `0755` (system) or `0700` (user). The private key of a
  generated certificate is `0600` too.
- Windows: setup creates the file with an ACL limited to `SYSTEM` and
  `Administrators` (and, for `--user` installs, the installing user), not
  inherited from the parent folder, already on the temporary file it writes
  before renaming it into place. This is best effort: if the ACL cannot be
  built, setup prints a warning and you should restrict the file yourself.
  Because the `Administrators` entry only applies to an elevated token, run
  `syncr-worker run` by hand from an elevated terminal for a system install.

The directories matter as much as the files: the service runs the tools in
`<data_dir>/tools` and trusts the config, so whoever can change those
directories can run code with the worker's rights. Setup therefore prepares
the data directory, and the tools and config directories where they are not
inside it:

- Windows: a directory setup creates (and any missing parent) gets the same
  private ACL, inherited by everything inside. An existing one must be a real
  directory (not a link or junction) owned by `SYSTEM`, `Administrators`,
  `TrustedInstaller` or the account running setup; otherwise setup stops and
  says so, since another account may have created it in advance. If other
  accounts can change an existing directory, setup restricts it when it holds
  only the worker's files (and every file in it has one of those owners), and
  refuses it otherwise, before contacting the controller.
- Linux: directories are created as before. An existing one that every user
  may write to (without the sticky bit) is refused; one owned by another
  account or writable by its group is reported as a warning.

For a system install, setup also warns when other accounts can change the
`syncr-worker` binary, the directory it is in, or an rclone or restic binary
that is not in the managed tools directory, because the service runs them
with full rights. Keep them where only administrators can change them, such
as `C:\Program Files\Syncr` or `/usr/local/bin`.

In memory the secrets are `redact.Secret` values: printing or logging the
loaded configuration (with `%v`, `%+v`, slog text or JSON) shows `[redacted]`,
and log attributes named like a secret are redacted too. Validation errors
never contain a secret value. Prefer environment variables
(`SYNCR_WORKER_SECRET`, ...) over the file if you inject secrets from a
secret store; they override the file.

## Tools

Each of rclone and restic is looked up in this order and used only if it
reports exactly the version pinned by this release:

1. `tools.rclone_path` / `tools.restic_path` (a configured path that is set but
   unusable is an error, not skipped);
2. a binary next to the `syncr-worker` executable;
3. the managed directory (`tools.dir`), filled by `syncr-worker tools fetch`
   or, when `tools.download` is true, by the worker itself;
4. `PATH`.

Candidates that were found but rejected (wrong version, not executable) are
logged as warnings, and the chosen binary's version, source and path as info,
at startup. If a tool cannot be found, `run` exits non-zero and says how to
fix it. Inspect the sources with `syncr-worker tools status`.

`tools fetch` and `tools status` take `--dir`, else `SYNCR_TOOLS_DIR`, else
`<SYNCR_DATA_DIR or the default data dir>/tools`. A `data_dir` or `tools.dir`
set only in the YAML file is not visible to them: pass `--dir` for those.

`syncr-worker setup` installs missing tools into `SYNCR_TOOLS_DIR` when it is
set, else into `<data_dir>/tools`, and writes what the service needs to find
them again: always `data_dir`, `tools.dir` when it is not `<data_dir>/tools`,
and `tools.rclone_path` / `tools.restic_path` for binaries given with
`SYNCR_RCLONE_PATH` / `SYNCR_RESTIC_PATH`. Its self-check fails if a managed
tool is not where the service will look, and warns about a tool found only on
the setup shell's `PATH` (the service may run with another one).

**The tools directory must be this worker's own.** `setup` and
`syncr-worker upgrade` (once the upgraded worker stayed up) delete the
managed rclone and restic versions in it that this worker no longer pins
(`<dir>/rclone/<version>`, `<dir>/restic/<version>`). Two workers of
different versions sharing one tools directory would delete each other's
tools, and so would any other program that keeps an `rclone` or `restic`
directory there. Configured, bundled and `PATH` binaries are never touched.

## Child process limits

Tool output is read line by line; the worker allows lines up to 32 MiB because
restic prints its snapshot list and `forget` result as one JSON line.
