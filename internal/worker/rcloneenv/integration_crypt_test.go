//go:build integration

package rcloneenv

import (
	"bytes"
	"crypto/rand"
	"errors"
	"maps"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
)

// Crypt remotes with the real rclone (SYNCR_TEST_RCLONE), defined only
// through the environment this package builds. A memory remote keeps its
// data inside one rclone process, so the tests that write with one
// process and read with another wrap an SFTP server instead (the image of
// deploy/docker-compose.dev.yml, as the runner's integration tests use).

const sftpImage = "atmoz/sftp:alpine"

// Passwords of the tests' crypt remotes.
const (
	rightPassword = `right pa:ss"w,o'rd`
	cryptSalt     = "salt for the names"
	wrongPassword = `wrong pa:ss"w,o'rd`
)

// rcloneExit runs rclone like runRclone and returns its exit code instead
// of an error; a run that could not start fails the test.
func rcloneExit(t *testing.T, env map[string]string, args ...string) (stdout, stderr []string, exit int) {
	t.Helper()
	res, err := proc.New(proc.Options{}).Run(t.Context(), proc.Spec{
		Path:   rcloneBinary(t),
		Args:   args,
		Env:    env,
		Stdout: func(l proc.Line) { stdout = append(stdout, string(l.Data)) },
		Stderr: func(l proc.Line) { stderr = append(stderr, string(l.Data)) },
	})
	if _, ok := errors.AsType[*proc.ExitError](err); err != nil && !ok {
		t.Fatalf("rclone %q: %v", args, err)
	}
	return stdout, stderr, res.ExitCode
}

// mustRclone runs rclone and fails the test unless it succeeds.
func mustRclone(t *testing.T, env map[string]string, args ...string) []string {
	t.Helper()
	out, errOut, code := rcloneExit(t, env, args...)
	if code != 0 {
		t.Fatalf("rclone %q: exit code %d: %q", args, code, errOut)
	}
	return out
}

// cryptDefs returns the remote "vault", a crypt remote that stores its
// data at path on store, with password and the file-name mode, and store.
func cryptDefs(store *syncrv1.RemoteDefinition, path, password, mode string) []*syncrv1.RemoteDefinition {
	return []*syncrv1.RemoteDefinition{store, {
		Alias:       "vault",
		BackendType: "crypt",
		Wrapped:     &syncrv1.Location{Remote: store.GetAlias(), Path: path},
		Options:     map[string]string{"filename_encryption": mode, "directory_name_encryption": "true"},
		Secrets:     map[string]string{"password": password, "password2": cryptSalt},
		ObscureKeys: []string{"password", "password2"},
	}}
}

// buildEnv returns the variables of an rclone child for remotes.
func buildEnv(t *testing.T, remotes []*syncrv1.RemoteDefinition) map[string]string {
	t.Helper()
	env, err := NewBuilder(nil).Build(t.TempDir(), remotes)
	if err != nil {
		t.Fatal(err)
	}
	return env.Vars()
}

// TestIntegrationCryptOverMemory: one rclone process opens a crypt remote
// over a memory remote, both defined by the environment: the crypt
// remote's "remote" (another alias of the job) resolves to the memory
// remote, and its passwords arrive obscured. rclone's crypt commands
// encode and decode a name with the remote's keys, so only the right
// password turns the encrypted name back.
func TestIntegrationCryptOverMemory(t *testing.T) {
	rcloneBinary(t)
	mem := &syncrv1.RemoteDefinition{Alias: "mem", BackendType: "memory"}
	const name = "photos/a b,c'd.txt"

	right := buildEnv(t, cryptDefs(mem, "bucket/vault", rightPassword, "standard"))
	if got := right["RCLONE_CONFIG_VAULT_REMOTE"]; got != "mem:bucket/vault" {
		t.Fatalf("REMOTE = %q", got)
	}
	out := mustRclone(t, right, "backend", "encode", "vault:", name)
	if len(out) != 1 {
		t.Fatalf("encode = %q", out)
	}
	encoded := strings.TrimSpace(out[0])
	segments := strings.Split(encoded, "/")
	if len(segments) != 2 || segments[0] == "photos" || strings.Contains(encoded, "a b") {
		t.Errorf("encoded name = %q, want two encrypted segments", encoded)
	}
	if got := mustRclone(t, right, "backend", "decode", "vault:", encoded); len(got) != 1 || strings.TrimSpace(got[0]) != name {
		t.Errorf("decode with the right password = %q, want %q", got, name)
	}

	wrong := buildEnv(t, cryptDefs(mem, "bucket/vault", wrongPassword, "standard"))
	if got, errOut, code := rcloneExit(t, wrong, "backend", "decode", "vault:", encoded); code == 0 || !slices.ContainsFunc(errOut, func(l string) bool {
		return strings.Contains(l, "failed to decrypt")
	}) {
		t.Errorf("decode with the wrong password = %q, exit code %d (stderr %q), want a failure", got, code, errOut)
	}

	// The other options reach rclone too: without name encryption, names
	// keep their form and get the suffix.
	off := buildEnv(t, cryptDefs(mem, "bucket/vault", rightPassword, "off"))
	if got := mustRclone(t, off, "backend", "encode", "vault:", name); len(got) != 1 || strings.TrimSpace(got[0]) != name+".bin" {
		t.Errorf("encode without name encryption = %q", got)
	}
}

