// Package ftptest serves a folder over FTP for integration tests, with the
// pinned rclone ("rclone serve ftp", the binary in SYNCR_TEST_RCLONE), so
// FTP remotes are tested against a real server without Docker.
//
// The server listens on 127.0.0.1, on a port picked free just before it
// starts, opens its passive data connections on ports the OS picks, and
// stops when the test ends. It reads no rclone config of the host's. Like
// any rclone serve, it caches directory listings: a test that changes the
// folder directly, not through the server, starts it with NoDirCache.
package ftptest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// startTimeout bounds how long Start waits for the server to accept
// connections.
const startTimeout = 30 * time.Second

// Server is a running "rclone serve ftp".
type Server struct {
	// Host and Port are where the server listens.
	Host, Port string
	// User and Pass are the only credentials it accepts.
	User, Pass string
	// Dir is the folder it serves: the root of every path on the remote.
	Dir string

	log *lockedBuffer
}

// Option changes how Start runs the server.
type Option func(*options)

type options struct {
	args []string // extra arguments of rclone serve ftp
}

// NoDirCache makes the server list the folder afresh on every request
// (--dir-cache-time 0s), so a test may change the folder directly between
// runs. It makes every request walk the folder again: fine for a few
// files, far too slow for a restic repository and its 256 data folders.
func NoDirCache() Option {
	return func(o *options) { o.args = append(o.args, "--dir-cache-time", "0s") }
}

// Start serves dir over FTP and returns the running server, which accepts
// only user and pass; it stops when t ends. pass must not be empty (rclone's
// server would accept any password). It skips the test when
// SYNCR_TEST_RCLONE is not set.
func Start(t testing.TB, dir, user, pass string, opts ...Option) *Server {
	t.Helper()
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	bin := os.Getenv("SYNCR_TEST_RCLONE")
	if bin == "" {
		t.Skip("SYNCR_TEST_RCLONE not set")
	}
	if pass == "" {
		t.Fatal("ftptest: an empty pass would make the server accept any password")
	}
	conf := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(conf, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The port is free when picked, but another process could take it
	// before rclone listens on it: then rclone exits, and Start tries
	// another one.
	var err error
	for range 3 {
		var s *Server
		if s, err = start(t, bin, conf, dir, user, pass, o.args); err == nil {
			return s
		}
		if !errors.Is(err, errExited) {
			break
		}
	}
	t.Fatalf("ftptest: %v", err)
	return nil
}

// start runs one server on a free port and waits until it accepts
// connections. The server is stopped when t ends, or at once on an error.
func start(t testing.TB, bin, conf, dir, user, pass string, extra []string) (*Server, error) {
	port, err := freePort(t.Context())
	if err != nil {
		return nil, err
	}
	s := &Server{Host: "127.0.0.1", Port: port, User: user, Pass: pass, Dir: dir, log: &lockedBuffer{}}
	ctx, cancel := context.WithCancel(context.Background())
	//nolint:gosec // G204: a test helper running the pinned rclone with the test's own arguments
	cmd := exec.CommandContext(ctx, bin, append([]string{"serve", "ftp", dir,
		"--addr", net.JoinHostPort(s.Host, s.Port),
		// goftp picks min+rand(max-min): "0-1" is always port 0, a port
		// the OS picks (and reports in its PASV/EPSV reply).
		"--passive-port", "0-1",
		"--user", user, "--pass", pass,
		"--config", conf,
	}, extra...)...)
	cmd.Env = environ()
	cmd.Stdout, cmd.Stderr = s.log, s.log
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start rclone serve ftp: %w", err)
	}
	exited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()
	stop := func() {
		cancel()
		<-exited
	}

	if err := s.waitReady(t.Context(), exited); err != nil {
		stop()
		if errors.Is(err, errExited) && waitErr != nil {
			err = fmt.Errorf("%w: %w: %s", err, waitErr, s.log.String())
		} else if errors.Is(err, errExited) {
			err = fmt.Errorf("%w: %s", err, s.log.String())
		}
		return nil, err
	}
	t.Cleanup(func() {
		stop()
		if t.Failed() {
			t.Logf("rclone serve ftp log:\n%s", s.log.String())
		}
	})
	return s, nil
}

var errExited = errors.New("rclone serve ftp exited")

// waitReady waits until the server accepts a connection, the process
// exits (errExited), or startTimeout passes.
func (s *Server) waitReady(ctx context.Context, exited <-chan struct{}) error {
	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	var d net.Dialer
	for {
		c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(s.Host, s.Port))
		if err == nil {
			return c.Close()
		}
		select {
		case <-exited:
			return errExited
		case <-ctx.Done():
			return fmt.Errorf("rclone serve ftp is not listening on port %s after %v: %w: %s", s.Port, startTimeout, ctx.Err(), s.log.String())
		case <-tick.C:
		}
	}
}

// Remote returns an FTP remote for the server under alias, as the
// controller defines one: host, port and user as options, and the password
// as a secret rclone gets obscured.
func (s *Server) Remote(alias string) *syncrv1.RemoteDefinition {
	return &syncrv1.RemoteDefinition{
		Alias:       alias,
		BackendType: "ftp",
		Options:     map[string]string{"host": s.Host, "port": s.Port, "user": s.User},
		Secrets:     map[string]string{"pass": s.Pass},
		ObscureKeys: []string{"pass"},
	}
}

// freePort returns a TCP port on 127.0.0.1 that was free a moment ago.
func freePort(ctx context.Context) (string, error) {
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("pick a free port: %w", err)
	}
	addr, ok := l.Addr().(*net.TCPAddr)
	if err := l.Close(); err != nil {
		return "", fmt.Errorf("pick a free port: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("pick a free port: unexpected address %v", l.Addr())
	}
	return strconv.Itoa(addr.Port), nil
}

// environ is the test process's environment without RCLONE_* variables,
// which would change how the server behaves.
func environ() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "RCLONE_") {
			env = append(env, kv)
		}
	}
	return env
}

// lockedBuffer collects the server's output, written by the exec package's
// copying goroutines while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
