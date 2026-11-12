package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// Golden files of the install commands live in testdata/install. Run the
// tests with SYNCR_UPDATE_GOLDEN=1 to rewrite them after a deliberate
// change, then review the diff.

const (
	goldenToken      = "syncr_et_AbCdEf-0123456789_xyzXYZ-abcdefghijklmnopq"
	goldenController = "https://syncr.example.com"
)

// renderGolden lays out commands for a golden file.
func renderGolden(cmds []*syncrv1.InstallCommand) string {
	var b strings.Builder
	for _, c := range cmds {
		fmt.Fprintf(&b, "### %s / %s / %s: %s\n# %s\n%s\n\n", c.GetPlatform(), c.GetScope(), c.GetShell(), c.GetTitle(), c.GetNote(), c.GetCommand())
	}
	return b.String()
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "install", name)
	if os.Getenv("SYNCR_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with SYNCR_UPDATE_GOLDEN=1 to create it)", err)
	}
	// Golden files are text: tolerate a CRLF checkout on Windows.
	if strings.ReplaceAll(string(want), "\r\n", "\n") != got {
		t.Errorf("%s differs from the rendered commands:\n%s", path, got)
	}
}

func TestEnrollCommandsGolden(t *testing.T) {
	t.Parallel()
	cmds, err := enrollCommands(InstallConfig{}, goldenController+"/", goldenToken)
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "enroll.golden", renderGolden(cmds))
	mirror, err := enrollCommands(InstallConfig{DownloadBaseURL: "https://mirror.example.com/syncr/v2.0.0/"}, "http://127.0.0.1:8080", goldenToken)
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "enroll_mirror_loopback.golden", renderGolden(mirror))
}

func TestPassiveCommandsGolden(t *testing.T) {
	t.Parallel()
	cmds, err := passiveCommands(InstallConfig{})
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "passive.golden", renderGolden(cmds))
}

func TestInstallCommandsShape(t *testing.T) {
	t.Parallel()
	cmds, err := enrollCommands(InstallConfig{}, goldenController, goldenToken)
	if err != nil {
		t.Fatal(err)
	}
	passive, err := passiveCommands(InstallConfig{})
	if err != nil {
		t.Fatal(err)
	}
	type key struct {
		p syncrv1.InstallPlatform
		s syncrv1.InstallScope
	}
	for name, list := range map[string][]*syncrv1.InstallCommand{"enroll": cmds, "passive": passive} {
		seen := map[key]bool{}
		for _, c := range list {
			seen[key{c.GetPlatform(), c.GetScope()}] = true
			wantShell := map[syncrv1.InstallPlatform]string{syncrv1.InstallPlatform_INSTALL_PLATFORM_LINUX: "sh", syncrv1.InstallPlatform_INSTALL_PLATFORM_WINDOWS: "powershell"}[c.GetPlatform()]
			if c.GetShell() != wantShell || c.GetTitle() == "" || c.GetNote() == "" {
				t.Errorf("%s: %v", name, c)
			}
			if (c.GetScope() == syncrv1.InstallScope_INSTALL_SCOPE_USER) != strings.Contains(c.GetCommand(), " setup --user ") {
				t.Errorf("%s %s: the scope and --user disagree: %s", name, c.GetTitle(), c.GetCommand())
			}
			if name == "enroll" && (!strings.Contains(c.GetCommand(), "--controller '"+goldenController+"'") || !strings.Contains(c.GetCommand(), "--token '"+goldenToken+"'")) {
				t.Errorf("enroll %s: the values are not single-quoted: %s", c.GetTitle(), c.GetCommand())
			}
			if name == "passive" && (!strings.Contains(c.GetCommand(), "--passive --listen ':7443' --advertise-url") || strings.Contains(c.GetCommand(), "--token")) {
				t.Errorf("passive %s: %s", c.GetTitle(), c.GetCommand())
			}
		}
		if len(list) != 4 || len(seen) != 4 {
			t.Errorf("%s: %d commands for %d platform/scope pairs, want 4", name, len(list), len(seen))
		}
	}
}

