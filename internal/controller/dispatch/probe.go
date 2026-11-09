package dispatch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// ProbeConfig tunes the ProbeBroker. Zero fields take the
// DefaultProbeConfig values.
type ProbeConfig struct {
	// MaxInFlightPerWorker bounds the probes waiting for one worker; more
	// fail with domain.ErrUnavailable at once.
	MaxInFlightPerWorker int
	// MaxTimeout caps a probe's RunProbe.timeout (workers cap it too).
	MaxTimeout time.Duration
	// Grace is added to a probe's timeout for the controller's own wait
	// (delivery, the worker's answer).
	Grace time.Duration
}

// DefaultProbeConfig returns the default configuration: 8 in flight per
// worker, a 90 s cap and 5 s of grace.
func DefaultProbeConfig() ProbeConfig {
	return ProbeConfig{MaxInFlightPerWorker: 8, MaxTimeout: 90 * time.Second, Grace: 5 * time.Second}
}

func (c ProbeConfig) withDefaults() ProbeConfig {
	def := DefaultProbeConfig()
	c.MaxInFlightPerWorker = cmp.Or(c.MaxInFlightPerWorker, def.MaxInFlightPerWorker)
	c.MaxTimeout = cmp.Or(c.MaxTimeout, def.MaxTimeout)
	c.Grace = cmp.Or(c.Grace, def.Grace)
	return c
}

// ProbeBrokerDeps are the ProbeBroker's collaborators. All are required
// except Logger.
type ProbeBrokerDeps struct {
	Workers Workers
	// Hub must be the dispatcher's Hub: a queued run_probe wakes the
	// worker's long poll.
	Hub   *Hub
	Clock Clock
	// Logger defaults to discarding. Probe specs are never logged.
	Logger *slog.Logger
}

// ProbeBroker runs probes on workers (M5 design §6.4). It breaks the wiring
// cycle between the dispatcher and the controller's probe runner: the
// runner calls Probe, the dispatcher (Deps.Probes) hands the broker's
// pending probes to active workers as Command.run_probe and their results
// (Service.CompleteProbe) back, and passive loops register their clients
// so Probe can call WorkerLinkService.Probe directly. Probes live in
// memory only; a controller restart fails the waiting callers with
// domain.ErrUnavailable. Safe for concurrent use.
//
// m5: Track B implements the broker: the pending table, the
// run_probe/cancel_probe commands ("probe:<id>", "probe-cancel:<id>", at
// most 16, before the job commands), acks, epoch adoption (delivered
// probes fail with ErrUnavailable "the worker restarted"), CompleteProbe
// matching by probe ID and worker, the passive client registry and the
// in-flight cap.
type ProbeBroker struct {
	workers Workers
	hub     *Hub
	clock   Clock
	log     *slog.Logger
	cfg     ProbeConfig
}

// NewProbeBroker returns a ProbeBroker.
func NewProbeBroker(d ProbeBrokerDeps, cfg ProbeConfig) (*ProbeBroker, error) {
	if d.Workers == nil || d.Hub == nil || d.Clock == nil {
		return nil, errors.New("dispatch: probe broker: missing dependency")
	}
	cfg = cfg.withDefaults()
	if cfg.MaxInFlightPerWorker < 1 || cfg.MaxTimeout <= 0 || cfg.Grace < 0 {
		return nil, fmt.Errorf("dispatch: probe broker: invalid config %+v", cfg)
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &ProbeBroker{workers: d.Workers, hub: d.Hub, clock: d.Clock, log: log, cfg: cfg}, nil
}

// errProbesPending is the error of the probe machinery until Track B
// implements it.
var errProbesPending = fmt.Errorf("worker probes: %w (m5: pending track B)", errors.ErrUnsupported)

// Probe runs p on the worker and returns its result: in active mode as a
// Command.run_probe delivered by the worker's long poll and answered with
// CompleteProbe, in passive mode by calling the worker's Probe RPC with
// ctx and the deadline p.timeout + Grace. A result with status FAILED or
// CANCELLED is a result, not an error.
//
// Errors: domain.ErrNotFound (no such worker, or removed);
// domain.ErrUnavailable (offline or pending, passive link not running, no
// answer in time, the worker restarted, a transport failure, more than
// MaxInFlightPerWorker); domain.ErrFailedPrecondition (the worker lacks
// link.FeatureProbe or the op: "worker %q (version %s) cannot run %s;
// upgrade it", or a passive worker answered Unimplemented); ctx's errors.
//
// m5: Track B implements this.
func (b *ProbeBroker) Probe(_ context.Context, workerID domain.WorkerID, _ *syncrv1.RunProbe) (*syncrv1.ProbeResult, error) {
	return nil, fmt.Errorf("probe worker %s: %w", workerID, errProbesPending)
}

// CompleteProbe delivers the result of a probe that worker workerID (in
// epoch) received as Command.run_probe (ControllerLinkService.
// CompleteProbe). It authenticates and adopts the epoch like Report
// (ErrStaleEpoch for a replaced one), then hands r to the broker: a result
// of an unknown, finished or another worker's probe is ignored (nil), so
// retries are idempotent.
//
// m5: Track B implements this (with Deps.Probes).
func (s *Service) CompleteProbe(_ context.Context, workerID domain.WorkerID, _ string, _ *syncrv1.ProbeResult) error {
	return fmt.Errorf("complete a probe of worker %s: %w", workerID, errProbesPending)
}
