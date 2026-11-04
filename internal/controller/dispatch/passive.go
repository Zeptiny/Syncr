package dispatch

import (
	"cmp"
	"context"
	"fmt"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// PassiveConfig tunes PassiveLoop. Zero fields take the defaults.
type PassiveConfig struct {
	// BusyInterval is the pause between rounds while the worker holds jobs,
	// was just sent commands or just returned events.
	BusyInterval time.Duration
	// IdleInterval is the pause between rounds while the worker is idle
	// (its heartbeat).
	IdleInterval time.Duration
	// MinBackoff and MaxBackoff bound the exponential pause after a failed
	// round. A wake-up does not shorten it.
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// MaxEvents and MaxBytes are sent in PullRequest (0: worker default).
	MaxEvents int32
	MaxBytes  int32
}

// DefaultPassiveConfig returns the default passive timing: about 1s while
// busy and 15s while idle, like an active worker's reports.
func DefaultPassiveConfig() PassiveConfig {
	return PassiveConfig{BusyInterval: time.Second, IdleInterval: 15 * time.Second, MinBackoff: time.Second, MaxBackoff: 30 * time.Second}
}

func (c PassiveConfig) withDefaults() PassiveConfig {
	d := DefaultPassiveConfig()
	c.BusyInterval = cmp.Or(c.BusyInterval, d.BusyInterval)
	c.IdleInterval = cmp.Or(c.IdleInterval, d.IdleInterval)
	c.MinBackoff = cmp.Or(c.MinBackoff, d.MinBackoff)
	c.MaxBackoff = max(cmp.Or(c.MaxBackoff, d.MaxBackoff), c.MinBackoff)
	return c
}

// PassiveLoop drives the link of one passive worker through a
// PassiveClient. Each round:
//
//  1. Pull(epoch, after_seq) with the stored epoch and ingest cursor, which
//     also acknowledges the events stored so far; ingest the response
//     (events and state) like a Report.
//  2. Derive the worker's commands for the epoch the worker answered with;
//     if there are any, deliver them with Command and apply its acks.
//
// The next round starts at once if the worker said has_more, after
// BusyInterval if it holds jobs, was sent commands or returned events (the
// next Pull acknowledges them, so a worker's last results, e.g. while it
// shuts down, do not wait an idle interval for their ack), after
// IdleInterval otherwise, and early when the worker's commands change (Hub).
// A failed
// round is retried with exponential backoff; it does not count as contact,
// so the worker turns offline after the workers' OfflineAfter.
//
// Run one loop per passive worker (the transport's supervisor starts and
// stops them as workers are registered and removed).
type PassiveLoop struct {
	svc      *Service
	workerID domain.WorkerID
	client   PassiveClient
	cfg      PassiveConfig
}

// NewPassiveLoop returns a loop for workerID that talks through client.
func (s *Service) NewPassiveLoop(workerID domain.WorkerID, client PassiveClient) *PassiveLoop {
	return &PassiveLoop{svc: s, workerID: workerID, client: client, cfg: s.cfg.Passive}
}

// Run runs rounds until ctx ends, then returns nil.
func (l *PassiveLoop) Run(ctx context.Context) error {
	var backoff time.Duration
	for {
		woken := l.svc.hub.wait(l.workerID)
		busy, more, err := l.Round(ctx)
		if ctx.Err() != nil {
			return nil
		}
		var pause time.Duration
		switch {
		case err != nil:
			backoff = min(max(2*backoff, l.cfg.MinBackoff), l.cfg.MaxBackoff)
			pause, woken = backoff, nil
			l.svc.log.WarnContext(ctx, "passive worker round failed", "worker_id", l.workerID.String(),
				"retry_in", backoff.String(), "error", err.Error())
		case more:
			backoff = 0
			continue
		case busy:
			backoff, pause = 0, l.cfg.BusyInterval
		default:
			backoff, pause = 0, l.cfg.IdleInterval
		}
		t := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-woken:
			t.Stop()
		case <-t.C:
		}
	}
}

// Round performs one round (see PassiveLoop). busy reports that the worker
// holds jobs, was sent commands or returned events to acknowledge; more that
// it has further events.
func (l *PassiveLoop) Round(ctx context.Context) (busy, more bool, err error) {
	w, err := l.svc.workers.Get(ctx, l.workerID)
	if err != nil {
		return false, false, err
	}
	pull, err := l.client.Pull(ctx, &syncrv1.PullRequest{
		Epoch: w.Link.Epoch, AfterSeq: w.Link.AckSeq, MaxEvents: l.cfg.MaxEvents, MaxBytes: l.cfg.MaxBytes,
	})
	if err != nil {
		return false, false, fmt.Errorf("pull from worker %s: %w", l.workerID, err)
	}
	if _, err := l.svc.Ingest(ctx, l.workerID, pull.GetEpoch(), pull.GetEvents(), pull.GetState()); err != nil {
		return false, false, err
	}
	cmds, err := l.svc.Commands(ctx, l.workerID, pull.GetEpoch())
	if err != nil {
		return false, false, err
	}
	if len(cmds) > 0 {
		resp, err := l.client.Command(ctx, &syncrv1.CommandRequest{Commands: cmds})
		if err != nil {
			return false, false, fmt.Errorf("send commands to worker %s: %w", l.workerID, err)
		}
		if err := l.svc.Ack(ctx, l.workerID, resp.GetEpoch(), resp.GetAcks()); err != nil {
			return false, false, err
		}
	}
	busy = len(pull.GetState().GetJobs()) > 0 || len(cmds) > 0 || len(pull.GetEvents()) > 0
	return busy, pull.GetHasMore(), nil
}