func TestInstallValidation(t *testing.T) {
	t.Parallel()
	good := InstallConfig{}
	for name, tt := range map[string]struct {
		cfg   InstallConfig
		url   string
		token string
	}{
		"single quote in the URL":      {good, "https://evil.example.com/'$(id)'", goldenToken},
		"double quote in the URL":      {good, `https://evil.example.com/"x`, goldenToken},
		"typographic quote in the URL": {good, "https://evil.example.com/’x", goldenToken},
		"space in the URL":             {good, "https://evil.example.com/ x", goldenToken},
		"dollar in the URL":            {good, "https://evil.example.com/$HOME", goldenToken},
		"backtick in the URL":          {good, "https://evil.example.com/`id`", goldenToken},
		"backslash in the URL":         {good, `https://evil.example.com\x`, goldenToken},
		"newline in the URL":           {good, "https://evil.example.com/\nid", goldenToken},
		"query in the URL":             {good, "https://syncr.example.com/?a=b", goldenToken},
		"fragment in the URL":          {good, "https://syncr.example.com/#x", goldenToken},
		"credentials in the URL":       {good, "https://user:pw@syncr.example.com", goldenToken},
		"plain http":                   {good, "http://syncr.example.com", goldenToken},
		"no scheme":                    {good, "syncr.example.com", goldenToken},
		"other scheme":                 {good, "ftp://syncr.example.com", goldenToken},
		"relative":                     {good, "/syncr", goldenToken},
		"empty URL":                    {good, "", goldenToken},
		"quote in the token":           {good, goldenController, "syncr_et_x'y"},
		"space in the token":           {good, goldenController, "syncr_et_x y"},
		"empty token":                  {good, goldenController, ""},
		"bad download URL":             {InstallConfig{DownloadBaseURL: "https://mirror.example.com/'x"}, goldenController, goldenToken},
		"download URL with a query":    {InstallConfig{DownloadBaseURL: "https://mirror.example.com/?x"}, goldenController, goldenToken},
		"download URL of another kind": {InstallConfig{DownloadBaseURL: "file:///srv/releases"}, goldenController, goldenToken},
	} {
		if cmds, err := enrollCommands(tt.cfg, tt.url, tt.token); err == nil {
			t.Errorf("%s: rendered %v", name, cmds)
		}
	}
	for _, u := range []string{"https://syncr.example.com:8443/base/", "http://localhost:8080", "http://127.0.0.1", "http://[::1]:8080", "https://[2001:db8::1]:8443"} {
		if _, err := enrollCommands(good, u, goldenToken); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	if _, err := passiveCommands(InstallConfig{DownloadBaseURL: "https://x.example.com/a b"}); err == nil {
		t.Error("passive commands rendered with a bad download URL")
	}
}

func TestInstallConfigValidate(t *testing.T) {
	t.Parallel()
	for _, ok := range []InstallConfig{{}, {PublicURL: "https://syncr.example.com"}, {PublicURL: "http://localhost:8080", DownloadBaseURL: "http://mirror.lan/releases"}} {
		if err := ok.Validate(); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
	for _, bad := range []InstallConfig{{PublicURL: "http://syncr.lan:8080"}, {PublicURL: "https://x/'"}, {DownloadBaseURL: "ftp://mirror"}} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v: accepted", bad)
		}
	}
	c := InstallConfig{PublicURL: "https://public.example.com/"}
	if u, ok, err := c.controllerURL("https://browser.example.com"); err != nil || !ok || u != "https://public.example.com" {
		t.Errorf("the public URL does not win: %q %v %v", u, ok, err)
	}
	if _, ok, err := (InstallConfig{}).controllerURL(""); ok || err != nil {
		t.Errorf("no URL at all: %v %v", ok, err)
	}
}

func TestIsReleaseVersion(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]bool{
		"v2.0.0": true, "v2.0.0-rc.1": true, "v10.20.30": true, "v2.0.0-rc.12": true,
		"": false, "dev": false, "2.0.0": false, "v2.0": false, "v2.0.0-beta.1": false, "v2.0.0-rc": false,
		"v2.0.0-rc.1-dirty": false, "v2.0.1-0.20261110120000-0123456789ab": false, "v2.0.0+meta": false, "v2.0.0\n": false,
	} {
		if got := isReleaseVersion(v); got != want {
			t.Errorf("isReleaseVersion(%q) = %v, want %v", v, got, want)
		}
	}
}

