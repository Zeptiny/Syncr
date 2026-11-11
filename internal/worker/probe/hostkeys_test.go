package probe

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// sshServer serves SSH on a loopback port with the given host keys and
// records every authentication attempt (there must be none).
type sshServer struct {
	host  string
	port  int
	keys  []ssh.PublicKey
	auths atomic.Int32
	conns atomic.Int32
}

func newSSHServer(t *testing.T, signers ...ssh.Signer) *sshServer {
	t.Helper()
	srv := &sshServer{}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			return nil, errors.New("no")
		},
		AuthLogCallback: func(ssh.ConnMetadata, string, error) { srv.auths.Add(1) },
	}
	for _, s := range signers {
		cfg.AddHostKey(s)
		srv.keys = append(srv.keys, s.PublicKey())
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			srv.conns.Add(1)
			go func() {
				defer func() { _ = conn.Close() }()
				if sc, chans, reqs, err := ssh.NewServerConn(conn, cfg); err == nil {
					go ssh.DiscardRequests(reqs)
					go func() {
						for ch := range chans {
							_ = ch.Reject(ssh.Prohibited, "test")
						}
					}()
					_ = sc.Close()
				}
			}()
		}
	}()
	srv.host, srv.port = hostPort(t, ln)
	return srv
}

func ed25519Signer(t *testing.T) ssh.Signer {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func ecdsaSigner(t *testing.T) ssh.Signer {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func rsaSigner(t *testing.T) ssh.Signer {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// checkKeys compares the probe's entries with the server's keys, in order.
func checkKeys(t *testing.T, got []*syncrv1.SftpHostKey, want []ssh.PublicKey) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d keys %v, want %d", len(got), got, len(want))
	}
	for i, k := range want {
		g := got[i]
		raw, err := base64.StdEncoding.DecodeString(g.GetKey())
		if err != nil {
			t.Errorf("key %d is not padded standard base64: %v", i, err)
		}
		if g.GetAlgorithm() != k.Type() || !bytes.Equal(raw, k.Marshal()) || g.GetFingerprintSha256() != ssh.FingerprintSHA256(k) {
			t.Errorf("key %d = %v, want %s %s", i, g, k.Type(), ssh.FingerprintSHA256(k))
		}
		// The entry is what rclone's host_keys takes: "<algorithm> <key>"
		// parses as an authorized key with the same bytes.
		parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(g.GetAlgorithm() + " " + g.GetKey()))
		if err != nil || !bytes.Equal(parsed.Marshal(), k.Marshal()) {
			t.Errorf("key %d does not parse as a known_hosts key: %v", i, err)
		}
	}
}

func TestSSHScannerFindsEveryKeyType(t *testing.T) {
	t.Parallel()
	ed, ec := ed25519Signer(t), ecdsaSigner(t)
	srv := newSSHServer(t, ec, ed)

	keys, err := (&SSHScanner{}).HostKeys(t.Context(), srv.host, srv.port)
	if err != nil {
		t.Fatal(err)
	}
	// In the scanner's order (ed25519 first), whatever the server's.
	checkKeys(t, keys, []ssh.PublicKey{ed.PublicKey(), ec.PublicKey()})
	if n := srv.auths.Load(); n != 0 {
		t.Errorf("the scanner made %d authentication attempts; it must send none", n)
	}
	if n := srv.conns.Load(); n != int32(len(hostKeyAlgorithms())) {
		t.Errorf("%d connections, want one per algorithm (%d)", n, len(hostKeyAlgorithms()))
	}
}

func TestSSHScannerAsksForRSAOnce(t *testing.T) {
	t.Parallel()
	rs, ed := rsaSigner(t), ed25519Signer(t)
	srv := newSSHServer(t, rs, ed)
	keys, err := (&SSHScanner{}).HostKeys(t.Context(), "["+srv.host+"]", srv.port)
	if err != nil {
		t.Fatal(err)
	}
	checkKeys(t, keys, []ssh.PublicKey{ed.PublicKey(), rs.PublicKey()})
	// ed25519, three ECDSA curves, then RSA once (rsa-sha2-512 succeeds).
	if n := srv.conns.Load(); n != 5 {
		t.Errorf("%d connections, want 5", n)
	}
}

