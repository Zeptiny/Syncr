// Package outbox is the worker's in-memory event outbox: the ordered,
// at-least-once buffer of link events (Hello, JobStarted, Progress,
// JobResult) between the job agent and the controller (see
// docs/adr/0002-worker-link.md).
//
// # Sequence numbers and epoch
//
// An Outbox has an epoch, a random UUIDv7 chosen when it is created and
// fixed for its lifetime (one Outbox per worker process). Append assigns
// every event the next seq, starting at 1, and a timestamp. Seqs strictly
// increase in append order. The first event of every epoch is the Hello
// passed to New.
//
// # Acknowledgement
//
// Events stay in the outbox until Ack(seq) drops every event with a seq less
// than or equal to seq. Acks are idempotent: an ack that goes backwards is a
// no-op, and an ack beyond the last assigned seq is refused with
// ErrAckAhead and changes nothing (it cannot refer to this epoch, so
// trusting it could lose events).
//
// # Bounded memory: coalescing and backpressure
//
// Two limits bound the backlog of unacked events (Options.CoalesceAt and
// Options.HighWater):
//
//   - Coalescing. While the backlog holds at least CoalesceAt events, a
//     Progress event appended for a job replaces every unacked Progress event
//     of that job, and a JobResult appended for a job drops all but the latest
//     unacked Progress event of that job. The replaced events are removed;
//     the new event gets a fresh seq at the end. So under backlog each job has
//     at most one Progress event pending, always the newest sample.
//   - Backpressure. Append never blocks and never drops a non-Progress event.
//     Instead, WaitCapacity blocks while the backlog holds HighWater events
//     or more; the agent calls it before starting a job, so no new work (and
//     no new JobStarted or JobResult) is admitted until the controller acks.
//     Jobs that already run still append their Progress (coalesced) and
//     their JobResult, which is why the backlog may exceed HighWater by about
//     two events per admitted job.
//
// What a consumer sees: seqs strictly increase within the epoch but may have
// gaps (coalesced Progress events, or events acked by a concurrent call);
// Hello, JobStarted and JobResult are never dropped or reordered relative to
// each other; a job's Progress events always come after its JobStarted and
// before its JobResult; the newest Progress sample of a job is never
// dropped while the job runs. An event once appended is never modified, so
// a consumer may see the same seq twice (redelivery) but always with the same
// content.
//
// # Notification
//
// Notify returns a channel that receives a value (coalesced, capacity one)
// after events were appended, so an active transport can flush promptly.
//
// The Outbox is safe for concurrent use. It persists nothing: when the
// worker process ends, its unacked events and its epoch are gone, and the
// controller reconciles the jobs it held as interrupted.
//
// Allowed dependencies: the standard library and the generated protobuf
// code.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"uuid"

	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// Defaults for Options.
const (
	// DefaultCoalesceAt is the default backlog size at which Progress events
	// start to be coalesced.
	DefaultCoalesceAt = 512
	// DefaultHighWater is the default backlog size at which WaitCapacity
	// blocks.
	DefaultHighWater = 2048
)

// ErrAckAhead is returned (wrapped) by Ack for a seq beyond the last assigned
// one.
var ErrAckAhead = errors.New("outbox: ack is ahead of the last assigned seq")

// ErrInvalidEvent is returned (wrapped) by Append for an event it does not
// accept.
var ErrInvalidEvent = errors.New("outbox: invalid event")

// Options configures an Outbox. The zero value selects the defaults.
type Options struct {
	// Epoch overrides the random epoch, for tests. Empty means a new UUIDv7.
	Epoch string

	// CoalesceAt is the backlog size (unacked events) from which Progress
	// events are coalesced. Zero means DefaultCoalesceAt.
	CoalesceAt int

	// HighWater is the backlog size from which WaitCapacity blocks. Zero
	// means DefaultHighWater; values below CoalesceAt are raised to it.
	HighWater int
}

