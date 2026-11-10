// Package linkcore turns worker-link envelopes into agent calls and back, so
// the active and passive transports (internal/worker/transport/...) only
// move bytes. It does no networking and holds little state: the command acks
// an active worker still has to deliver, and the probes in flight with
// their undelivered results (probe.go).
//
// # Active mode (the worker calls the controller)
//
// The transport runs two loops that may overlap in time, but must not run
// two Reports (or two Polls) concurrently:
//
// Report loop:
//
//  1. req := core.BuildReport(0). It carries the epoch, every unacked event
//     (up to the limit), every undelivered command ack and a WorkerState
//     consistent with those events.
//  2. Send it. On a transport error, wait (backoff) and go to 1: building a
//     new request resends the same events and acks; the controller
//     deduplicates by (epoch, seq) and by command id.
//  3. On success, more, err := core.ApplyReportResponse(req, resp). It acks
//     the events up to resp.ack_seq and marks req's command acks delivered.
//     An error only reports an implausible ack (log it, keep going).
//  4. If more, go to 1 at once. Otherwise wait for core.Notify() (new
//     events), rate-limited to about one Report per second, or for the idle
//     heartbeat (~15 s), whichever comes first, then go to 1.
//
// Poll loop:
//
//  1. req := core.BuildPoll(wait). It carries the epoch and every undelivered
//     command ack.
//  2. Send it (long poll). On a transport error, back off and go to 1; the
//     acks stay undelivered and go out with the next Poll or Report.
//  3. On success, core.ApplyPollResponse(ctx, req, resp) marks req's acks
//     delivered, hands resp.commands to the agent and returns the new acks.
//     They are sent with the next request automatically; since the loop
//     polls again at once, that is immediate. A transport may also trigger a
//     Report to deliver them.
//
// # Passive mode (the controller calls the worker)
//
//   - Command: resp := core.HandleCommandRequest(ctx, req). Every command
//     with an ID that the agent accepts is acked in the response
//     (duplicates too); the controller redelivers commands whose ack it did
//     not get. The agent refuses StartJob once it shuts down.
//   - Pull: resp := core.HandlePull(req). req.after_seq acknowledges every
//     event up to it when req.epoch is the worker's epoch; otherwise (a new
//     worker process, or a controller that never saw this epoch) it is
//     ignored and the response starts at the oldest unacked event. The
//     response always carries the epoch, the events after after_seq (up to
//     max_events and about max_bytes, but at least one), has_more and the
//     WorkerState consistent with the events.
//     When has_more is set the controller should Pull again at once.
//
// # Probes
//
// A probe is a short request answered outside the job slots (Prober). In
// passive mode it is one call: resp := core.HandleProbe(ctx, req), which
// ends when the controller's call does. In active mode a run_probe command
// starts it in the background (deduplicated by probe ID, acked like every
// command) and a cancel_probe command stops it; a cancel_probe that comes
// first, in an earlier Poll or the same one, keeps it from ever starting
// (the cancelled ID is remembered like a started one). A third loop of the
// transport delivers the results:
//
//  1. Wait for core.ProbeNotify().
//  2. For each result of core.PendingProbeResults(), call CompleteProbe.
//     On success, or when the controller refuses the result for good, call
//     core.ProbeDelivered(id). On a transport error, back off and go to 2:
//     an undelivered result stays pending until it expires.
//
// # Shutdown
//
// The composition root calls agent.Shutdown, which leaves every job's
// CANCELLED result in the outbox, then gives the transport a bounded grace
// period for a final flush: the active transport keeps reporting until
// core.Drained() (or the deadline); the passive transport keeps serving Pull
// (and Command, whose StartJobs the agent now refuses) until
// core.WaitDrained(ctx) returns.
//
// Allowed dependencies: the standard library, the generated protobuf code,
// internal/link (the probe time limits) and internal/worker/agent (for its
// Snapshot type).
package linkcore

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/agent"
	"github.com/Zeptiny/Syncr/internal/worker/outbox"
)

// Event batch limits.
const (
	// DefaultMaxEvents is the number of events per Report or Pull when the
	// caller does not choose one.
	DefaultMaxEvents = 256
	// MaxEvents caps the events per Report or Pull, bounding message size.
	MaxEvents = 1024
	// DefaultMaxBytes is the soft bound on the encoded size of the events of
	// a Report, and of a Pull that does not set max_bytes. A batch always
	// holds at least one pending event, however large; the agent bounds the
	// size of a single event (agent.DefaultMaxOutputBytes), so a message
	// stays well below the link's 16 MiB read limits.
	DefaultMaxBytes = 4 << 20
	// MaxBytes caps PullRequest.max_bytes.
	MaxBytes = 8 << 20
)

