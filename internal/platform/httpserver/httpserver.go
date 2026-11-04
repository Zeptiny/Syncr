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
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
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