// startSFTP starts an SFTP server and returns the remote "box" for it,
// whose user may write in "upload". It skips where Linux containers
// cannot run.
func startSFTP(t *testing.T) *syncrv1.RemoteDefinition {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("skipping: testcontainers needs Linux containers, and Windows CI runners only run Windows containers")
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)
	// atmoz/sftp splits its argument on ":", so the password avoids it.
	const user, pass = "syncr", `p'a"s,s w$rd`
	ctr, err := testcontainers.Run(t.Context(), sftpImage,
		testcontainers.WithCmd(user+":"+pass+":1001:1001:upload"),
		testcontainers.WithExposedPorts("22/tcp"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("22/tcp").WithStartupTimeout(time.Minute)),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatal(err)
	}
	host, err := ctr.Host(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	port, err := ctr.MappedPort(t.Context(), "22/tcp")
	if err != nil {
		t.Fatal(err)
	}
	return &syncrv1.RemoteDefinition{
		Alias:       "box",
		BackendType: "sftp",
		// shell_type none: the chrooted server has no shell for hash
		// commands, and rclone would otherwise probe for one.
		Options:     map[string]string{"host": host, "port": port.Port(), "user": user, "shell_type": "none"},
		Secrets:     map[string]string{"pass": pass},
		ObscureKeys: []string{"pass"},
	}
}

// files writes random files below dir and returns their contents by
// slash-separated path.
func files(t *testing.T, dir string, sizes map[string]int) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for name, size := range sizes {
		data := make([]byte, size)
		_, _ = rand.Read(data)
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		out[name] = data
	}
	return out
}

// readTree returns the files below dir by slash-separated path.
func readTree(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)], err = os.ReadFile(p)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func containsLine(lines []string, sub string) bool {
	return slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, sub) })
}

