package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var host = HostPlatform()

// zipFixture serves a valid zip with the fake binary and returns the spec.
func zipFixture(t *testing.T) (*archiveServer, Spec) {
	t.Helper()
	archive := buildZip(t, zipMember{name: fakeEntry, body: fakeBinary(fakeVersion), mode: 0o755})
	srv := newArchiveServer(t, map[string][]byte{"faketool.zip": archive})
	return srv, testSpec(srv, host, "faketool.zip", FormatZip, fakeEntry, archive)
}

// listAll returns every path below dir, relative and slash-separated.
func listAll(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestInstallHappyPath(t *testing.T) {
	t.Parallel()
	srv, spec := zipFixture(t)
	dir := t.TempDir()
	det := &contentDetector{}
	inst := newTestInstaller(t, srv, dir, det)

	got, err := inst.Install(t.Context(), spec, host)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reused {
		t.Error("first install reported Reused")
	}
	wantPath := filepath.Join(dir, fakeName, fakeVersion, host.OS+"-"+host.Arch, spec.Binary(host.OS))
	if got.Path != wantPath {
		t.Errorf("Path = %s, want %s", got.Path, wantPath)
	}
	content, err := os.ReadFile(got.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, fakeBinary(fakeVersion)) {
		t.Errorf("binary content = %q", content)
	}
	if got.SHA256 != sum(content) {
		t.Errorf("SHA256 = %s, want %s", got.SHA256, sum(content))
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(got.Path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Errorf("mode = %v, want 0755", info.Mode().Perm())
		}
	}

	// The sidecar records what was installed.
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(got.Path), sidecarFileName))
	if err != nil {
		t.Fatal(err)
	}
	var sc sidecar
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatal(err)
	}
	if sc.Tool != fakeName || sc.Version != fakeVersion || sc.BinarySHA256 != sum(content) ||
		sc.ArchiveSHA256 != spec.Artifacts[host].SHA256 || sc.OS != host.OS || sc.Arch != host.Arch {
		t.Errorf("sidecar = %+v", sc)
	}

	// No temp files are left behind.
	for _, p := range listAll(t, dir) {
		if strings.Contains(p, tempPrefix) {
			t.Errorf("leftover temp file %s", p)
		}
	}
	if det.calls.Load() == 0 {
		t.Error("installed binary was not version-checked")
	}

	// Installed verifies without the network.
	srv.hits.Store(0)
	if _, err := inst.Installed(t.Context(), spec, host); err != nil {
		t.Errorf("Installed: %v", err)
	}
	if srv.hits.Load() != 0 {
		t.Error("Installed used the network")
	}
}

func TestInstallBz2(t *testing.T) {
	t.Parallel()
	archive := mustB64(t, goodBz2B64)
	srv := newArchiveServer(t, map[string][]byte{"faketool.bz2": archive})
	spec := testSpec(srv, host, "faketool.bz2", FormatBzip2, "", archive)
	got, err := newTestInstaller(t, srv, t.TempDir(), &contentDetector{}).Install(t.Context(), spec, host)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(got.Path)
	if !strings.HasPrefix(string(content), "faketool v1.2.3\n") {
		t.Errorf("content = %q", content)
	}
}

