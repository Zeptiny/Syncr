//go:build integration

package rcloneenv

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/ftptest"
)

// TestIntegrationFTPValuesReachServerUnchanged sends a user and a password
// full of special characters to an FTP server (rclone serve ftp), which
// compares both exactly, and lists a path that looks like a connection
// string: the listing works only if all three arrive as they were given.
// FTP sends commands as lines, so the values hold no line break.
func TestIntegrationFTPValuesReachServerUnchanged(t *testing.T) {
	rcloneBinary(t)
	const user = `u'se"r, with:spaces;and=more #$HOME`
	const pass = "p:a's\"s,w o=r d;$HOME`x`{y}[z]\\ ünï"
	path := ",type=local,copy_links:/etc x"
	if runtime.GOOS == "windows" {
		// Windows forbids ":" and '"' in names; the rest stays.
		path = ",type=local,copy_links=etc x"
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, path, "found.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := ftptest.Start(t, dir, user, pass)

	list := func(def *syncrv1.RemoteDefinition) ([]string, []string, error) {
		t.Helper()
		env, err := NewBuilder(nil).Build(t.TempDir(), []*syncrv1.RemoteDefinition{def})
		if err != nil {
			t.Fatal(err)
		}
		return runRclone(t, env.Vars(), "lsf", "--retries=1", "--low-level-retries=1", "--", RemotePath(def.GetAlias(), path))
	}

	out, errOut, err := list(srv.Remote("box"))
	if err != nil || !slices.Equal(out, []string{"found.txt"}) {
		t.Fatalf("lsf = %q, %v (stderr %q); want the file below the path", out, err, errOut)
	}
	for _, l := range errOut {
		if strings.Contains(l, pass) {
			t.Errorf("rclone logged the password: %q", l)
		}
	}

	// The server does check: one character off fails, for either value.
	wrongPass := srv.Remote("box")
	wrongPass.Secrets["pass"] = strings.TrimSuffix(pass, "ï")
	wrongUser := srv.Remote("box")
	wrongUser.Options["user"] = strings.TrimSuffix(user, "E")
	for name, def := range map[string]*syncrv1.RemoteDefinition{"password": wrongPass, "user": wrongUser} {
		if out, _, err := list(def); err == nil {
			t.Errorf("a wrong %s was accepted: listed %q", name, out)
		}
	}
}