// TestIntegrationCryptSFTP writes through a crypt remote over an SFTP
// server and reads it back in other rclone processes, for each file-name
// mode: the data comes back byte for byte, and the server holds only
// encrypted data (and encrypted names, unless names are not encrypted).
// With the wrong password (the worker always sets strict_names), nothing
// is ever read and a sync deletes nothing: with encrypted names, listing
// fails ("undecryptable") instead of looking empty; with obfuscated names
// it lists other names and reading fails; without name encryption it
// lists the real names and reading fails ("bad password?").
func TestIntegrationCryptSFTP(t *testing.T) {
	rcloneBinary(t)
	box := startSFTP(t)
	sizes := map[string]int{"a.txt": 100, "dir/b.bin": 300_000, "dir/sp ace,'q.txt": 64}
	for _, mode := range []string{"standard", "obfuscate", "off"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			folder := "upload/" + mode + " vault, 'x'"
			right := buildEnv(t, cryptDefs(box, folder, rightPassword, mode))
			wrong := buildEnv(t, cryptDefs(box, folder, wrongPassword, mode))
			plain := buildEnv(t, []*syncrv1.RemoteDefinition{box})

			src, back := t.TempDir(), t.TempDir()
			want := files(t, src, sizes)
			mustRclone(t, right, "copy", "--", src, "vault:")
			mustRclone(t, right, "copy", "--", "vault:", back)
			if got := readTree(t, back); !maps.EqualFunc(got, want, bytes.Equal) {
				t.Errorf("copied back %v, want %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
			}

			// What the SFTP server holds: rclone's encrypted files, under
			// encrypted names unless names are not encrypted.
			raw := t.TempDir()
			mustRclone(t, plain, "copy", "--", RemotePath("box", folder), raw)
			stored := readTree(t, raw)
			if len(stored) != len(sizes) {
				t.Errorf("the server holds %q, want %d files", slices.Sorted(maps.Keys(stored)), len(sizes))
			}
			plainNames := []string{"dir", "a.txt", "b.bin", "sp ace,'q.txt"}
			for name, data := range stored {
				segments := strings.Split(name, "/")
				switch {
				case mode == "off" && !strings.HasSuffix(name, ".bin"):
					t.Errorf("stored name %q lacks the suffix", name)
				case mode != "off" && slices.ContainsFunc(segments, func(s string) bool { return slices.Contains(plainNames, s) }):
					t.Errorf("stored name %q is not encrypted", name)
				}
				if !bytes.HasPrefix(data, []byte("RCLONE\x00\x00")) {
					t.Errorf("stored file %q is not an rclone crypt file", name)
				}
				for orig, content := range want {
					if bytes.Contains(data, content[:32]) {
						t.Errorf("stored file %q holds the plain content of %s", name, orig)
					}
				}
			}

			// The wrong password. A listing of the root never looks empty.
			out, errOut, code := rcloneExit(t, wrong, "lsf", "-R", "--", "vault:")
			t.Logf("lsf -R vault: with the wrong password: exit code %d, %q, stderr %q", code, out, errOut)
			switch mode {
			case "standard":
				if code == 0 || len(out) != 0 || !containsLine(errOut, "undecryptable") {
					t.Errorf("lsf with the wrong password = %q, exit code %d (stderr %q), want a failure naming undecryptable names", out, code, errOut)
				}
			case "obfuscate":
				for _, l := range out {
					if slices.Contains(plainNames, strings.TrimSuffix(pathpkg.Base(l), "/")) {
						t.Errorf("lsf with the wrong password decrypted %q", l)
					}
				}
			case "off":
				if code != 0 || !slices.Contains(out, "a.txt") {
					t.Errorf("lsf with the wrong password = %q, exit code %d (stderr %q), want the real names", out, code, errOut)
				}
			}
			// A folder below it: with encrypted names its name encrypts to
			// another one, which does not exist.
			out, errOut, code = rcloneExit(t, wrong, "lsf", "--", "vault:dir")
			t.Logf("lsf vault:dir with the wrong password: exit code %d, %q, stderr %q", code, out, errOut)
			if mode != "off" && (code != 3 || len(out) != 0 || !containsLine(errOut, "directory not found")) {
				t.Errorf("lsf of a folder with the wrong password = %q, exit code %d, want directory not found (exit code 3)", out, code)
			}

			// Nothing is read: copying fails, and a sync into a folder
			// deletes nothing, from the root or from a folder.
			_, errOut, code = rcloneExit(t, wrong, "copy", "--", "vault:", t.TempDir())
			if code == 0 || (mode != "standard" && !containsLine(errOut, "bad password")) {
				t.Errorf("copy with the wrong password: exit code %d (stderr %q), want a failure (bad password, unless names are encrypted)", code, errOut)
			}
			for _, from := range []string{"vault:", "vault:dir"} {
				dst := t.TempDir()
				kept := files(t, dst, map[string]int{"keep.txt": 10, "a.txt": 7, "b.bin": 5, "dir/other.bin": 20})
				if _, errOut, code := rcloneExit(t, wrong, "sync", "--", from, dst); code == 0 {
					t.Errorf("sync from %s with the wrong password succeeded (stderr %q)", from, errOut)
				}
				if got := readTree(t, dst); !maps.EqualFunc(got, kept, bytes.Equal) {
					t.Errorf("sync from %s with the wrong password changed the destination: %v, want %v",
						from, slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(kept)))
				}
			}
		})
	}
}