func TestInstallFailures(t *testing.T) {
	t.Parallel()
	goodZip := buildZip(t, zipMember{name: fakeEntry, body: fakeBinary(fakeVersion)})
	tests := []struct {
		name string
		// serve is what the server returns; pinned is what the manifest
		// pins (defaults to serve).
		serve, pinned []byte
		format        Format
		entry         string
		maxArchive    int64
		wantErr       string
		check         func(t *testing.T, err error)
	}{
		{
			name: "checksum mismatch", serve: goodZip, pinned: []byte("something else"), format: FormatZip, entry: fakeEntry,
			wantErr: "checksum mismatch",
			check: func(t *testing.T, err error) {
				t.Helper()
				ce, ok := errors.AsType[*ChecksumError](err)
				if !ok {
					t.Fatalf("not a *ChecksumError: %v", err)
				}
				if ce.Got != sum(goodZip) || ce.Want != sum([]byte("something else")) {
					t.Errorf("ChecksumError = %+v", ce)
				}
			},
		},
		{
			name: "corrupt archive with valid checksum", serve: []byte("PK\x03\x04 garbage"), format: FormatZip, entry: fakeEntry,
			wantErr: "zip",
		},
		{
			name: "corrupt bz2 with valid checksum", serve: corrupt(mustB64(t, goodBz2B64)), format: FormatBzip2,
			wantErr: "decompress",
		},
		{
			name: "zip-slip entry", format: FormatZip, entry: fakeEntry, wantErr: "unsafe path",
			serve: buildZip(t, zipMember{name: "../../evil", body: []byte("x")}, zipMember{name: fakeEntry, body: fakeBinary(fakeVersion)}),
		},
		{
			name: "archive without the binary", format: FormatZip, entry: fakeEntry, wantErr: "does not contain",
			serve: buildZip(t, zipMember{name: "other/file", body: []byte("x")}),
		},
		{
			name: "download over the size cap", serve: goodZip, format: FormatZip, entry: fakeEntry, maxArchive: 10,
			wantErr: "limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pinned := tt.pinned
			if pinned == nil {
				pinned = tt.serve
			}
			srv := newArchiveServer(t, map[string][]byte{"a": tt.serve})
			spec := testSpec(srv, host, "a", tt.format, tt.entry, pinned)
			dir := t.TempDir()
			inst, err := NewInstaller(InstallerOptions{Dir: dir, HTTPClient: srv.Client(), MaxArchiveBytes: tt.maxArchive})
			if err != nil {
				t.Fatal(err)
			}

			_, err = inst.Install(t.Context(), spec, host)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
			if tt.check != nil {
				tt.check(t, err)
			}

			// A failed install leaves nothing behind that could be resolved
			// or mistaken for a binary.
			for _, p := range listAll(t, dir) {
				base := filepath.Base(p)
				if info, err := os.Stat(filepath.Join(dir, p)); err == nil && info.IsDir() {
					continue
				}
				if base == spec.Binary(host.OS) || base == sidecarFileName || strings.Contains(base, tempPrefix) {
					t.Errorf("failed install left %s", p)
				}
			}
			if _, err := inst.Installed(t.Context(), spec, host); !errors.Is(err, ErrNotInstalled) {
				t.Errorf("Installed after failure: %v, want ErrNotInstalled", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "..", "evil")); err == nil {
				t.Error("an entry escaped the tools directory")
			}
		})
	}
}

func TestInstallHTTPErrorAndUnsupportedPlatform(t *testing.T) {
	t.Parallel()
	srv, spec := zipFixture(t)
	art := spec.Artifacts[host]
	art.URL = srv.URL + "/missing.zip"
	spec.Artifacts[host] = art
	inst := newTestInstaller(t, srv, t.TempDir(), nil)
	if _, err := inst.Install(t.Context(), spec, host); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("404: err = %v", err)
	}
	_, err := inst.Install(t.Context(), spec, Platform{OS: "plan9", Arch: "mips"})
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Errorf("unsupported platform: err = %v", err)
	}
}

func TestInstallIdempotent(t *testing.T) {
	t.Parallel()
	srv, spec := zipFixture(t)
	dir := t.TempDir()
	inst := newTestInstaller(t, srv, dir, &contentDetector{})

	first, err := inst.Install(t.Context(), spec, host)
	if err != nil {
		t.Fatal(err)
	}
	st1, _ := os.Stat(first.Path)

	second, err := inst.Install(t.Context(), spec, host)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Reused || second.Path != first.Path || second.SHA256 != first.SHA256 {
		t.Errorf("second install = %+v, first = %+v", second, first)
	}
	if srv.hits.Load() != 1 {
		t.Errorf("server hit %d times, want 1", srv.hits.Load())
	}
	if st2, _ := os.Stat(first.Path); !st2.ModTime().Equal(st1.ModTime()) {
		t.Error("an idempotent re-run rewrote the binary")
	}
	// A second Installer (a new process, in effect) also reuses it.
	third, err := newTestInstaller(t, srv, dir, nil).Install(t.Context(), spec, host)
	if err != nil || !third.Reused || srv.hits.Load() != 1 {
		t.Errorf("third install = %+v, %v, hits=%d", third, err, srv.hits.Load())
	}
}

