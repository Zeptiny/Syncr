// Command syncr-controller runs the Syncr controller: the API, the worker
// link (enrollment, active workers' Report/Poll, the passive supervisor),
// job dispatch and (from later milestones) the web UI and scheduling.
//
// This package is the composition root: it is the only place that reads the
// process environment, builds adapters and wires them together (wire.go;
// docs/adr/0005-runtime-wiring.md describes the result).
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	controllerconfig "github.com/Zeptiny/Syncr/internal/controller/config"
	"github.com/Zeptiny/Syncr/internal/platform/config"
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
	healthcheck := fs.Bool("healthcheck", false, "check that the controller configured like this one answers on /healthz, and exit 0 if it does (for container health checks)")
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

	if *healthcheck {
		return checkHealth(ctx, cfg.HTTP, stdout)
	}
	if err := cfg.CheckServe(); err != nil {
		return err
	}

	logger, err := logging.New(stderr, cfg.Log)
	if err != nil {
		return err
	}
	if err := serve(ctx, cfg, info, logger, ready); err != nil {
		return err
	}
	logger.Info("controller stopped")
	return nil
}

// healthTimeout bounds -healthcheck.
const healthTimeout = 5 * time.Second

// checkHealth calls /healthz of the controller listening on cfg.Addr over
// loopback. It verifies only that the process answers, so a TLS certificate
// is not verified (it is usually issued for a public name, not 127.0.0.1).
func checkHealth(ctx context.Context, cfg controllerconfig.HTTP, stdout io.Writer) error {
	_, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return err
	}
	scheme := "http"
	tr := &http.Transport{DisableKeepAlives: true} // no proxy: this is a loopback call
	if cfg.TLS() {
		scheme = "https"
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true} //nolint:gosec // loopback liveness probe of this very process; the certificate names the public host
	}
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	url := scheme + "://" + net.JoinHostPort("127.0.0.1", port) + "/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: %s answered %s", url, resp.Status)
	}
	_, err = fmt.Fprintln(stdout, "ok")
	return err
}