// Outbox buffers events until they are acknowledged. Create it with New.
type Outbox struct {
	epoch      string
	coalesceAt int
	highWater  int
	notify     chan struct{}

	mu sync.Mutex
	// events are the unacked events in seq order. Every element is
	// immutable once appended.
	events []*syncrv1.Event
	// head is the last assigned seq; acked is the highest seq acked.
	head, acked int64
	// coalesced counts Progress events removed by coalescing.
	coalesced int64
	// changed is closed and replaced whenever events are dropped, waking
	// WaitCapacity and WaitDrained.
	changed chan struct{}
}

// New returns an Outbox whose first event (seq 1) is hello.
func New(hello *syncrv1.Hello, opts Options) *Outbox {
	o := &Outbox{
		epoch:      opts.Epoch,
		coalesceAt: opts.CoalesceAt,
		highWater:  opts.HighWater,
		notify:     make(chan struct{}, 1),
		changed:    make(chan struct{}),
	}
	if o.epoch == "" {
		o.epoch = uuid.NewV7().String()
	}
	if o.coalesceAt <= 0 {
		o.coalesceAt = DefaultCoalesceAt
	}
	if o.highWater <= 0 {
		o.highWater = DefaultHighWater
	}
	o.highWater = max(o.highWater, o.coalesceAt)
	if hello == nil {
		hello = &syncrv1.Hello{}
	}
	o.appendLocked(&syncrv1.Event{Payload: &syncrv1.Event_Hello{Hello: hello}})
	return o
}

// Epoch returns the outbox's epoch.
func (o *Outbox) Epoch() string { return o.epoch }

// Append adds ev at the end of the outbox and returns its seq. Append sets
// ev's Seq and Time and takes ownership of ev: the caller must not modify it
// afterwards. Hello events are refused (the Hello is passed to New), as are
// events without a payload. Append never blocks; see the package
// documentation for how Progress events are coalesced under backlog.
func (o *Outbox) Append(ev *syncrv1.Event) (int64, error) {
	switch ev.GetPayload().(type) {
	case nil:
		return 0, fmt.Errorf("%w: no payload", ErrInvalidEvent)
	case *syncrv1.Event_Hello:
		return 0, fmt.Errorf("%w: hello is only the first event of an epoch", ErrInvalidEvent)
	}
	o.mu.Lock()
	if len(o.events) >= o.coalesceAt {
		o.coalesceLocked(ev)
	}
	seq := o.appendLocked(ev)
	o.mu.Unlock()

	select {
	case o.notify <- struct{}{}:
	default:
	}
	return seq, nil
}

// appendLocked assigns the next seq and the current time to ev and appends
// it.
func (o *Outbox) appendLocked(ev *syncrv1.Event) int64 {
	o.head++
	ev.Seq = o.head
	ev.Time = timestamppb.Now()
	o.events = append(o.events, ev)
	return o.head
}

// coalesceLocked removes Progress events of the job ev belongs to, before ev
// is appended: all of them for a Progress ev, all but the latest for a
// JobResult ev.
func (o *Outbox) coalesceLocked(ev *syncrv1.Event) {
	var jobID string
	keepLatest := false
	switch p := ev.GetPayload().(type) {
	case *syncrv1.Event_Progress:
		jobID = p.Progress.GetJobId()
	case *syncrv1.Event_JobResult:
		jobID = p.JobResult.GetJobId()
		keepLatest = true
	default:
		return
	}
	latest := -1
	if keepLatest {
		for i := len(o.events) - 1; i >= 0; i-- {
			if isProgressOf(o.events[i], jobID) {
				latest = i
				break
			}
		}
	}
	kept := o.events[:0]
	for i, e := range o.events {
		if i != latest && isProgressOf(e, jobID) {
			o.coalesced++
			continue
		}
		kept = append(kept, e)
	}
	clear(o.events[len(kept):]) // release removed events
	o.events = kept
}

func isProgressOf(ev *syncrv1.Event, jobID string) bool {
	p := ev.GetProgress()
	return p != nil && p.GetJobId() == jobID
}

// Pending returns up to limit unacked events with seq > afterSeq, in seq
// order, and whether more such events exist beyond them. A limit <= 0 means
// no limit. The returned events are shared with the outbox and must not be
// modified.
func (o *Outbox) Pending(afterSeq int64, limit int) (events []*syncrv1.Event, more bool) {
	b := o.Read(afterSeq, limit)
	return b.Events, b.More
}

