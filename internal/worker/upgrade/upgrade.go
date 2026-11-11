// Package upgrade replaces an installed worker with another release
// ("syncr-worker upgrade") in a way that cannot leave the worker stopped.
//
// Run performs six steps, and nothing on the machine changes before the
// fourth:
//
//  1. Download checksums.txt and this platform's archive
//     (syncr-worker_<os>_<arch>.tar.gz, or .zip on Windows), require the
//     archive's SHA-256 to equal its line in checksums.txt (nothing skips
//     this), and extract only the syncr-worker binary, under a size cap,
//     to a private temporary file next to the installed one.
//  2. Run the new binary's "version": with Config.Version it must report
//     exactly that version; if it reports the installed version there is
//     nothing to do.
//  3. Run its "tools fetch" for the worker's tools directory, then require
//     that its "tools status" resolves every tool it pins with the worker's
//     configured tool paths. Older tool versions stay installed.
//  4. Swap the binaries (see OSSwapper): an atomic rename on Unix; on
//     Windows the installed file is renamed to <name>.old and the new one
//     moved into place.
//  5. Restart the service and require it to stay running for
//     Config.HealthWindow (about 20 seconds).
//  6. On failure: stop the service, put the old binary back, start it and
//     return an error that says where the service's log is. On success:
//     prune the old tool versions (tools.Installer.Prune), remove the old
//     binary where the OS allows it (RemoveLeftover removes it at the next
//     start otherwise) and report both versions.
//
// Once step 4 began, the remaining steps run to the end even if the
// caller's context is cancelled, so an interrupted upgrade still ends with
// a running worker.
//
// The checksum file comes from the same release as the archive: it catches
// a corrupted or truncated download, not a compromised release. The trust
// is the same as for the install commands (HTTPS to the release host).
//
// The steps' effects go through ports (HTTPDoer, Services, Binary, Swapper,
// Pruner) so that every path, rollback included, is tested with fakes.
//
// Allowed dependencies: the standard library and internal/worker/{proc,
// service,tools}.
package upgrade

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"time"

	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/service"
	"github.com/Zeptiny/Syncr/internal/worker/tools"
)

// Names and defaults.
const (
	// BinaryName is the worker binary's name in a release archive, without
	// the ".exe" of Windows.
	BinaryName = "syncr-worker"
	// ChecksumsFile is the release asset that lists every archive's SHA-256.
	ChecksumsFile = "checksums.txt"
	// DefaultReleases is where releases are published; the assets of a
	// release are under <DefaultReleases>/download/<version>, those of the
	// latest one under <DefaultReleases>/latest/download.
	DefaultReleases = "https://github.com/Zeptiny/Syncr/releases"
	// DefaultHealthWindow is how long the restarted service must stay up.
	DefaultHealthWindow = 20 * time.Second
	// healthPoll is how often the service's state is checked meanwhile.
	healthPoll = time.Second
	// rollbackTimeout bounds the rollback's service calls.
	rollbackTimeout = 2 * time.Minute
)

// Temporary files next to the installed binary.
const (
	tempDownload = ".syncr-worker-download-*"
	tempBinary   = ".syncr-worker-new-*"
)

// Errors, matched with errors.Is.
var (
	// ErrInsecureSource: --from is not https (or http to this machine).
	ErrInsecureSource = errors.New("refusing to download from an insecure source")
	// ErrInvalidVersion: --version is not a release version.
	ErrInvalidVersion = errors.New("invalid version")
	// ErrNoChecksum: checksums.txt has no (valid) line for the archive.
	ErrNoChecksum = errors.New("no checksum for the archive")
	// ErrChecksumMismatch: the archive does not match checksums.txt.
	ErrChecksumMismatch = errors.New("the downloaded archive does not match its checksum")
	// ErrVersionMismatch: the new binary reports another version than
	// --version.
	ErrVersionMismatch = errors.New("the downloaded binary has another version than requested")
	// ErrNotWritable: the upgrade cannot write the installed binary's
	// directory.
	ErrNotWritable = errors.New("the worker binary cannot be replaced")
	// ErrServiceNotInstalled: there is no service to restart, and
	// Config.NoRestart is not set.
	ErrServiceNotInstalled = errors.New("the worker service is not installed")
	// ErrOtherBinary: the service runs another file than the one to
	// replace.
	ErrOtherBinary = errors.New("the service runs another worker binary")
	// ErrToolsUnavailable: the new version's tools could not be provisioned.
	ErrToolsUnavailable = errors.New("the new version's tools are not available")
	// ErrRolledBack: the new version did not stay up and the old one was
	// put back and started.
	ErrRolledBack = errors.New("the upgrade was rolled back")
	// ErrRollbackFailed: the rollback itself failed; the error says how to
	// finish it by hand.
	ErrRollbackFailed = errors.New("the rollback failed")
)

