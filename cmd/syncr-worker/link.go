package main

import (
	"context"
	"log/slog"

	workerconfig "github.com/Zeptiny/Syncr/internal/worker/config"
)

// linkStarter starts the worker's link to the controller and returns a
// function that stops it. It is called once, after the worker is built.
type linkStarter func(ctx context.Context, cfg workerconfig.Config, w *worker, log *slog.Logger) (stop func(), err error)

// startLink is THE integration point for the link transport: the next phase
// replaces its body with "build the job agent from w.registry, then start the
// active transport (cfg.Link.Mode == LinkActive) or the passive TLS server
// (LinkPassive) with cfg.Link's credentials", and returns a stop function
// that drains them. `run` already waits for ctx and calls stop, whether it
// runs interactively or under a service manager. Until then the worker idles.
func startLink(_ context.Context, cfg workerconfig.Config, _ *worker, log *slog.Logger) (func(), error) {
	switch cfg.Link.Mode {
	case workerconfig.LinkUnset:
		log.Warn("this worker is not enrolled; run \"syncr-worker setup\" to connect it to a controller")
	default:
		log.Warn("worker link not implemented yet; idling until stopped", "link_mode", string(cfg.Link.Mode))
	}
	return func() {}, nil
}
