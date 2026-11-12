package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"time"

	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/platform/version"
	"github.com/Zeptiny/Syncr/internal/worker/agent"
	workerconfig "github.com/Zeptiny/Syncr/internal/worker/config"
	"github.com/Zeptiny/Syncr/internal/worker/linkcore"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/setup"
	wactive "github.com/Zeptiny/Syncr/internal/worker/transport/active"
	wpassive "github.com/Zeptiny/Syncr/internal/worker/transport/passive"
)

// linkStarter starts the worker's link to the controller and returns a
// function that stops it. It is called once, after the worker is built;
// `run` calls stop after its context ends.
type linkStarter func(ctx context.Context, cfg workerconfig.Config, w *worker, log *slog.Logger) (stop func(), err error)

// Shutdown budget of the link. A service manager allows about 20 s for a
// stop (the Windows SCM's default; internal/worker/service waits as long),
// so the steps together stay below that.
const (
	// agentShutdownTimeout bounds agent.Shutdown: running jobs get the
	// supervisor's grace period to stop their tools, plus a margin.
	agentShutdownTimeout = proc.DefaultGracePeriod + 2*time.Second
	// drainTimeout bounds the final flush of the CANCELLED results.
	drainTimeout = 5 * time.Second
)

// linkInfo is what the link needs from the build and the host.
type linkInfo struct {
	version  version.Info
	hostname func() (string, error)
}

// startLink builds the job agent over the worker's runners and starts the
// transport of cfg.Link.Mode:
//
//   - active: the Report and Poll loops towards the controller;
//   - passive: the TLS server the controller calls;
//   - unset: nothing (the worker is not enrolled); it idles with a hint.
//
// The link also carries the controller's probes to w.probes (linkcore
// WithProber), and the Hello event names the link features and probe
// operations of this build (identity) and the hooks of the configuration
// (w.hooks), which the agent runs for the jobs that name them.
//
// The returned stop function shuts down in order: (1) the transport's run
// context ends (active loops stop; passive keeps serving), (2) the probes
// in flight are cancelled (Core.Close) and the agent cancels its jobs and
// waits for their CANCELLED results, (3) those results
// are flushed: the active transport reports until acknowledged
// (Transport.Drain), the passive server answers Pulls until acknowledged
// (Server.Shutdown). Each step is bounded, see the timeouts above.
func startLink(ctx context.Context, cfg workerconfig.Config, w *worker, log *slog.Logger, info linkInfo) (func(), error) {
	if cfg.Link.Mode == workerconfig.LinkUnset {
		log.Warn("this worker is not enrolled; run \"syncr-worker setup\" to connect it to a controller")
		return func() {}, nil
	}

	a, err := agent.New(agent.Config{
		Runners:           w.registry,
		Hooks:             w.hooks,
		Identity:          identity(w, info),
		MaxConcurrentJobs: cfg.MaxConcurrentJobs,
		ProgressInterval:  cfg.ProgressInterval,
		Logger:            log.With("component", "agent"),
	})
	if err != nil {
		return nil, fmt.Errorf("start the job agent: %w", err)
	}
	var opts []linkcore.Option
	if w.probes != nil {
		opts = append(opts, linkcore.WithProber(w.probes))
	}
	core := linkcore.New(a, a.Outbox(), log.With("component", "link"), opts...)
	log.Info("worker link starting", "link_mode", string(cfg.Link.Mode), "epoch", a.Epoch(),
		"max_concurrent_jobs", cfg.MaxConcurrentJobs)

	shutdownAgent := func() {
		// Probes first: they are short and their callers have gone.
		core.Close()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentShutdownTimeout)
		defer cancel()
		if err := a.Shutdown(sctx); err != nil {
			log.Error("shutdown: jobs did not stop in time", "error", err.Error())
			return
		}
		log.Info("shutdown: jobs stopped")
	}

	switch cfg.Link.Mode {
	case workerconfig.LinkActive:
		return startActive(ctx, cfg.Link, core, shutdownAgent, log)
	case workerconfig.LinkPassive:
		return startPassive(ctx, cfg.Link, core, shutdownAgent, log)
	default:
		return nil, fmt.Errorf("unknown link mode %q", cfg.Link.Mode)
	}
}