// Agent is what the link needs from the job agent. *agent.Agent implements
// it.
type Agent interface {
	// Epoch returns the worker process epoch.
	Epoch() string
	// HandleCommands processes commands and returns their acks.
	HandleCommands(ctx context.Context, cmds []*syncrv1.Command) []*syncrv1.CommandAck
	// Snapshot returns pending events after afterSeq, bounded by count and
	// encoded size, with a consistent state.
	Snapshot(afterSeq int64, limit, maxBytes int) agent.Snapshot
	// AckEvents acknowledges events up to seq.
	AckEvents(seq int64) error
}

// Outbox is the part of the outbox the transports wait on. *outbox.Outbox
// implements it.
type Outbox interface {
	Notify() <-chan struct{}
	WaitDrained(ctx context.Context) error
	Stats() outbox.Stats
}

// Core holds the link logic of one worker process. Create it with New.
type Core struct {
	agent Agent
	out   Outbox
	log   *slog.Logger

	mu sync.Mutex
	// acks are command acks not yet delivered in a successful Report or
	// Poll (active mode), in the order they were produced, without
	// duplicates.
	acks     []*syncrv1.CommandAck
	ackIndex map[string]struct{}

	// probes are the probes in flight and their undelivered results.
	probes probes
}

// New returns a Core for a and its outbox. A nil logger discards logs.
func New(a Agent, out Outbox, log *slog.Logger, opts ...Option) *Core {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Core{agent: a, out: out, log: log, ackIndex: make(map[string]struct{})}
	c.probes.init()
	for _, o := range opts {
		o(c)
	}
	return c
}

// Notify signals that events were appended (see outbox.Outbox.Notify).
func (c *Core) Notify() <-chan struct{} { return c.out.Notify() }

// Drained reports whether every event has been acked.
func (c *Core) Drained() bool { return c.out.Stats().Backlog == 0 }

// WaitDrained blocks until every event has been acked or ctx is done.
func (c *Core) WaitDrained(ctx context.Context) error {
	if err := c.out.WaitDrained(ctx); err != nil {
		return fmt.Errorf("linkcore: outbox not drained: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Active mode
// ---------------------------------------------------------------------------

// BuildReport returns the next Report request: all unacked events (at most
// maxEvents; <= 0 means DefaultMaxEvents, capped at MaxEvents; and about
// DefaultMaxBytes, but at least one), all undelivered command acks and the
// matching worker state.
func (c *Core) BuildReport(maxEvents int) *syncrv1.ReportRequest {
	snap := c.agent.Snapshot(0, clampMax(maxEvents), DefaultMaxBytes)
	return &syncrv1.ReportRequest{
		Epoch:  snap.Epoch,
		Events: snap.Events,
		Acks:   c.pendingAcks(),
		State:  snap.State,
	}
}

// ApplyReportResponse applies the controller's answer to req, which was
// sent successfully: it marks req's command acks delivered and acknowledges
// events up to resp.ack_seq. It reports whether unacked events remain (the
// transport should Report again soon). Acks that go backwards are no-ops; an
// ack beyond the last event the worker produced is ignored and returned as
// an error for logging.
func (c *Core) ApplyReportResponse(req *syncrv1.ReportRequest, resp *syncrv1.ReportResponse) (more bool, err error) {
	c.delivered(req.GetAcks())
	if req.GetEpoch() != c.agent.Epoch() {
		err = fmt.Errorf("linkcore: report response for epoch %q, current epoch %q: ignored", req.GetEpoch(), c.agent.Epoch())
	} else if ackErr := c.agent.AckEvents(resp.GetAckSeq()); ackErr != nil {
		err = fmt.Errorf("linkcore: report response: %w", ackErr)
	}
	if err != nil {
		c.log.Warn("ignoring implausible event ack", "error", err.Error())
	}
	return !c.Drained(), err
}

// BuildPoll returns the next Poll request: the epoch, the long-poll wait and
// every undelivered command ack.
func (c *Core) BuildPoll(wait time.Duration) *syncrv1.PollRequest {
	req := &syncrv1.PollRequest{Epoch: c.agent.Epoch(), Acks: c.pendingAcks()}
	if wait > 0 {
		req.Wait = durationpb.New(wait)
	}
	return req
}

// ApplyPollResponse applies the controller's answer to req, which was sent
// successfully: req's acks are marked delivered, resp's commands are handed
// to the agent in order, and the acks of those commands are returned. They
// are also queued, so the next BuildPoll or BuildReport carries them until
// one of those calls succeeds.
func (c *Core) ApplyPollResponse(ctx context.Context, req *syncrv1.PollRequest, resp *syncrv1.PollResponse) []*syncrv1.CommandAck {
	c.delivered(req.GetAcks())
	if len(resp.GetCommands()) == 0 {
		return nil
	}
	// Probe commands are the core's own: probes run outside the agent's
	// job slots and their results travel on their own RPC.
	acks, rest := c.takeProbeCommands(resp.GetCommands())
	if len(rest) > 0 {
		acks = append(acks, c.agent.HandleCommands(ctx, rest)...)
	}
	c.queueAcks(acks)
	return acks
}

func (c *Core) pendingAcks() []*syncrv1.CommandAck {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.acks) == 0 {
		return nil
	}
	out := make([]*syncrv1.CommandAck, len(c.acks))
	for i, a := range c.acks {
		out[i] = &syncrv1.CommandAck{CommandId: a.GetCommandId()}
	}
	return out
}

func (c *Core) queueAcks(acks []*syncrv1.CommandAck) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range acks {
		id := a.GetCommandId()
		if _, dup := c.ackIndex[id]; dup {
			continue
		}
		c.ackIndex[id] = struct{}{}
		c.acks = append(c.acks, &syncrv1.CommandAck{CommandId: id})
	}
}

// delivered forgets acks that reached the controller.
func (c *Core) delivered(acks []*syncrv1.CommandAck) {
	if len(acks) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range acks {
		delete(c.ackIndex, a.GetCommandId())
	}
	kept := c.acks[:0]
	for _, a := range c.acks {
		if _, ok := c.ackIndex[a.GetCommandId()]; ok {
			kept = append(kept, a)
		}
	}
	clear(c.acks[len(kept):])
	c.acks = kept
}

// PendingAcks returns how many command acks wait for delivery (active mode).
func (c *Core) PendingAcks() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.acks)
}