// versionPattern matches a release version: v<major>.<minor>.<patch> with
// an optional pre-release, as the release tags have.
var versionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// Services controls the worker's service; service.Manager implements it.
type Services interface {
	// Preflight reports whether the service can be controlled with the
	// current rights.
	Preflight(spec service.Spec) error
	Status(ctx context.Context, spec service.Spec) (service.State, error)
	Start(ctx context.Context, spec service.Spec) error
	Stop(ctx context.Context, spec service.Spec) error
	Restart(ctx context.Context, spec service.Spec) error
	// Info says, among others, where the service's log is.
	Info(spec service.Spec) service.Info
}

// Pruner removes the managed installs of tool versions other than the
// ones to keep; *tools.Installer implements it.
type Pruner interface {
	Prune(ctx context.Context, keep []tools.ToolVersion) ([]string, error)
}

// Config configures one upgrade.
type Config struct {
	// Version is the release to install, e.g. "v2.1.0"; empty means the
	// latest release.
	Version string
	// From is the base URL of the release assets (a mirror); empty means
	// the GitHub release. It must be https; plain http is accepted only to
	// localhost or a loopback address.
	From string
	// Executable is the absolute path of the installed worker binary, which
	// the service runs; symbolic links are resolved. Required.
	Executable string
	// Current is the version of the installed binary; empty means: ask
	// it (run Executable's "version").
	Current string
	// OS and Arch select the release archive; empty means the running
	// system's.
	OS, Arch string
	// ToolsDir is the worker's managed tools directory, into which the new
	// version's tools are fetched. Required.
	ToolsDir string
	// ToolPaths are the worker's configured tool binaries
	// (tools.rclone_path, tools.restic_path) by tool name; empty entries
	// are ignored. The new version must resolve its pins with them.
	ToolPaths map[string]string
	// Service is the installed worker service. Ignored with NoRestart. If
	// its Executable is set, it must name the same file as Executable.
	Service service.Spec
	// NoRestart swaps the binary without touching any service (a worker
	// set up with --no-service): the operator restarts the process. Old tool
	// versions are kept, because the running worker still uses them.
	NoRestart bool
	// DryRun downloads and verifies the release and runs its version
	// command, then reports what would be done; it changes nothing (its
	// temporary files are removed).
	DryRun bool
	// HealthWindow is how long the restarted service must stay running;
	// zero means DefaultHealthWindow.
	HealthWindow time.Duration
	// Out receives progress messages for the operator; nil discards them.
	Out io.Writer

	// HTTP downloads the release assets; nil means tools.NewHTTPClient().
	HTTP HTTPDoer
	// Services controls the service; required unless NoRestart or DryRun.
	Services Services
	// Binary runs the new binary; nil means an ExecBinary over a
	// proc.Supervisor.
	Binary Binary
	// Swapper replaces the installed binary; nil means NewOSSwapper().
	Swapper Swapper
	// Pruner removes old tool versions; nil means a tools.Installer for
	// ToolsDir.
	Pruner Pruner
	// Sleep waits d or until ctx ends (for the health check); nil means a
	// timer.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Result describes a finished upgrade.
type Result struct {
	// Old and New are the installed version before and the version of the
	// downloaded release.
	Old, New string
	// UpToDate reports that the release is the installed version; nothing
	// was changed.
	UpToDate bool
	// DryRun reports a dry run; nothing was changed.
	DryRun bool
	// Swapped reports that the binary was replaced (and, with a service,
	// that it is running the new version).
	Swapped bool
	// Restarted reports that the service was restarted and stayed up.
	Restarted bool
	// Pruned lists the tool directories removed.
	Pruned []string
	// Warnings are problems that did not fail the upgrade (an old tool
	// version that could not be removed, a backup left behind).
	Warnings []string
}

// upgrader is one Run.
type upgrader struct {
	cfg Config
	// exe is the resolved installed binary, dir its directory, mode its
	// permission bits, backup the path of the copy of the old version.
	exe, dir, backup string
	mode             os.FileMode
	// base is the assets' base URL; asset the archive's name; member the
	// binary's name in it.
	base, asset, member string
	format              tools.Format
	goos                string
	res                 Result
}

// Run upgrades the worker as Config describes and returns what it did. A
// failure before the swap leaves the machine unchanged; a failure after it
// is rolled back (ErrRolledBack), or says how to finish the rollback by
// hand (ErrRollbackFailed).
func Run(ctx context.Context, cfg Config) (Result, error) {
	u, err := newUpgrader(cfg)
	if err != nil {
		return Result{}, err
	}
	return u.run(ctx)
}

func newUpgrader(cfg Config) (*upgrader, error) {
	cfg.OS = cmp.Or(cfg.OS, runtime.GOOS)
	cfg.Arch = cmp.Or(cfg.Arch, runtime.GOARCH)
	cfg.HealthWindow = cmp.Or(cfg.HealthWindow, DefaultHealthWindow)
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	if cfg.HTTP == nil {
		cfg.HTTP = tools.NewHTTPClient()
	}
	if cfg.Binary == nil {
		cfg.Binary = NewExecBinary(proc.New(proc.Options{}))
	}
	if cfg.Swapper == nil {
		cfg.Swapper = NewOSSwapper()
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleep
	}
	if cfg.Version != "" && !versionPattern.MatchString(cfg.Version) {
		return nil, fmt.Errorf("%w %q: a release version looks like v2.1.0 or v2.1.0-rc.1", ErrInvalidVersion, cfg.Version)
	}
	switch {
	case cfg.ToolsDir == "" || !filepath.IsAbs(cfg.ToolsDir):
		return nil, fmt.Errorf("upgrade: the tools directory %q is not an absolute path", cfg.ToolsDir)
	case !cfg.NoRestart && !cfg.DryRun && cfg.Services == nil:
		return nil, errors.New("upgrade: no service manager")
	}
	if cfg.Pruner == nil {
		inst, err := tools.NewInstaller(tools.InstallerOptions{Dir: cfg.ToolsDir})
		if err != nil {
			return nil, err
		}
		cfg.Pruner = inst
	}

	u := &upgrader{cfg: cfg, goos: cfg.OS}
	if !filepath.IsAbs(cfg.Executable) {
		return nil, fmt.Errorf("upgrade: the worker binary %q is not an absolute path", cfg.Executable)
	}
	exe, err := filepath.EvalSymlinks(cfg.Executable)
	if err != nil {
		return nil, fmt.Errorf("upgrade: find the worker binary: %w", err)
	}
	info, err := os.Stat(exe)
	if err != nil {
		return nil, fmt.Errorf("upgrade: find the worker binary: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("upgrade: %s is not a regular file", exe)
	}
	u.exe, u.dir, u.backup = exe, filepath.Dir(exe), exe+BackupSuffix
	u.mode = info.Mode().Perm() | 0o100 // stays executable by its owner
	if svc := cfg.Service.Executable; !cfg.NoRestart && svc != "" {
		resolved, err := filepath.EvalSymlinks(svc)
		if err != nil || resolved != exe {
			return nil, fmt.Errorf("%w: the %s service runs %s, not %s; upgrade that binary (run its upgrade command)", ErrOtherBinary, cfg.Service.Name, svc, exe)
		}
	}

	if u.base, err = releaseBase(cfg.From, cfg.Version); err != nil {
		return nil, err
	}
	u.member = BinaryName + u.exeSuffix()
	u.asset, u.format = fmt.Sprintf("%s_%s_%s.tar.gz", BinaryName, cfg.OS, cfg.Arch), tools.FormatTarGz
	if cfg.OS == "windows" {
		u.asset, u.format = fmt.Sprintf("%s_%s_%s.zip", BinaryName, cfg.OS, cfg.Arch), tools.FormatZip
	}
	return u, nil
}

// exeSuffix is ".exe" on Windows.
func (u *upgrader) exeSuffix() string {
	if u.goos == "windows" {
		return ".exe"
	}
	return ""
}

func (u *upgrader) say(format string, args ...any) {
	_, _ = fmt.Fprintf(u.cfg.Out, format+"\n", args...)
}

func (u *upgrader) warn(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	u.res.Warnings = append(u.res.Warnings, msg)
	u.say("warning: %s", msg)
}

func (u *upgrader) run(ctx context.Context) (Result, error) {
	cfg := &u.cfg
	if cfg.Current == "" {
		v, err := cfg.Binary.Version(ctx, u.exe)
		if err != nil {
			return u.res, fmt.Errorf("find the installed version: %w", err)
		}
		cfg.Current = v
	}
	u.res.Old = cfg.Current
	if !cfg.NoRestart && cfg.Services != nil { // read-only, so a dry run checks it too
		if err := u.checkService(ctx); err != nil {
			return u.res, err
		}
	}

	// 1. Download and verify.
	newBin, err := u.download(ctx)
	if err != nil {
		return u.res, err
	}
	defer func() { _ = os.Remove(newBin) }() // gone once swapped in

	// 2. Check the new binary.
	newVersion, err := cfg.Binary.Version(ctx, newBin)
	if err != nil {
		return u.res, fmt.Errorf("run the downloaded binary: %w; nothing was changed", err)
	}
	u.res.New = newVersion
	if cfg.Version != "" && newVersion != cfg.Version {
		return u.res, fmt.Errorf("%w: it reports %s, not %s; nothing was changed", ErrVersionMismatch, newVersion, cfg.Version)
	}
	if newVersion == cfg.Current {
		u.res.UpToDate = true
		u.say("%s is already installed; nothing to do", newVersion)
		return u.res, nil
	}
	if cfg.DryRun {
		u.res.DryRun = true
		u.say("Dry run: the release checks out. An upgrade from %s to %s would:", cfg.Current, newVersion)
		u.say("  - fetch the tools %s pins into %s and check that it finds them", newVersion, cfg.ToolsDir)
		u.say("  - replace %s (the old binary kept as %s until the upgrade succeeded)", u.exe, u.backup)
		if cfg.NoRestart {
			u.say("  - leave the restart to you (--no-restart)")
		} else {
			u.say("  - restart the %s service and require it to stay running for %s, or roll back", cfg.Service.Name, cfg.HealthWindow)
			u.say("  - remove the tool versions %s no longer uses", newVersion)
		}
		u.say("Nothing was changed.")
		return u.res, nil
	}

	// 3. Provision its tools.
	u.say("Fetching the tools %s pins into %s", newVersion, cfg.ToolsDir)
	pins, err := cfg.Binary.FetchTools(ctx, newBin, cfg.ToolsDir)
	if err != nil {
		return u.res, fmt.Errorf("%w: %w; the worker was not changed", ErrToolsUnavailable, err)
	}
	if err := cfg.Binary.CheckTools(ctx, newBin, cfg.ToolsDir, cfg.ToolPaths); err != nil {
		return u.res, fmt.Errorf("%w: %w; the worker was not changed", ErrToolsUnavailable, err)
	}

	// 4. Swap. From here on the upgrade runs to its end.
	ctx = context.WithoutCancel(ctx)
	if err := cfg.Swapper.Swap(u.exe, newBin, u.backup); err != nil {
		return u.res, fmt.Errorf("replace %s: %w; the worker was not changed", u.exe, err)
	}
	u.res.Swapped = true
	u.say("Replaced %s with %s", u.exe, newVersion)

	if cfg.NoRestart {
		u.removeBackup()
		u.say("Restart the worker to run %s (--no-restart). The tools of %s stay installed until the next setup or upgrade.", newVersion, cfg.Current)
		return u.res, nil
	}

	// 5. Restart and watch.
	if err := u.restartAndWatch(ctx); err != nil {
		// 6. Roll back.
		return u.res, u.rollback(ctx, err)
	}
	u.res.Restarted = true

	// 6. Finish.
	if len(pins) == 0 {
		u.warn("the tool versions %s pins could not be read, so no old tool version was removed", newVersion)
	} else {
		removed, err := cfg.Pruner.Prune(ctx, pins)
		u.res.Pruned = removed
		for _, dir := range removed {
			u.say("Removed the old tools in %s", dir)
		}
		if err != nil {
			u.warn("old tool versions not removed: %v", err)
		}
	}
	u.removeBackup()
	u.say("Upgraded from %s to %s", cfg.Current, newVersion)
	return u.res, nil
}

// checkService makes sure, before anything changes, that the service can
// be restarted: the rights are there and the service exists.
func (u *upgrader) checkService(ctx context.Context) error {
	spec := u.cfg.Service
	if err := u.cfg.Services.Preflight(spec); err != nil {
		return err
	}
	st, err := u.cfg.Services.Status(ctx, spec)
	if err != nil {
		return fmt.Errorf("check the worker service: %w", err)
	}
	switch st {
	case service.StateNotInstalled:
		return fmt.Errorf("%w (%s): for a worker you start yourself, add --no-restart and restart it after the upgrade", ErrServiceNotInstalled, spec.Name)
	case service.StateStopped:
		u.say("The %s service is stopped; the upgrade will start it", spec.Name)
	}
	return nil
}

// restartAndWatch restarts the service and requires it to be running at
// every poll of the health window.
func (u *upgrader) restartAndWatch(ctx context.Context) error {
	spec := u.cfg.Service
	u.say("Restarting the %s service", spec.Name)
	if err := u.cfg.Services.Restart(ctx, spec); err != nil {
		return fmt.Errorf("restart the service: %w", err)
	}
	u.say("Checking that it stays running for %s", u.cfg.HealthWindow)
	for waited := time.Duration(0); waited < u.cfg.HealthWindow; {
		d := min(healthPoll, u.cfg.HealthWindow-waited)
		if err := u.cfg.Sleep(ctx, d); err != nil {
			return err
		}
		waited += d
		st, err := u.cfg.Services.Status(ctx, spec)
		if err != nil {
			return fmt.Errorf("check the service: %w", err)
		}
		if st != service.StateRunning {
			return fmt.Errorf("the service was %s %s after the restart", st, waited.Round(time.Second))
		}
	}
	return nil
}

// rollback puts the old binary back after cause and starts it again.
func (u *upgrader) rollback(ctx context.Context, cause error) error {
	spec := u.cfg.Service
	ctx, cancel := context.WithTimeout(ctx, rollbackTimeout)
	defer cancel()
	logHint := u.cfg.Services.Info(spec).LogHint
	u.say("The new version did not stay running (%v); rolling back to %s", cause, u.cfg.Current)

	stopErr := u.cfg.Services.Stop(ctx, spec)
	if err := u.cfg.Swapper.Restore(u.exe, u.backup); err != nil {
		return fmt.Errorf("%w: %w; to finish by hand, stop the %s service, move %s to %s and start the service; its log: %s",
			ErrRollbackFailed, errors.Join(err, stopErr), spec.Name, u.backup, u.exe, logHint)
	}
	u.res.Swapped = false
	if err := u.cfg.Services.Start(ctx, spec); err != nil {
		return fmt.Errorf("%w: %s is back in place, but the service did not start: %w; start it by hand; its log: %s",
			ErrRollbackFailed, u.cfg.Current, err, logHint)
	}
	return fmt.Errorf("%w: %s did not stay running (%w); %s was put back and started. See the service's log: %s",
		ErrRolledBack, u.res.New, cause, u.cfg.Current, logHint)
}

// removeBackup removes the old binary's backup where the OS allows it. On
// Windows it usually cannot: this command runs from that file. The worker
// removes it at a later start (RemoveLeftover), or the next upgrade does.
func (u *upgrader) removeBackup() {
	err := os.Remove(u.backup)
	switch {
	case err == nil, errors.Is(err, os.ErrNotExist):
	case u.goos == "windows":
		u.say("The old binary stays at %s while this command runs; the worker removes it at a later start", u.backup)
	default:
		u.warn("the old binary stays at %s: %v", u.backup, err)
	}
}

// sleep waits d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-t.C:
		return nil
	}
}
