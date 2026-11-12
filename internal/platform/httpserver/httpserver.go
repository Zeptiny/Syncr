// Package httpserver runs an *http.Server with context-driven graceful
// shutdown.
//
// Allowed dependencies: the standard library only.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Serve serves srv on ln until ctx is done, then shuts down gracefully: it
// stops accepting connections and waits up to shutdownTimeout for in-flight
// requests before closing the remaining connections.
//
// Serve returns nil after a clean shutdown, or the error that stopped the
// server. It always closes ln.
func Serve(ctx context.Context, srv *http.Server, ln net.Listener, shutdownTimeout time.Duration) error {
	return serve(ctx, srv, ln, func() error { return srv.Serve(ln) }, shutdownTimeout)
}

// ServeTLS is Serve over TLS with the PEM certificate chain and key in
// certFile and keyFile (see http.Server.ServeTLS: srv.TLSConfig, if set, is
// used as the base configuration, and HTTP/2 is negotiated if srv.Protocols
// allows it). A missing or invalid certificate makes it return an error at
// once.
func ServeTLS(ctx context.Context, srv *http.Server, ln net.Listener, certFile, keyFile string, shutdownTimeout time.Duration) error {
	return serve(ctx, srv, ln, func() error { return srv.ServeTLS(ln, certFile, keyFile) }, shutdownTimeout)
}

func serve(ctx context.Context, srv *http.Server, ln net.Listener, run func() error, shutdownTimeout time.Duration) error {
	serveErr := make(chan error, 1)
	go func() { serveErr <- run() }()

	select {
	case err := <-serveErr:
		_ = ln.Close() // ServeTLS returns without closing it if the certificate does not load
		return fmt.Errorf("httpserver: serve: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		_ = srv.Close() // drop connections that did not finish in time
	}
	err := <-serveErr
	switch {
	case shutdownErr != nil:
		return fmt.Errorf("httpserver: shutdown: %w", shutdownErr)
	case err != nil && !errors.Is(err, http.ErrServerClosed):
		return fmt.Errorf("httpserver: serve: %w", err)
	}
	return nil
}