func TestSSHScannerThroughTheExecutor(t *testing.T) {
	t.Parallel()
	ed := ed25519Signer(t)
	srv := newSSHServer(t, ed)
	e, err := New(Config{}) // the default scanner
	if err != nil {
		t.Fatal(err)
	}
	res := e.Probe(t.Context(), &syncrv1.RunProbe{ProbeId: "p", Spec: &syncrv1.ProbeSpec{Op: &syncrv1.ProbeSpec_SftpHostKeys{
		SftpHostKeys: &syncrv1.ProbeSftpHostKeys{Host: srv.host, Port: int32(srv.port)},
	}}})
	if res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_OK {
		t.Fatalf("result = %v", res)
	}
	checkKeys(t, res.GetSftpHostKeys().GetKeys(), []ssh.PublicKey{ed.PublicKey()})
}

// listen returns a loopback listener whose connections serve runs.
func listen(t *testing.T, serve func(net.Conn)) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(c)
		}
	}()
	return hostPort(t, ln)
}

// hostPort returns the address ln listens on.
func hostPort(t *testing.T, ln net.Listener) (string, int) {
	t.Helper()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address %v", ln.Addr())
	}
	return addr.IP.String(), addr.Port
}

func TestSSHScannerFailures(t *testing.T) {
	t.Parallel()
	// A port nothing listens on: listen, then close.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, closedPort := hostPort(t, ln)
	_ = ln.Close()

	notSSHHost, notSSHPort := listen(t, func(c net.Conn) {
		_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
		_ = c.Close()
	})
	silentHost, silentPort := listen(t, func(c net.Conn) {
		t.Cleanup(func() { _ = c.Close() })
	})

	tests := []struct {
		name    string
		scanner *SSHScanner
		host    string
		port    int
		code    syncrv1.JobErrorCode
	}{
		{"connection refused", &SSHScanner{}, "127.0.0.1", closedPort, syncrv1.JobErrorCode_JOB_ERROR_CODE_TEMPORARY},
		{"not an SSH server", &SSHScanner{}, notSSHHost, notSSHPort, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED},
		{"silent server", &SSHScanner{DialTimeout: 100 * time.Millisecond}, silentHost, silentPort, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED},
		{"empty host", &SSHScanner{}, "", 22, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC},
		{"host with a space", &SSHScanner{}, "a b", 22, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC},
		{"host with a path", &SSHScanner{}, "host/x", 22, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC},
		{"bad IPv6", &SSHScanner{}, "fe80::zz", 22, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC},
		{"port too large", &SSHScanner{}, "127.0.0.1", 65536, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC},
		{"negative port", &SSHScanner{}, "127.0.0.1", -1, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			keys, err := tt.scanner.HostKeys(t.Context(), tt.host, tt.port)
			if err == nil || keys != nil {
				t.Fatalf("HostKeys() = %v, %v; want an error", keys, err)
			}
			if code := runner.CodeOf(err); code != tt.code {
				t.Errorf("code = %v (%v), want %v", code, err, tt.code)
			}
		})
	}
}

func TestSSHScannerStopsWithItsContext(t *testing.T) {
	t.Parallel()
	accepted := make(chan struct{}, 1)
	host, port := listen(t, func(c net.Conn) {
		t.Cleanup(func() { _ = c.Close() })
		accepted <- struct{}{}
	})
	ctx, cancel := context.WithCancelCause(t.Context())
	errStop := errors.New("probe cancelled")
	go func() {
		<-accepted
		cancel(errStop)
	}()
	_, err := (&SSHScanner{}).HostKeys(ctx, host, port)
	if !errors.Is(err, errStop) {
		t.Errorf("HostKeys() error = %v, want the context's cause", err)
	}
}

func TestSSHScannerDefaultPort(t *testing.T) {
	t.Parallel()
	var dialed string
	s := &SSHScanner{Dial: func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = address
		return nil, errors.New("no network in this test")
	}}
	if _, err := s.HostKeys(t.Context(), "sftp.example.com", 0); runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_TEMPORARY {
		t.Errorf("err = %v", err)
	}
	if dialed != net.JoinHostPort("sftp.example.com", strconv.Itoa(22)) {
		t.Errorf("dialed %q, want port 22", dialed)
	}
}
