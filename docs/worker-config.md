# Worker configuration

`syncr-worker run` reads an optional YAML file (`--config path` or
`SYNCR_CONFIG`), applies `SYNCR_*` environment overrides, then validates.
Unknown keys are rejected and all invalid settings are reported together.
Paths must be absolute on the machine the worker runs on.

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
```

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