// Batch is a consistent read of the outbox (see Read).
type Batch struct {
	// Events are the returned events, in seq order. They are shared with the
	// outbox and must not be modified.
	Events []*syncrv1.Event
	// More reports that unacked events exist after the returned ones.
	More bool
	// Withheld are the JobResults of unacked events after the returned ones
	// (or after afterSeq if none were returned): results that exist but are
	// not part of this batch.
	Withheld []*syncrv1.JobResult
}

// Read is Pending plus the results it withholds, read atomically. A caller
// describing the jobs it holds alongside the batch uses Withheld to keep
// that description consistent with the events it sends: a job whose result
// is not in the batch is still held as far as the batch's reader knows.
func (o *Outbox) Read(afterSeq int64, limit int) Batch {
	o.mu.Lock()
	defer o.mu.Unlock()
	start := o.indexAfter(afterSeq)
	end := len(o.events)
	if limit > 0 && end-start > limit {
		end = start + limit
	}
	var b Batch
	if end > start {
		b.Events = append([]*syncrv1.Event(nil), o.events[start:end]...)
	}
	b.More = end < len(o.events)
	for _, e := range o.events[end:] {
		if r := e.GetJobResult(); r != nil {
			b.Withheld = append(b.Withheld, r)
		}
	}
	return b
}

// indexAfter returns the index of the first event with seq > seq.
func (o *Outbox) indexAfter(seq int64) int {
	lo, hi := 0, len(o.events)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if o.events[mid].GetSeq() <= seq {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// Ack drops every event with seq <= seq and returns how many were dropped.
// It is idempotent: acking a seq at or below the current ack level drops
// nothing. A seq beyond the last assigned one is refused with an error
// wrapping ErrAckAhead, and nothing is dropped.
func (o *Outbox) Ack(seq int64) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if seq > o.head {
		return 0, fmt.Errorf("%w: ack %d, last assigned %d", ErrAckAhead, seq, o.head)
	}
	if seq <= o.acked {
		return 0, nil
	}
	o.acked = seq
	n := o.indexAfter(seq)
	if n == 0 {
		return 0, nil
	}
	clear(o.events[:n])
	o.events = o.events[n:]
	if len(o.events) == 0 {
		o.events = nil // let the backing array go
	}
	close(o.changed)
	o.changed = make(chan struct{})
	return n, nil
}

// Notify returns a channel that receives a value after events were
// appended. Signals are coalesced (the channel has capacity one), so a
// receiver must read everything pending after each signal. It is meant for
// a single consumer: the active transport.
func (o *Outbox) Notify() <-chan struct{} { return o.notify }

// WaitCapacity blocks until the backlog is below the high-water mark or ctx
// is done, in which case it returns context.Cause(ctx).
func (o *Outbox) WaitCapacity(ctx context.Context) error {
	return o.waitUntil(ctx, func() bool { return len(o.events) < o.highWater })
}

// WaitDrained blocks until every event has been acked or ctx is done, in
// which case it returns context.Cause(ctx). A transport uses it to give the
// controller a chance to collect the last results during shutdown.
func (o *Outbox) WaitDrained(ctx context.Context) error {
	return o.waitUntil(ctx, func() bool { return len(o.events) == 0 })
}

func (o *Outbox) waitUntil(ctx context.Context, cond func() bool) error {
	for {
		o.mu.Lock()
		ok, changed := cond(), o.changed
		o.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

// Stats describe the outbox for logs and metrics.
type Stats struct {
	// Backlog is the number of unacked events.
	Backlog int
	// Head is the last assigned seq; Acked the highest acked seq.
	Head, Acked int64
	// Coalesced counts Progress events removed by coalescing so far.
	Coalesced int64
	// Saturated reports that the backlog is at or above the high-water mark,
	// so no new job is started until the controller acks.
	Saturated bool
}

// Stats returns the outbox's current statistics.
func (o *Outbox) Stats() Stats {
	o.mu.Lock()
	defer o.mu.Unlock()
	return Stats{
		Backlog:   len(o.events),
		Head:      o.head,
		Acked:     o.acked,
		Coalesced: o.coalesced,
		Saturated: len(o.events) >= o.highWater,
	}
}
