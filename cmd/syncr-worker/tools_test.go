package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/worker/tools"
)

// fakeRelease serves one zip over TLS and returns a manifest pinning it for
// linux/riscv64, a platform no test machine runs, so fetched binaries are
// never executed.
func fakeRelease(t *testing.T, pinOverride string) (toolsOverrides, string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("faketool-1/faketool")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("faketool v1\n"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	archive := buf.Bytes()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) }))
	t.Cleanup(srv.Close)

	digest := sha256.Sum256(archive)
	pin := hex.EncodeToString(digest[:])
	if pinOverride != "" {
		pin = pinOverride
	}
	m := tools.Manifest{Tools: []tools.Spec{{
		Name: "faketool", Version: "1", VersionArgs: []string{"version"}, VersionPattern: `^faketool v(\S+)`,
		Artifacts: map[tools.Platform]tools.Artifact{
			{OS: "linux", Arch: "riscv64"}: {URL: srv.URL + "/faketool.zip", SHA256: pin, Format: tools.FormatZip, Entry: "faketool-1/faketool"},
		},
	}}}
	return toolsOverrides{manifest: &m, httpClient: srv.Client()}, pin
}

func TestToolsFetch(t *testing.T) {
	t.Parallel()
	ov, _ := fakeRelease(t, "")
	dir := t.TempDir()
	args := []string{"tools", "fetch", "--dir", dir, "--os", "linux", "--arch", "riscv64"}

	e, stdout, stderr := testEnv(nil)
	e.tools = ov
	if code := run(t.Context(), args, e); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	fields := strings.Fields(stdout.String())
	if len(fields) != 5 || fields[0] != "faketool" || fields[1] != "1" || fields[2] != "linux/riscv64" || fields[3] != "installed" {
		t.Fatalf("output = %q", stdout)
	}
	path := fields[4]
	if want := filepath.Join(dir, "faketool", "1", "linux-riscv64", "faketool"); path != want {
		t.Errorf("path = %s, want %s", path, want)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error(err)
	}

	// Re-running is idempotent.
	e, stdout, stderr = testEnv(nil)
	e.tools = ov
	if code := run(t.Context(), args, e); code != exitOK {
		t.Fatalf("rerun exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout.String(), "up-to-date") {
		t.Errorf("rerun output = %q", stdout)
	}
}

func TestToolsFetchErrors(t *testing.T) {
	t.Parallel()
	okOverrides, _ := fakeRelease(t, "")
	badPin, _ := fakeRelease(t, strings.Repeat("0", 64))
	tests := []struct {
		name       string
		ov         toolsOverrides
		args       []string
		wantCode   int
		wantStderr string
	}{
		{name: "checksum mismatch", ov: badPin, args: []string{"--os", "linux", "--arch", "riscv64"}, wantCode: exitError, wantStderr: "checksum mismatch"},
		{name: "unsupported platform", ov: okOverrides, args: []string{"--os", "plan9", "--arch", "mips"}, wantCode: exitError, wantStderr: "unsupported platform"},
		{name: "stray argument", ov: okOverrides, args: []string{"extra"}, wantCode: exitUsage, wantStderr: "unexpected arguments"},
		{name: "bad flag", ov: okOverrides, args: []string{"--bogus"}, wantCode: exitUsage, wantStderr: "flag provided but not defined"},
		{name: "help", ov: okOverrides, args: []string{"-h"}, wantCode: exitOK, wantStderr: "-dir"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e, _, stderr := testEnv(nil)
			e.tools = tt.ov
			args := append([]string{"tools", "fetch", "--dir", t.TempDir()}, tt.args...)
			if tt.name == "help" {
				args = []string{"tools", "fetch", "-h"}
			}
			if code := run(t.Context(), args, e); code != tt.wantCode {
				t.Errorf("exit %d, want %d (stderr: %s)", code, tt.wantCode, stderr)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr %q does not contain %q", stderr, tt.wantStderr)
			}
		})
	}
}

func TestToolsStatusWithNothingInstalled(t *testing.T) {
	t.Parallel()
	ov, _ := fakeRelease(t, "")
	e, stdout, stderr := testEnv(nil)
	e.tools = ov
	code := run(t.Context(), []string{"tools", "status", "--dir", t.TempDir()}, e)
	if code != exitError {
		t.Errorf("exit %d, want %d", code, exitError)
	}
	for _, want := range []string{"faketool (pinned 1)", "configured", "managed", "path", "=> none"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stderr.String(), "no verified binary for: faketool") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestToolsDispatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStderr string
	}{
		{"no subcommand", []string{"tools"}, exitUsage, "fetch"},
		{"help", []string{"tools", "help"}, exitOK, "status"},
		{"unknown", []string{"tools", "nope"}, exitUsage, `unknown tools subcommand "nope"`},
		{"listed in top-level help", []string{"help"}, exitOK, "tools"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e, _, stderr := testEnv(nil)
			if code := run(t.Context(), tt.args, e); code != tt.wantCode {
				t.Errorf("exit %d, want %d", code, tt.wantCode)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr %q does not contain %q", stderr, tt.wantStderr)
			}
		})
	}
}

func TestToolsDir(t *testing.T) {
	t.Parallel()
	e, _, _ := testEnv(map[string]string{toolsDirEnv: "/from/env"})
	if got, _ := toolsDir(e, "/from/flag"); got != "/from/flag" {
		t.Errorf("flag: %s", got)
	}
	if got, _ := toolsDir(e, ""); got != "/from/env" {
		t.Errorf("env: %s", got)
	}
	// With neither, it is under the worker data directory (location depends
	// on the user running the tests).
	e, _, _ = testEnv(map[string]string{"ProgramData": `C:\ProgramData`, "HOME": t.TempDir()})
	if got, err := toolsDir(e, ""); err == nil && filepath.Base(got) != "tools" {
		t.Errorf("default: %s", got)
	}
}
