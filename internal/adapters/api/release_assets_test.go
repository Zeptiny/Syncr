package api

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// linuxArch is how the Linux install commands turn `uname -m` into the
// architecture of the archive name; the releases build these two.
const linuxArch = `$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')`

// TestReleaseAssets checks a release build against the install commands:
// SYNCR_RELEASE_DIST names the dist/ directory of a GoReleaser build (CI
// builds a snapshot on every push). Every archive the commands download
// exists, is listed with its SHA-256 in checksums.txt, and holds only the
// binary the command runs, at its root; every worker archive of the build
// is one the commands download and one GetWorkerUpgradeCommands knows.
// internal/worker/upgrade has the same check for `syncr-worker upgrade`.
func TestReleaseAssets(t *testing.T) {
	t.Parallel()
	dist := os.Getenv("SYNCR_RELEASE_DIST")
	if dist == "" {
		t.Skip("set SYNCR_RELEASE_DIST to the dist/ directory of a GoReleaser build")
	}

	const version = "v2.0.0"
	cfg := InstallConfig{Version: version}
	base := ReleaseDownloadBaseURL + "/" + version
	enroll, err := enrollCommands(cfg, goldenController, goldenToken)
	if err != nil {
		t.Fatal(err)
	}
	passive, err := passiveCommands(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// The asset each command downloads, and the binary it runs from it.
	assetRef := regexp.MustCompile(regexp.QuoteMeta(base+"/") + `([A-Za-z0-9._{}-]+)`)
	tarMember := regexp.MustCompile(`tar -xz (\S+)`)
	zipMember := regexp.MustCompile(`& "\$d\\([^"]+)"`)
	want := map[string]string{} // archive name -> member
	for _, c := range append(enroll, passive...) {
		cmd := strings.ReplaceAll(c.GetCommand(), linuxArch, "{arch}")
		m := assetRef.FindAllStringSubmatch(cmd, -1)
		if len(m) != 1 {
			t.Fatalf("%s: want one download from %s, found %d:\n%s", c.GetTitle(), base, len(m), c.GetCommand())
		}
		member := tarMember.FindStringSubmatch(cmd)
		if strings.HasSuffix(m[0][1], ".zip") {
			member = zipMember.FindStringSubmatch(cmd)
		}
		if member == nil {
			t.Fatalf("%s: cannot tell which binary the command runs:\n%s", c.GetTitle(), c.GetCommand())
		}
		for _, arch := range []string{"amd64", "arm64"} {
			name := strings.ReplaceAll(m[0][1], "{arch}", arch)
			want[name] = member[1]
			if name == m[0][1] {
				break // no architecture to expand
			}
		}
	}

	sums := readChecksums(t, filepath.Join(dist, "checksums.txt"))
	for name, member := range want {
		path := filepath.Join(dist, name)
		sum, err := fileSHA256(path)
		if err != nil {
			t.Errorf("the install commands download %s: %v", name, err)
			continue
		}
		if sums[name] != sum {
			t.Errorf("checksums.txt lists %q for %s, its SHA-256 is %s", sums[name], name, sum)
		}
		entries, err := archiveEntries(path)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(entries) != 1 || entries[0] != member {
			t.Errorf("%s holds %q; the install command runs %q from its root", name, entries, member)
		}
	}

	// Conversely, every worker archive is downloaded by the install
	// commands and has upgrade commands.
	built, err := filepath.Glob(filepath.Join(dist, "syncr-worker_*"))
	if err != nil {
		t.Fatal(err)
	}
	platform := regexp.MustCompile(`^syncr-worker_([a-z0-9]+)_([a-z0-9]+)\.(tar\.gz|zip)$`)
	found := 0
	for _, path := range built {
		m := platform.FindStringSubmatch(filepath.Base(path))
		if m == nil {
			continue // a build directory, not an archive
		}
		found++
		if _, ok := want[m[0]]; !ok {
			t.Errorf("no install command downloads %s", m[0])
		}
		if !releasePlatform(m[1], m[2]) {
			t.Errorf("%s has no upgrade commands (releasePlatform)", m[0])
		}
	}
	if found != len(want) {
		t.Errorf("the build has %d worker archives, the install commands download %d: %v", found, len(want), want)
	}
}

// readChecksums parses a sha256sum-style checksums.txt into name -> hex.
func readChecksums(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sums := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if fields := strings.Fields(sc.Text()); len(fields) == 2 {
			sums[strings.TrimPrefix(fields[1], "*")] = fields[0]
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return sums
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// archiveEntries lists the names in a .tar.gz or .zip archive.
func archiveEntries(path string) ([]string, error) {
	var names []string
	if strings.HasSuffix(path, ".zip") {
		zr, err := zip.OpenReader(path)
		if err != nil {
			return nil, err
		}
		defer func() { _ = zr.Close() }()
		for _, f := range zr.File {
			names = append(names, f.Name)
		}
		return names, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
		names = append(names, h.Name)
	}
}