func TestInstallRepairsDamagedInstall(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		damage func(t *testing.T, binPath string)
	}{
		{"binary modified", func(t *testing.T, p string) {
			t.Helper()
			if err := os.WriteFile(p, []byte("trojan"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"same size, different bytes", func(t *testing.T, p string) {
			t.Helper()
			b, _ := os.ReadFile(p)
			b[0] ^= 0xff
			if err := os.WriteFile(p, b, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"binary deleted", func(t *testing.T, p string) {
			t.Helper()
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
		}},
		{"sidecar corrupted", func(t *testing.T, p string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(filepath.Dir(p), sidecarFileName), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"sidecar deleted", func(t *testing.T, p string) {
			t.Helper()
			if err := os.Remove(filepath.Join(filepath.Dir(p), sidecarFileName)); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv, spec := zipFixture(t)
			inst := newTestInstaller(t, srv, t.TempDir(), &contentDetector{})
			first, err := inst.Install(t.Context(), spec, host)
			if err != nil {
				t.Fatal(err)
			}
			tt.damage(t, first.Path)

			if _, err := inst.Installed(t.Context(), spec, host); !errors.Is(err, ErrInstallInvalid) && !errors.Is(err, ErrNotInstalled) {
				t.Fatalf("Installed on damaged install: %v", err)
			}
			again, err := inst.Install(t.Context(), spec, host)
			if err != nil {
				t.Fatal(err)
			}
			if again.Reused || srv.hits.Load() != 2 {
				t.Errorf("Reused=%v hits=%d, want a fresh download", again.Reused, srv.hits.Load())
			}
			if content, _ := os.ReadFile(again.Path); !bytes.Equal(content, fakeBinary(fakeVersion)) {
				t.Errorf("repaired content = %q", content)
			}
		})
	}
}

func TestInstallPinChangeInvalidatesInstall(t *testing.T) {
	t.Parallel()
	srv, spec := zipFixture(t)
	inst := newTestInstaller(t, srv, t.TempDir(), nil)
	if _, err := inst.Install(t.Context(), spec, host); err != nil {
		t.Fatal(err)
	}
	// Same version string but a different pinned digest must not be trusted.
	art := spec.Artifacts[host]
	art.SHA256 = sum([]byte("other"))
	spec.Artifacts[host] = art
	if _, err := inst.Installed(t.Context(), spec, host); !errors.Is(err, ErrInstallInvalid) {
		t.Errorf("err = %v, want ErrInstallInvalid", err)
	}
}

func TestInstallForeignPlatformIsNotExecuted(t *testing.T) {
	t.Parallel()
	archive := buildZip(t, zipMember{name: fakeEntry, body: fakeBinary(fakeVersion)})
	srv := newArchiveServer(t, map[string][]byte{"a.zip": archive})
	foreign := Platform{OS: "windows", Arch: "arm64"}
	if foreign == host {
		foreign = Platform{OS: "linux", Arch: "arm64"}
	}
	spec := testSpec(srv, foreign, "a.zip", FormatZip, fakeEntry, archive)
	det := &contentDetector{}
	inst := newTestInstaller(t, srv, t.TempDir(), det)
	got, err := inst.Install(t.Context(), spec, foreign)
	if err != nil {
		t.Fatal(err)
	}
	if det.calls.Load() != 0 {
		t.Error("a foreign-platform binary was executed")
	}
	if want := spec.Binary(foreign.OS); filepath.Base(got.Path) != want {
		t.Errorf("binary name = %s, want %s", filepath.Base(got.Path), want)
	}
}

func TestInstallRejectsWrongVersionAfterInstall(t *testing.T) {
	t.Parallel()
	// The archive matches its pinned digest but contains another version:
	// the post-install check must refuse it.
	archive := buildZip(t, zipMember{name: fakeEntry, body: fakeBinary("9.9.9")})
	srv := newArchiveServer(t, map[string][]byte{"a.zip": archive})
	spec := testSpec(srv, host, "a.zip", FormatZip, fakeEntry, archive)
	_, err := newTestInstaller(t, srv, t.TempDir(), &contentDetector{}).Install(t.Context(), spec, host)
	if _, ok := errors.AsType[*VersionMismatchError](err); !ok {
		t.Fatalf("err = %v, want *VersionMismatchError", err)
	}
}

func TestInstallBaseURL(t *testing.T) {
	t.Parallel()
	archive := buildZip(t, zipMember{name: fakeEntry, body: fakeBinary(fakeVersion)})
	srv := newArchiveServer(t, map[string][]byte{"mirror/a.zip": archive})
	spec := testSpec(srv, host, "elsewhere/a.zip", FormatZip, fakeEntry, archive)
	// The pinned URL 404s; the mirror serves the same pinned bytes.
	inst, err := NewInstaller(InstallerOptions{Dir: t.TempDir(), HTTPClient: srv.Client(), BaseURL: srv.URL + "/mirror/"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inst.Install(t.Context(), spec, host); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentInstallsDownloadOnce(t *testing.T) {
	t.Parallel()
	srv, spec := zipFixture(t)
	dir := t.TempDir()

	const n = 8
	var wg sync.WaitGroup
	results := make([]Installed, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Separate Installers model separate workers sharing a directory.
			inst := newTestInstaller(t, srv, dir, &contentDetector{})
			results[i], errs[i] = inst.Install(t.Context(), spec, host)
		}()
	}
	wg.Wait()

	reused := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("install %d: %v", i, errs[i])
		}
		if results[i].Path != results[0].Path || results[i].SHA256 != results[0].SHA256 {
			t.Errorf("install %d differs: %+v vs %+v", i, results[i], results[0])
		}
		if results[i].Reused {
			reused++
		}
	}
	if srv.hits.Load() != 1 || reused != n-1 {
		t.Errorf("downloads = %d, reused = %d; want 1 and %d", srv.hits.Load(), reused, n-1)
	}
}

func TestInstallWaitsForLockAndHonorsContext(t *testing.T) {
	t.Parallel()
	srv, spec := zipFixture(t)
	dir := t.TempDir()
	inst := newTestInstaller(t, srv, dir, nil)

	release, err := acquireLock(t.Context(), filepath.Join(dir, lockFileName))
	if err != nil {
		t.Fatal(err)
	}

	// While another process holds the lock, Install blocks and gives up with
	// its context, without downloading.
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	if _, err := inst.Install(ctx, spec, host); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if srv.hits.Load() != 0 {
		t.Error("downloaded while the lock was held by someone else")
	}

	// Released: the same call now succeeds.
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if _, err := inst.Install(t.Context(), spec, host); err != nil {
		t.Fatal(err)
	}
}

func TestInstallCleansStaleTempFiles(t *testing.T) {
	t.Parallel()
	srv, spec := zipFixture(t)
	dir := t.TempDir()
	target := filepath.Join(dir, fakeName, fakeVersion, host.OS+"-"+host.Arch)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(target, tempPrefix+"dl-crashed")
	if err := os.WriteFile(stale, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newTestInstaller(t, srv, dir, nil).Install(t.Context(), spec, host); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale temp file survived: %v", err)
	}
}

func TestNewInstallerRequiresDir(t *testing.T) {
	t.Parallel()
	if _, err := NewInstaller(InstallerOptions{}); err == nil {
		t.Error("NewInstaller accepted an empty directory")
	}
}
