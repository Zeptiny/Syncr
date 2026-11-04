package memstore

import (
	"context"
	"errors"
	"sync"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// maxQueued bounds a subscription's queue. Progress updates are coalesced
// (only the newest pending one is kept) and a job has few status changes,
// so this is only reached by a reader that stopped reading; the oldest
// updates are then dropped.
const maxQueued = 256

// errClosed is returned by Next after Close.
var errClosed = errors.New("memstore: subscription closed")

// Broker is an in-process jobs.Broker. Create it with NewBroker; safe for
// concurrent use.
type Broker struct {
	mu   sync.Mutex
	subs map[domain.JobID]map[*subscription]struct{}
}

var _ jobs.Broker = (*Broker)(nil)

// NewBroker returns a Broker without subscribers.
func NewBroker() *Broker {
	return &Broker{subs: make(map[domain.JobID]map[*subscription]struct{})}
}

// Publish implements jobs.Broker. It never blocks.
func (b *Broker) Publish(u jobs.Update) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs[u.JobID] {
		s.push(u)
	}
}

// Subscribe implements jobs.Broker.
func (b *Broker) Subscribe(id domain.JobID) jobs.Subscription {
	s := &subscription{broker: b, id: id, signal: make(chan struct{}, 1)}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs[id] == nil {
		b.subs[id] = make(map[*subscription]struct{})
	}
	b.subs[id][s] = struct{}{}
	return s
}

// Subscribers returns the number of open subscriptions of a job (for
// tests and diagnostics).
func (b *Broker) Subscribers(id domain.JobID) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs[id])
}

func (b *Broker) remove(s *subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subs[s.id], s)
	if len(b.subs[s.id]) == 0 {
		delete(b.subs, s.id)
	}
}

type subscription struct {
	broker *Broker
	id     domain.JobID
	signal chan struct{} // capacity 1: "the queue may be non-empty"

	mu     sync.Mutex
	queue  []jobs.Update
	closed bool
}

func (s *subscription) push(u jobs.Update) {
	s.mu.Lock()
	switch n := len(s.queue); {
	case u.Progress != nil && n > 0 && s.queue[n-1].Progress != nil:
		s.queue[n-1] = u // coalesce consecutive progress
	case n >= maxQueued:
		s.queue = append(s.queue[1:], u)
	default:
		s.queue = append(s.queue, u)
	}
	s.mu.Unlock()
	select {
	case s.signal <- struct{}{}:
	default:
	}
}

// Next implements jobs.Subscription.
func (s *subscription) Next(ctx context.Context) (jobs.Update, error) {
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			u := s.queue[0]
			s.queue[0] = jobs.Update{}
			s.queue = s.queue[1:]
			s.mu.Unlock()
			return u, nil
		}
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return jobs.Update{}, errClosed
		}
		select {
		case <-s.signal:
		case <-ctx.Done():
			return jobs.Update{}, ctx.Err()
		}
	}
}

// Close implements jobs.Subscription.
func (s *subscription) Close() {
	s.mu.Lock()
	already := s.closed
	s.closed = true
	s.mu.Unlock()
	if already {
		return
	}
	s.broker.remove(s)
	select {
	case s.signal <- struct{}{}:
	default:
	}
}
