//go:build integration

package rcloneenv

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"maps"
	"net"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/sftp"
)

// sshServer serves SSH on a loopback port and counts the logins of user
// with the public key want, the only credential it accepts. Logged-in
// clients get no channels, so rclone fails right after authenticating.
func sshServer(t *testing.T, user string, want ssh.PublicKey) (addr string, logins *atomic.Int32) {
	t.Helper()
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if c.User() == user && bytes.Equal(k.Marshal(), want.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	logins = new(atomic.Int32)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				// NewServerConn returns without an error only once the
				// client has proven it holds the private key.
				sconn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				logins.Add(1)
				go ssh.DiscardRequests(reqs)
				for ch := range chans {
					_ = ch.Reject(ssh.Prohibited, "test server")
				}
				_ = sconn.Close()
			}()
		}
	}()
	return ln.Addr().String(), logins
}

// TestIntegrationSFTPPrivateKey maps a private key pasted with its line
// breaks through the sftp descriptor, as the controller does, and checks
// that rclone parses it and signs in with it.
func TestIntegrationSFTPPrivateKey(t *testing.T) {
	rcloneBinary(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(block))
	if !strings.HasPrefix(keyPEM, "-----BEGIN OPENSSH PRIVATE KEY-----\n") || strings.Count(keyPEM, "\n") < 3 {
		t.Fatalf("not a multi-line OpenSSH key: %q", keyPEM)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	const user = "syncr"
	addr, logins := sshServer(t, user, sshPub)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}

	cfg := backend.Config{
		// shell_type none: rclone would otherwise probe for a shell.
		Options: map[string]string{"host": host, "port": port, "user": user, "shell_type": "none"},
		Secrets: map[string]string{"key_pem": keyPEM},
	}
	if err := (sftp.Type{}).Validate(cfg); err != nil {
		t.Fatal(err)
	}
	r, err := sftp.Type{}.ToRclone(cfg)
	if err != nil {
		t.Fatal(err)
	}
	def := func(key string) *syncrv1.RemoteDefinition {
		secrets := maps.Clone(r.Secrets)
		secrets["key_pem"] = key
		return &syncrv1.RemoteDefinition{
			Alias: "box", BackendType: r.Backend,
			Options: maps.Clone(r.Options), Secrets: secrets, ObscureKeys: slices.Clone(r.ObscureKeys),
		}
	}
	lsf := func(d *syncrv1.RemoteDefinition) []string {
		t.Helper()
		env, err := NewBuilder(nil).Build(t.TempDir(), []*syncrv1.RemoteDefinition{d})
		if err != nil {
			t.Fatal(err)
		}
		_, stderr, err := runRclone(t, env.Vars(), "lsf", "--retries=1", "--low-level-retries=1", RemotePath("box", ""))
		if err == nil {
			t.Fatal("lsf succeeded on a server without sftp")
		}
		return stderr
	}
	formatErr := func(stderr []string) bool {
		return slices.ContainsFunc(stderr, func(l string) bool { return strings.Contains(l, "pem key not formatted properly") })
	}

	// The descriptor's value: rclone reads the key and signs in with it.
	stderr := lsf(def(r.Secrets["key_pem"]))
	if formatErr(stderr) || logins.Load() == 0 {
		t.Errorf("rclone did not sign in with the key (%d logins); stderr %q", logins.Load(), stderr)
	}

	// The key as pasted is what rclone cannot read: the escaping is needed.
	before := logins.Load()
	if stderr := lsf(def(keyPEM)); !formatErr(stderr) || logins.Load() != before {
		t.Errorf("rclone accepted a key_pem with raw line breaks; stderr %q", stderr)
	}
}