func startActive(ctx context.Context, l workerconfig.LinkConfig, core *linkcore.Core, shutdownAgent func(), log *slog.Logger) (func(), error) {
	var roots *x509.CertPool
	if l.ControllerCAFile != "" {
		pool, err := setup.LoadCAPool(l.ControllerCAFile)
		if err != nil {
			return nil, fmt.Errorf("link.controller_ca_file: %w", err)
		}
		roots = pool
	}
	tr, err := wactive.New(core, wactive.Options{
		ControllerURL: l.ControllerURL,
		WorkerID:      l.WorkerID,
		Secret:        l.WorkerSecret.Reveal(),
		RootCAs:       roots,
		Logger:        log.With("component", "transport"),
	})
	if err != nil {
		return nil, fmt.Errorf("start the active link: %w", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := tr.Run(runCtx); err != nil {
			log.Error("active link stopped", "error", err.Error())
		}
	}()
	log.Info("active link started", "controller", l.ControllerURL, "worker_id", l.WorkerID)

	return func() {
		cancel()
		<-done
		log.Info("shutdown: active link loops stopped")
		shutdownAgent()
		dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
		defer dcancel()
		if err := tr.Drain(dctx); err != nil {
			log.Warn("shutdown: not every event reached the controller; the controller reconciles the jobs when the worker returns", "error", err.Error())
			return
		}
		log.Info("shutdown: every event delivered to the controller")
	}, nil
}

func startPassive(ctx context.Context, l workerconfig.LinkConfig, core *linkcore.Core, shutdownAgent func(), log *slog.Logger) (func(), error) {
	srv, err := wpassive.New(core, wpassive.Options{
		Addr:     l.ListenAddr,
		CertFile: l.TLSCertFile,
		KeyFile:  l.TLSKeyFile,
		Secret:   l.PairingSecret.Reveal(),
		Logger:   log.With("component", "transport"),
	})
	if err != nil {
		return nil, fmt.Errorf("start the passive link: %w", err)
	}
	ln, err := srv.Listen(ctx)
	if err != nil {
		return nil, fmt.Errorf("start the passive link: listen on %s: %w", l.ListenAddr, err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	log.Info("passive link listening", "addr", ln.Addr().String(), "advertise_url", l.AdvertiseURL, "fingerprint", srv.Fingerprint())

	return func() {
		// The passive server has no loops of its own: it keeps answering
		// the controller's Pulls while the jobs stop, so their results
		// flow out as they arrive.
		shutdownAgent()
		dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
		defer dcancel()
		if err := srv.Shutdown(dctx); err != nil {
			log.Warn("shutdown: the controller did not collect every event; it reconciles the jobs when the worker returns", "error", err.Error())
		} else {
			log.Info("shutdown: every event collected by the controller")
		}
		if err := <-served; err != nil && !errors.Is(err, net.ErrClosed) {
			log.Error("passive link server stopped with an error", "error", err.Error())
		}
	}, nil
}

// identity describes this worker process in its Hello event.
func identity(w *worker, info linkInfo) agent.Identity {
	host, err := info.hostname()
	if err != nil {
		host = ""
	}
	id := agent.Identity{
		WorkerVersion: info.version.Version,
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		Hostname:      host,
		LocalRoots:    w.policy.Roots(),
	}
	if w.probes != nil {
		id.Features, id.ProbeOps = link.Features(), w.probes.Ops()
	}
	for _, tool := range []string{toolRclone, toolRestic} {
		if r, ok := w.tools[tool]; ok {
			id.Tools = append(id.Tools, agent.Tool{Name: tool, Version: r.Version})
		}
	}
	return id
}