// ---------------------------------------------------------------------------
// Passive mode
// ---------------------------------------------------------------------------

// HandleCommandRequest hands req's commands to the agent and returns their
// acks with the worker's epoch.
func (c *Core) HandleCommandRequest(ctx context.Context, req *syncrv1.CommandRequest) *syncrv1.CommandResponse {
	return &syncrv1.CommandResponse{
		Epoch: c.agent.Epoch(),
		Acks:  c.agent.HandleCommands(ctx, req.GetCommands()),
	}
}

// HandlePull answers a Pull. If req.epoch is the worker's epoch, every event
// up to req.after_seq is acknowledged and the response holds the events
// after it; an after_seq beyond the last event the worker produced cannot be
// trusted, so it is ignored (logged) like a foreign epoch. For a foreign or
// empty epoch, after_seq is ignored and the response starts at the oldest
// unacked event. Responses always carry the epoch, has_more and the worker
// state consistent with the returned events.
func (c *Core) HandlePull(req *syncrv1.PullRequest) *syncrv1.PullResponse {
	after := int64(0)
	if req.GetEpoch() == c.agent.Epoch() {
		after = req.GetAfterSeq()
		if err := c.agent.AckEvents(after); err != nil {
			c.log.Warn("ignoring implausible pull ack", "after_seq", after, "error", err.Error())
			after = 0
		}
	}
	return c.pull(after, req.GetMaxEvents(), req.GetMaxBytes())
}

func (c *Core) pull(after int64, maxEvents, maxBytes int32) *syncrv1.PullResponse {
	snap := c.agent.Snapshot(after, clampMax(int(maxEvents)), clampBytes(int(maxBytes)))
	return &syncrv1.PullResponse{
		Epoch:   snap.Epoch,
		Events:  snap.Events,
		HasMore: snap.More,
		State:   snap.State,
	}
}

func clampBytes(n int) int {
	switch {
	case n <= 0:
		return DefaultMaxBytes
	case n > MaxBytes:
		return MaxBytes
	}
	return n
}

func clampMax(n int) int {
	switch {
	case n <= 0:
		return DefaultMaxEvents
	case n > MaxEvents:
		return MaxEvents
	}
	return n
}
