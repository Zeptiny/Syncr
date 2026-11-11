package upgrade

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestReleaseAssets runs the download step of an upgrade against a release
// build: SYNCR_RELEASE_DIST names the dist/ directory of a GoReleaser build
// (CI builds a snapshot on every push), served as a mirror (--from). For
// every platform the releases build (linux/amd64, linux/arm64,
// windows/amd64), the upgrade must find its archive, verify it against
// checksums.txt and extract the worker binary, with the same code a real
// upgrade uses. On the platform running the test, the extracted binary must
// report its version the way step 2 reads it. internal/adapters/api has the
// same check for the install commands.
func TestReleaseAssets(t *testing.T) {
	t.Parallel()
	dist := os.Getenv("SYNCR_RELEASE_DIST")
	if dist == "" {
		t.Skip("set SYNCR_RELEASE_DIST to the dist/ directory of a GoReleaser build")
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(dist)))
	t.Cleanup(srv.Close)

	for _, p := range []struct{ os, arch string }{{"linux", "amd64"}, {"linux", "arm64"}, {"windows", "amd64"}} {
		t.Run(p.os+"_"+p.arch, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			exe := filepath.Join(dir, BinaryName)
			if err := os.WriteFile(exe, []byte("installed"), 0o700); err != nil {
				t.Fatal(err)
			}
			u, err := newUpgrader(Config{
				Executable: exe,
				ToolsDir:   filepath.Join(dir, "tools"),
				OS:         p.os,
				Arch:       p.arch,
				From:       srv.URL,
				HTTP:       srv.Client(),
				DryRun:     true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dist, u.asset)); err != nil {
				t.Fatalf("the upgrade downloads %s, which the build lacks: %v", u.asset, err)
			}
			bin, err := u.download(t.Context())
			if err != nil {
				t.Fatalf("download %s: %v", u.asset, err)
			}
			info, err := os.Stat(bin)
			if err != nil || info.Size() == 0 {
				t.Fatalf("extracted %s: %v (size %v)", u.member, err, info)
			}
			if p.os != runtime.GOOS || p.arch != runtime.GOARCH {
				return
			}
			out, err := exec.CommandContext(t.Context(), bin, "version").Output()
			if err != nil {
				t.Fatalf("%s version: %v", u.member, err)
			}
			// What ExecBinary.Version reads: "syncr-worker <version> ...".
			if f := strings.Fields(string(out)); len(f) < 2 || f[0] != BinaryName {
				t.Fatalf("%s version printed %q", u.member, out)
			}
			// A snapshot reports dev-<commit>; a release its tag.
			v := strings.Fields(string(out))[1]
			if !regexp.MustCompile(`^(dev-[0-9a-f]+|v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?)$`).MatchString(v) {
				t.Errorf("the release build reports version %q", v)
			}
		})
	}
}
