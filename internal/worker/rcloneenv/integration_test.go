//go:build integration

package rcloneenv

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
)

// Integration tests run the real rclone (SYNCR_TEST_RCLONE) with environments
// built by this package.

func rcloneBinary(t *testing.T) string {
	t.Helper()
	path := os.Getenv("SYNCR_TEST_RCLONE")
	if path == "" {
		t.Skip("SYNCR_TEST_RCLONE not set")
	}
	return path
}

// runRclone runs rclone through the supervisor (minimal environment) with
// env and returns stdout lines, stderr lines and the error.
func runRclone(t *testing.T, env map[string]string, args ...string) (stdout, stderr []string, err error) {
	t.Helper()
	_, err = proc.New(proc.Options{}).Run(t.Context(), proc.Spec{
		Path:   rcloneBinary(t),
		Args:   args,
		Env:    env,
		Stdout: func(l proc.Line) { stdout = append(stdout, string(l.Data)) },
		Stderr: func(l proc.Line) { stderr = append(stderr, string(l.Data)) },
	})
	return stdout, stderr, err
}

func TestIntegrationObscureInteropsWithRclone(t *testing.T) {
	rcloneBinary(t)
	for _, plain := range []string{"simple", "pa:ss,w\"o rd'", "ünïcödé 🔑", strings.Repeat("x", 300)} {
		o, err := Obscure(plain)
		if err != nil {
			t.Fatal(err)
		}
		out, errOut, err := runRclone(t, nil, "reveal", "--", o)
		if err != nil || len(out) != 1 || out[0] != plain {
			t.Errorf("rclone reveal(Obscure(%q)) = %q, %v (stderr %q)", plain, out, err, errOut)
		}

		out, errOut, err = runRclone(t, nil, "obscure", "--", plain)
		if err != nil || len(out) != 1 {
			t.Fatalf("rclone obscure: %q, %v (stderr %q)", out, err, errOut)
		}
		if got := reveal(t, out[0]); got != plain {
			t.Errorf("reveal(rclone obscure %q) = %q", plain, got)
		}
	}
}

func TestIntegrationIsolatedFromHost(t *testing.T) {
	rcloneBinary(t)
	// A desktop user's rclone setup: a config file in HOME, and RCLONE_*
	// variables in the worker's own environment.
	home := t.TempDir()
	confDir := filepath.Join(home, ".config", "rclone")
	if err := os.MkdirAll(confDir, 0o700); err != nil {
		t.Fatal(err)
	}
	hostConf := filepath.Join(confDir, "rclone.conf")
	if err := os.WriteFile(hostConf, []byte("[homeonly]\ntype = memory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("RCLONE_CONFIG", hostConf)
	t.Setenv("RCLONE_CONFIG_LEAKED_TYPE", "memory")

	dir := t.TempDir()
	env, err := NewBuilder(nil).Build(dir, []*syncrv1.RemoteDefinition{{Alias: "jobmem", BackendType: "memory"}})
	if err != nil {
		t.Fatal(err)
	}

	out, errOut, err := runRclone(t, env.Vars(), "config", "file")
	if err != nil || !slices.ContainsFunc(out, func(l string) bool { return strings.TrimSpace(l) == filepath.Join(dir, "rclone.conf") }) {
		t.Errorf("rclone config file = %q, %v (stderr %q); want the per-job file", out, err, errOut)
	}

	// The job's remote works; the host's remotes are unknown.
	if _, errOut, err := runRclone(t, env.Vars(), "lsf", "jobmem:"); err != nil {
		t.Errorf("lsf jobmem: %v (stderr %q)", err, errOut)
	}
	for _, r := range []string{"homeonly:", "leaked:"} {
		if _, _, err := runRclone(t, env.Vars(), "lsf", r); err == nil {
			t.Errorf("rclone could use host remote %s", r)
		}
	}
	out, _, _ = runRclone(t, env.Vars(), "listremotes")
	if slices.ContainsFunc(out, func(l string) bool { return strings.Contains(l, "homeonly") || strings.Contains(l, "leaked") }) {
		t.Errorf("listremotes shows host remotes: %q", out)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "rclone.conf")); err != nil || len(b) != 0 {
		t.Errorf("per-job config = %q, %v; want it empty", b, err)
	}
}

// TestIntegrationValuesReachBackendUnchanged sends credentials full of
// special characters to a WebDAV server and checks what arrives, and that a
// path that looks like a connection string is just a path.
func TestIntegrationValuesReachBackendUnchanged(t *testing.T) {
	rcloneBinary(t)
	const user = "u'se\"r,with spaces\nand newline"
	const pass = "p:a's\"s,w o\nr=d;$HOME`x`"

	var mu sync.Mutex
	type seen struct{ user, pass, path string }
	var requests []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ := r.BasicAuth()
		mu.Lock()
		requests = append(requests, seen{u, p, r.URL.Path})
		mu.Unlock()
		http.NotFound(w, r)
	}))
	defer srv.Close()

	env, err := NewBuilder(nil).Build(t.TempDir(), []*syncrv1.RemoteDefinition{{
		Alias:       "dav",
		BackendType: "webdav",
		Options:     map[string]string{"url": srv.URL + "/root", "vendor": "other", "user": user},
		Secrets:     map[string]string{"pass": pass},
		ObscureKeys: []string{"pass"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	const path = ",type=local,copy_links:/etc x"
	_, errOut, err := runRclone(t, env.Vars(), "lsf", "--retries=1", "--low-level-retries=1", "--", RemotePath("dav", path))
	if err == nil {
		t.Fatal("lsf on an empty server succeeded")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) == 0 {
		t.Fatalf("rclone made no request (stderr %q)", errOut)
	}
	for _, r := range requests {
		if r.user != user || r.pass != pass {
			t.Errorf("server saw credentials %q / %q, want %q / %q", r.user, r.pass, user, pass)
		}
		if r.path != "/root/"+path && r.path != "/root/"+path+"/" {
			t.Errorf("server saw path %q, want %q", r.path, "/root/"+path)
		}
	}
	for _, l := range errOut {
		if strings.Contains(l, pass) {
			t.Errorf("rclone logged the password: %q", l)
		}
	}
}
