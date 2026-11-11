package api

import (
	"cmp"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// Where install commands download worker releases from unless
// InstallConfig.DownloadBaseURL is set: the assets of the controller's own
// release (ReleaseDownloadBaseURL + "/" + InstallConfig.Version) when the
// controller is a release build, else those of the latest release
// (DefaultDownloadBaseURL).
const (
	DefaultDownloadBaseURL = "https://github.com/Zeptiny/Syncr/releases/latest/download"
	ReleaseDownloadBaseURL = "https://github.com/Zeptiny/Syncr/releases/download"
)

// releaseVersion matches the versions of release builds: vX.Y.Z, or a
// release candidate vX.Y.Z-rc.N. Its groups are the four numbers (the last
// empty for a final release).
var releaseVersion = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)(?:-rc\.([0-9]+))?$`)

// isReleaseVersion reports whether v is the version of a release build (a
// development build reports "dev" or a Go pseudo-version).
func isReleaseVersion(v string) bool { return releaseVersion.MatchString(v) }

// compareReleaseVersions orders two release versions (isReleaseVersion) by
// semantic versioning: by major, minor and patch number, and a release
// candidate before the release of its version, release candidates by
// number (v2.0.0-rc.1 < v2.0.0-rc.2 < v2.0.0 < v2.0.1). It returns -1, 0
// or +1; a version that is not a release version sorts first.
func compareReleaseVersions(a, b string) int {
	pa, okA := releaseParts(a)
	pb, okB := releaseParts(b)
	if !okA || !okB {
		return cmp.Compare(boolInt(okA), boolInt(okB))
	}
	for i := range 3 {
		if c := cmp.Compare(pa[i], pb[i]); c != 0 {
			return c
		}
	}
	// A final release (no candidate number) after its candidates.
	switch {
	case pa[3] == pb[3]:
		return 0
	case pa[3] < 0:
		return 1
	case pb[3] < 0:
		return -1
	}
	return cmp.Compare(pa[3], pb[3])
}

// releaseParts returns the major, minor and patch numbers of a release
// version and its release candidate number, -1 for a final release.
func releaseParts(v string) ([4]int64, bool) {
	m := releaseVersion.FindStringSubmatch(v)
	if m == nil {
		return [4]int64{}, false
	}
	parts := [4]int64{0, 0, 0, -1}
	for i, s := range m[1:] {
		if s == "" {
			continue
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return [4]int64{}, false // too many digits
		}
		parts[i] = n
	}
	return parts, true
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// InstallConfig configures the worker install and upgrade commands of
// WorkerAdminService (CreateEnrollmentToken, GetPassiveInstallCommands,
// GetWorkerUpgradeCommands), rendered by this file.
type InstallConfig struct {
	// PublicURL is the controller URL workers use (http.public_url). If
	// empty, CreateEnrollmentToken uses the request's controller_url (the
	// browser's origin), which must be https or a loopback http URL.
	PublicURL string
	// DownloadBaseURL is the base URL of the release assets
	// (workers.download_base_url); "" means GitHub's: the release of
	// Version, or the latest release.
	DownloadBaseURL string
	// Version is the controller's version (version.Info.Version; NewHandler
	// uses Deps.BuildInfo.Version when it is empty). When it is a release
	// version (isReleaseVersion), install commands without a
	// DownloadBaseURL download that release, upgrade commands ask for it
	// (--version), and Worker.outdated compares workers' versions with it.
	Version string
}

// Validate checks the configured URLs, so the composition root can refuse a
// configuration whose install commands could not be rendered: the public
// URL must be one a worker accepts (https, or http on a loopback address;
// no credentials, query or fragment), the download base URL an http(s) URL
// without query or fragment, and neither may contain quotes, spaces or
// other characters a shell would interpret.
func (c InstallConfig) Validate() error {
	if c.PublicURL != "" {
		if _, err := controllerURLOf(c.PublicURL); err != nil {
			return fmt.Errorf("public URL: %w", err)
		}
	}
	if _, err := c.downloadBase(); err != nil {
		return err
	}
	return nil
}

// downloadBase returns the validated download base URL, without a trailing
// slash: DownloadBaseURL, or the GitHub release of a release Version, or
// GitHub's latest release.
func (c InstallConfig) downloadBase() (string, error) {
	raw := c.DownloadBaseURL
	switch {
	case raw != "":
	case isReleaseVersion(c.Version):
		raw = ReleaseDownloadBaseURL + "/" + c.Version
	default:
		raw = DefaultDownloadBaseURL
	}
	u, err := parseSafeURL(raw)
	if err != nil {
		return "", fmt.Errorf("download base URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", errors.New("download base URL: must be an http or https URL")
	}
	return strings.TrimSuffix(raw, "/"), nil
}

// controllerURL returns the controller URL for enrollment commands: the
// configured public URL or, without one, requested (the browser's origin;
// "" means unknown, and no commands are rendered). An invalid requested URL
// is an error naming the problem; ok is false when there is no URL.
func (c InstallConfig) controllerURL(requested string) (u string, ok bool, err error) {
	raw := c.PublicURL
	if raw == "" {
		raw = requested
	}
	if raw == "" {
		return "", false, nil
	}
	u, err = controllerURLOf(raw)
	if err != nil {
		return "", false, err
	}
	return u, true, nil
}

// safeURL matches URLs that are safe to paste into sh and PowerShell inside
// single or double quotes: no quotes (including PowerShell's typographic
// ones), spaces, "$", "`", "\", "!" or non-ASCII characters.
var safeURL = regexp.MustCompile(`^[A-Za-z0-9._~:/%+@=,\[\]-]+$`)

