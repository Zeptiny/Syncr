package httpserver

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func TestServeGracefulShutdown(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	srv := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
			_, _ = io.WriteString(w, "done")
		}),
	}
	ln := listen(t)
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, srv, ln, 5*time.Second) }()

	type result struct {
		body string
		err  error
	}
	resp := make(chan result, 1)
	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String(), nil)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			resp <- result{err: err}
			return
		}
		defer func() { _ = r.Body.Close() }()
		b, err := io.ReadAll(r.Body)
		resp <- result{body: string(b), err: err}
	}()

	<-started
	cancel() // shutdown begins while the request is in flight
	time.Sleep(50 * time.Millisecond)
	close(release)

	if r := <-resp; r.err != nil || r.body != "done" {
		t.Fatalf("in-flight request: body %q, err %v", r.body, r.err)
	}
	if err := <-served; err != nil {
		t.Fatalf("Serve() = %v, want nil", err)
	}
}

func TestServeShutdownTimeout(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	srv := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			close(started)
			<-r.Context().Done() // never finishes on its own
		}),
	}
	ln := listen(t)
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, srv, ln, 50*time.Millisecond) }()

	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String(), nil)
		if r, err := http.DefaultClient.Do(req); err == nil {
			_ = r.Body.Close()
		}
	}()
	<-started
	cancel()

	if err := <-served; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Serve() = %v, want deadline exceeded", err)
	}
}

func TestServeListenerFailure(t *testing.T) {
	t.Parallel()
	ln := listen(t)
	_ = ln.Close()
	err := Serve(t.Context(), &http.Server{ReadHeaderTimeout: time.Second}, ln, time.Second)
	if err == nil {
		t.Fatal("Serve() on a closed listener = nil, want error")
	}
}
