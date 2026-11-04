// Command syncr-controller runs the Syncr controller: the API, the web UI and
// (from later milestones) scheduling, job dispatch and worker links.
//
// This file is the composition root: it is the only place that reads the
// process environment, builds adapters and wires them together.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/Zeptiny/Syncr/internal/adapters/api"
	controllerconfig "github.com/Zeptiny/Syncr/internal/controller/config"
	"github.com/Zeptiny/Syncr/internal/platform/config"
	"github.com/Zeptiny/Syncr/internal/platform/httpserver"
	"github.com/Zeptiny/Syncr/internal/platform/logging"
	"github.com/Zeptiny/Syncr/internal/platform/version"
)

const name = "syncr-controller"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.LookupEnv, nil)
	stop()
	if err != nil {
		if !errors.Is(err, errUsage) {
			fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		}
		os.Exit(1)
	}
}

// errUsage reports invalid command-line arguments; flag already printed why.
var errUsage = errors.New("usage")

// run is main without process globals, so tests can drive it. If ready is
// non-nil it receives the listen address once the server accepts
// connections.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, lookupEnv config.LookupEnv, ready chan<- net.Addr) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to the YAML config file (default $"+config.PathEnv+"; optional)")
	showVersion := fs.Bool("version", false, "print version information and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "%s: unexpected arguments: %q\n", name, fs.Args())
		return errUsage
	}

	info := version.Get()
	if *showVersion {
		_, err := fmt.Fprintf(stdout, "%s %s\n", name, info)
		return err
	}

	path := *configPath
	if path == "" {
		path, _ = lookupEnv(config.PathEnv)
	}
	cfg := controllerconfig.Default()
	if err := config.Load(path, lookupEnv, &cfg); err != nil {
		return err
	}

	logger, err := logging.New(stderr, cfg.Log)
	if err != nil {
		return err
	}

	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", cfg.HTTP.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true) // gRPC clients without TLS (h2c)
	srv := &http.Server{
		Handler:           api.NewHandler(api.Deps{BuildInfo: info, Logger: logger}),
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		Protocols:         protocols,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	logger.Info("controller starting",
		"version", info.Version, "commit", info.Commit, "addr", ln.Addr().String())
	if ready != nil {
		ready <- ln.Addr()
	}
	if err := httpserver.Serve(ctx, srv, ln, cfg.HTTP.ShutdownTimeout); err != nil {
		return err
	}
	logger.Info("controller stopped")
	return nil
}