// safeToken matches enrollment tokens (a prefix and base64url).
var safeToken = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

// parseSafeURL parses raw, which must be an absolute URL of safe
// characters with a host and without credentials, query or fragment.
func parseSafeURL(raw string) (*url.URL, error) {
	if len(raw) > 2048 || !safeURL.MatchString(raw) {
		return nil, errors.New("must be a plain URL without quotes, spaces or shell characters")
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return nil, errors.New("is not a valid URL")
	case u.Hostname() == "" || u.Opaque != "":
		return nil, errors.New("must be an absolute URL with a host")
	case u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return nil, errors.New("must not contain credentials, a query or a fragment")
	}
	return u, nil
}

// controllerURLOf validates a controller URL the way the worker's setup
// does (https, or http on a loopback host) and returns it without a
// trailing slash.
func controllerURLOf(raw string) (string, error) {
	u, err := parseSafeURL(raw)
	if err != nil {
		return "", err
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopbackHost(u.Hostname()):
	case u.Scheme == "http":
		return "", errors.New("must use https (plain http is allowed only for localhost)")
	default:
		return "", errors.New("must be an https URL")
	}
	return strings.TrimSuffix(raw, "/"), nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// sq single-quotes s for sh and PowerShell. Callers pass only values that
// matched safeURL or safeToken, which contain no quote of either shell.
func sq(s string) string { return "'" + s + "'" }

// passivePort is the port of the passive install commands.
const passivePort = "7443"

// Shell commands that download and unpack a release. The artifact names
// are fixed: GoReleaser must produce them (Milestone 6).
func linuxDownload(base string) string {
	return `curl -fsSL "` + base + `/syncr-worker_linux_$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/').tar.gz" | tar -xz syncr-worker`
}

func windowsDownload(base, dir string) string {
	return strings.Join([]string{
		"$ErrorActionPreference = 'Stop'",
		"$d = " + dir,
		"$z = Join-Path $env:TEMP 'syncr-worker.zip'",
		"Invoke-WebRequest -UseBasicParsing -Uri " + sq(base+"/syncr-worker_windows_amd64.zip") + " -OutFile $z",
		"Expand-Archive -Path $z -DestinationPath $d -Force",
	}, "\n")
}

// Install locations.
const (
	linuxSystemBin = "/usr/local/bin/syncr-worker"
	linuxUserBin   = `"$HOME/.local/bin/syncr-worker"`
	windowsSystem  = "Join-Path $env:ProgramFiles 'Syncr'"
	windowsUser    = "Join-Path $env:LOCALAPPDATA 'Programs\\Syncr'"
	windowsExe     = `& "$d\syncr-worker.exe"`
	alreadyThere   = " If syncr-worker is already installed, run only the setup command."
)

// renderInstall renders the four commands (Linux and Windows, system and
// user) that install the worker and run setup with args (already quoted).
func renderInstall(base, args string, windowsSystemExtra string) []*syncrv1.InstallCommand {
	return []*syncrv1.InstallCommand{
		{
			Platform: syncrv1.InstallPlatform_INSTALL_PLATFORM_LINUX, Scope: syncrv1.InstallScope_INSTALL_SCOPE_SYSTEM,
			Shell: "sh", Title: "Linux (system service)",
			Command: linuxDownload(base) + " \\\n" +
				"  && sudo install -m 0755 syncr-worker " + linuxSystemBin + " \\\n" +
				"  && sudo " + linuxSystemBin + " setup " + args,
			Note: "Needs sudo. Installs " + linuxSystemBin + " and a system service." + alreadyThere,
		},
		{
			Platform: syncrv1.InstallPlatform_INSTALL_PLATFORM_LINUX, Scope: syncrv1.InstallScope_INSTALL_SCOPE_USER,
			Shell: "sh", Title: "Linux (current user)",
			Command: linuxDownload(base) + " \\\n" +
				`  && mkdir -p "$HOME/.local/bin"` + " \\\n" +
				"  && install -m 0755 syncr-worker " + linuxUserBin + " \\\n" +
				"  && " + linuxUserBin + " setup --user " + args,
			Note: "No root needed. Installs ~/.local/bin/syncr-worker and a service of the current user." + alreadyThere,
		},
		{
			Platform: syncrv1.InstallPlatform_INSTALL_PLATFORM_WINDOWS, Scope: syncrv1.InstallScope_INSTALL_SCOPE_SYSTEM,
			Shell: "powershell", Title: "Windows (system service)",
			Command: windowsDownload(base, windowsSystem) + "\n" + windowsSystemExtra +
				windowsExe + " setup " + args,
			Note: "Run in an elevated PowerShell (Run as administrator). Installs a Windows service." + alreadyThere,
		},
		{
			Platform: syncrv1.InstallPlatform_INSTALL_PLATFORM_WINDOWS, Scope: syncrv1.InstallScope_INSTALL_SCOPE_USER,
			Shell: "powershell", Title: "Windows (current user)",
			Command: windowsDownload(base, windowsUser) + "\n" +
				windowsExe + " setup --user " + args,
			Note: "Run in a normal PowerShell. Installs a scheduled task of the current user." + alreadyThere,
		},
	}
}

// enrollCommands renders the commands that install an active worker and
// enroll it at controllerURL with token. Both values are validated and
// single-quoted.
func enrollCommands(cfg InstallConfig, controllerURL, token string) ([]*syncrv1.InstallCommand, error) {
	base, err := cfg.downloadBase()
	if err != nil {
		return nil, err
	}
	u, err := controllerURLOf(controllerURL)
	if err != nil {
		return nil, fmt.Errorf("controller URL: %w", err)
	}
	if !safeToken.MatchString(token) {
		return nil, errors.New("the enrollment token has unexpected characters")
	}
	return renderInstall(base, "--controller "+sq(u)+" --token "+sq(token), ""), nil
}

// passiveCommands renders the commands that install a passive worker
// listening on passivePort and advertising this host's name; setup then
// prints the URL, fingerprint and pairing secret to register it with.
func passiveCommands(cfg InstallConfig) ([]*syncrv1.InstallCommand, error) {
	base, err := cfg.downloadBase()
	if err != nil {
		return nil, err
	}
	firewall := "New-NetFirewallRule -DisplayName 'Syncr worker' -Direction Inbound -Protocol TCP -LocalPort " + passivePort + " -Action Allow | Out-Null\n"
	cmds := renderInstall(base, "--passive --listen "+sq(":"+passivePort), firewall)
	advertise := map[syncrv1.InstallPlatform]string{
		syncrv1.InstallPlatform_INSTALL_PLATFORM_LINUX:   ` --advertise-url "https://$(hostname):` + passivePort + `"`,
		syncrv1.InstallPlatform_INSTALL_PLATFORM_WINDOWS: ` --advertise-url "https://$($env:COMPUTERNAME):` + passivePort + `"`,
	}
	for _, c := range cmds {
		c.Command += advertise[c.Platform]
		c.Note += " The controller must reach this machine at the advertised URL: replace the host name if it cannot. Setup prints the URL, fingerprint and pairing secret to register the worker with."
		if c.Platform == syncrv1.InstallPlatform_INSTALL_PLATFORM_WINDOWS && c.Scope == syncrv1.InstallScope_INSTALL_SCOPE_USER {
			c.Note += " An administrator must allow inbound TCP port " + passivePort + " in the Windows firewall."
		}
	}
	return cmds, nil
}

// releasePlatform reports whether releases have a worker build for goos
// and goarch (the archive syncr-worker_<os>_<arch>); an empty goarch means
// unknown, and any of goos's builds may fit.
func releasePlatform(goos, goarch string) bool {
	switch goos {
	case "linux":
		return goarch == "" || goarch == "amd64" || goarch == "arm64"
	case "windows":
		return goarch == "" || goarch == "amd64"
	}
	return false
}

// upgradeCommands renders the commands that upgrade a worker running on
// goos/goarch in place ("syncr-worker upgrade"): one for the system service
// and one for the per-user service, each with the binary path the install
// commands install to. With a release Version they ask for it (--version);
// with a DownloadBaseURL they download from it (--from). An unknown
// operating system, or one without a release build for goarch (when
// known), has none (nil).
func upgradeCommands(cfg InstallConfig, goos, goarch string) ([]*syncrv1.InstallCommand, error) {
	if !releasePlatform(goos, goarch) {
		return nil, nil
	}
	var args, note string
	switch {
	case isReleaseVersion(cfg.Version):
		args += " --version " + sq(cfg.Version)
		note = " Installs " + cfg.Version + ", the controller's version."
	case cfg.DownloadBaseURL != "":
		note = " Installs the release the download mirror holds."
	default:
		note = " Installs the latest release."
	}
	if cfg.DownloadBaseURL != "" {
		base, err := cfg.downloadBase()
		if err != nil {
			return nil, err
		}
		args += " --from " + sq(base)
		if strings.HasPrefix(base, "http:") {
			note += " The upgrade downloads over plain http only from the worker's own machine (localhost): use an https mirror otherwise."
		}
	}
	const keep = " Running jobs are interrupted by the restart. If the new version does not stay up, the old one is put back."
	if goos == "linux" {
		return []*syncrv1.InstallCommand{
			{
				Platform: syncrv1.InstallPlatform_INSTALL_PLATFORM_LINUX, Scope: syncrv1.InstallScope_INSTALL_SCOPE_SYSTEM,
				Shell: "sh", Title: "Linux (system service)",
				Command: "sudo " + linuxSystemBin + " upgrade" + args,
				Note:    "Needs sudo. Replaces " + linuxSystemBin + " and restarts the system service." + note + keep,
			},
			{
				Platform: syncrv1.InstallPlatform_INSTALL_PLATFORM_LINUX, Scope: syncrv1.InstallScope_INSTALL_SCOPE_USER,
				Shell: "sh", Title: "Linux (current user)",
				Command: linuxUserBin + " upgrade --user" + args,
				Note:    "Run as the user the worker runs as. Replaces ~/.local/bin/syncr-worker and restarts that user's service." + note + keep,
			},
		}, nil
	}
	return []*syncrv1.InstallCommand{
		{
			Platform: syncrv1.InstallPlatform_INSTALL_PLATFORM_WINDOWS, Scope: syncrv1.InstallScope_INSTALL_SCOPE_SYSTEM,
			Shell: "powershell", Title: "Windows (system service)",
			Command: "$d = " + windowsSystem + "\n" + windowsExe + " upgrade" + args,
			Note:    "Run in an elevated PowerShell (Run as administrator). Replaces the worker in Program Files and restarts the Windows service." + note + keep,
		},
		{
			Platform: syncrv1.InstallPlatform_INSTALL_PLATFORM_WINDOWS, Scope: syncrv1.InstallScope_INSTALL_SCOPE_USER,
			Shell: "powershell", Title: "Windows (current user)",
			Command: "$d = " + windowsUser + "\n" + windowsExe + " upgrade --user" + args,
			Note:    "Run in a normal PowerShell as the user the worker runs as. Replaces the worker in that user's programs folder and restarts its scheduled task." + note + keep,
		},
	}, nil
}