// TestInstallFollowsTheControllerVersion: without a configured download
// base, a release controller's install commands download its own release;
// a development build's download the latest; a configured base always
// wins.
func TestInstallFollowsTheControllerVersion(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		cfg  InstallConfig
		want string
	}{
		{InstallConfig{Version: "v2.0.0"}, "https://github.com/Zeptiny/Syncr/releases/download/v2.0.0"},
		{InstallConfig{Version: "v2.0.0-rc.1"}, "https://github.com/Zeptiny/Syncr/releases/download/v2.0.0-rc.1"},
		{InstallConfig{Version: "dev"}, DefaultDownloadBaseURL},
		{InstallConfig{}, DefaultDownloadBaseURL},
		{InstallConfig{Version: "v2.0.0", DownloadBaseURL: "https://mirror.example.com/syncr/"}, "https://mirror.example.com/syncr"},
	} {
		base, err := tt.cfg.downloadBase()
		if err != nil || base != tt.want {
			t.Errorf("%+v: base = %q, %v; want %q", tt.cfg, base, err, tt.want)
		}
		cmds, err := enrollCommands(tt.cfg, goldenController, goldenToken)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cmds {
			if !strings.Contains(c.GetCommand(), tt.want+"/syncr-worker_") {
				t.Errorf("%+v %s: %s does not download from %s", tt.cfg, c.GetTitle(), c.GetCommand(), tt.want)
			}
		}
	}
	cmds, err := enrollCommands(InstallConfig{Version: "v2.0.0-rc.1"}, goldenController, goldenToken)
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "enroll_release.golden", renderGolden(cmds))
}

func TestUpgradeCommandsGolden(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		cfg  InstallConfig
	}{
		{"upgrade_release", InstallConfig{Version: "v2.0.0"}},
		{"upgrade_dev_mirror", InstallConfig{Version: "dev", DownloadBaseURL: "https://mirror.example.com/syncr/v2.0.0/"}},
	} {
		var all []*syncrv1.InstallCommand
		for _, goos := range []string{"linux", "windows"} {
			cmds, err := upgradeCommands(tt.cfg, goos, "amd64")
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, cmds...)
		}
		checkGolden(t, tt.name+".golden", renderGolden(all))
	}
}

