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
  # Managed tools directory; default <data_dir>/tools.
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
  worker_secret: wsec_...    # SYNCR_WORKER_SECRET

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

### Secrets

`worker_secret` and `pairing_secret` are stored in clear text in the config
file, so the file must be private:

- Linux: setup writes it with mode `0600` (owner only), atomically, in a
  directory of mode `0755` (system) or `0700` (user). The private key of a
  generated certificate is `0600` too.
- Windows: setup restricts the file's ACL to `SYSTEM` and `Administrators`
  (and, for `--user` installs, the installing user), without inheriting from
  the parent folder. This is best effort: if the ACL cannot be set, setup
  prints a warning and you should restrict the file yourself. Because the
  `Administrators` entry only applies to an elevated token, run
  `syncr-worker run` by hand from an elevated terminal for a system install.

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

## Child process limits

Tool output is read line by line; the worker allows lines up to 32 MiB because
restic prints its snapshot list and `forget` result as one JSON line.