func TestUpgradeCommands(t *testing.T) {
	t.Parallel()
	type key struct {
		p syncrv1.InstallPlatform
		s syncrv1.InstallScope
	}
	// The binary each scope installs to (as the install commands do).
	wantBin := map[key]string{
		{syncrv1.InstallPlatform_INSTALL_PLATFORM_LINUX, syncrv1.InstallScope_INSTALL_SCOPE_SYSTEM}:   "sudo /usr/local/bin/syncr-worker upgrade",
		{syncrv1.InstallPlatform_INSTALL_PLATFORM_LINUX, syncrv1.InstallScope_INSTALL_SCOPE_USER}:     `"$HOME/.local/bin/syncr-worker" upgrade --user`,
		{syncrv1.InstallPlatform_INSTALL_PLATFORM_WINDOWS, syncrv1.InstallScope_INSTALL_SCOPE_SYSTEM}: "$d = Join-Path $env:ProgramFiles 'Syncr'\n& \"$d\\syncr-worker.exe\" upgrade",
		{syncrv1.InstallPlatform_INSTALL_PLATFORM_WINDOWS, syncrv1.InstallScope_INSTALL_SCOPE_USER}:   "$d = Join-Path $env:LOCALAPPDATA 'Programs\\Syncr'\n& \"$d\\syncr-worker.exe\" upgrade --user",
	}
	for _, tt := range []struct {
		name     string
		cfg      InstallConfig
		wantArgs string // after the binary and scope
	}{
		{"release", InstallConfig{Version: "v2.0.0"}, " --version 'v2.0.0'"},
		{"release candidate", InstallConfig{Version: "v2.0.0-rc.1"}, " --version 'v2.0.0-rc.1'"},
		{"development build", InstallConfig{Version: "dev"}, ""},
		{"no version", InstallConfig{}, ""},
		{"release with a mirror", InstallConfig{Version: "v2.0.0", DownloadBaseURL: "https://mirror.example.com/r/"}, " --version 'v2.0.0' --from 'https://mirror.example.com/r'"},
		{"development build with a mirror", InstallConfig{Version: "dev", DownloadBaseURL: "https://mirror.example.com/r"}, " --from 'https://mirror.example.com/r'"},
		{"http mirror", InstallConfig{DownloadBaseURL: "http://127.0.0.1:8000"}, " --from 'http://127.0.0.1:8000'"},
	} {
		for _, goos := range []string{"linux", "windows"} {
			cmds, err := upgradeCommands(tt.cfg, goos, "")
			if err != nil {
				t.Fatalf("%s/%s: %v", tt.name, goos, err)
			}
			if len(cmds) != 2 || cmds[0].GetScope() != syncrv1.InstallScope_INSTALL_SCOPE_SYSTEM || cmds[1].GetScope() != syncrv1.InstallScope_INSTALL_SCOPE_USER {
				t.Fatalf("%s/%s: %v", tt.name, goos, cmds)
			}
			for _, c := range cmds {
				want := wantBin[key{c.GetPlatform(), c.GetScope()}] + tt.wantArgs
				if c.GetCommand() != want {
					t.Errorf("%s/%s %s:\n%s\nwant\n%s", tt.name, goos, c.GetTitle(), c.GetCommand(), want)
				}
				if !strings.Contains(c.GetNote(), "Running jobs are interrupted") {
					t.Errorf("%s/%s %s: the note does not warn about running jobs: %s", tt.name, goos, c.GetTitle(), c.GetNote())
				}
				if wantShell := map[string]string{"linux": "sh", "windows": "powershell"}[goos]; c.GetShell() != wantShell {
					t.Errorf("%s/%s: shell %q", tt.name, goos, c.GetShell())
				}
				if strings.HasPrefix(tt.cfg.DownloadBaseURL, "http:") != strings.Contains(c.GetNote(), "plain http") {
					t.Errorf("%s/%s: http note: %s", tt.name, goos, c.GetNote())
				}
			}
		}
	}

	for _, tt := range []struct{ goos, goarch string }{
		{"", ""}, {"", "amd64"}, {"darwin", "arm64"}, {"freebsd", "amd64"}, {"windows", "arm64"}, {"linux", "386"},
	} {
		if cmds, err := upgradeCommands(InstallConfig{Version: "v2.0.0"}, tt.goos, tt.goarch); err != nil || cmds != nil {
			t.Errorf("%s/%s: %v, %v; want none", tt.goos, tt.goarch, cmds, err)
		}
	}
	for _, arch := range []string{"amd64", "arm64"} {
		if cmds, _ := upgradeCommands(InstallConfig{}, "linux", arch); len(cmds) != 2 {
			t.Errorf("linux/%s: %d commands", arch, len(cmds))
		}
	}
	// A mirror that cannot be quoted safely is refused, as for the
	// install commands.
	for _, bad := range []string{"https://mirror.example.com/'$(id)'", "https://mirror.example.com/a b", "ftp://mirror.example.com", "https://mirror.example.com/?q"} {
		if cmds, err := upgradeCommands(InstallConfig{DownloadBaseURL: bad}, "linux", "amd64"); err == nil {
			t.Errorf("%q: rendered %v", bad, cmds)
		}
	}
	// A version that is not a release version is never pasted in.
	for _, v := range []string{"v2.0.0'; rm -rf /", "v2.0.0-rc.1 --from https://evil.example.com"} {
		cmds, err := upgradeCommands(InstallConfig{Version: v}, "linux", "amd64")
		if err != nil || strings.Contains(cmds[0].GetCommand(), "--version") || strings.Contains(cmds[0].GetCommand(), "evil") {
			t.Errorf("%q: %v, %v", v, cmds, err)
		}
	}
}
