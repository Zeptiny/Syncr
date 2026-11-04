package outbox

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

func started(job string) *syncrv1.Event {
	return &syncrv1.Event{Payload: &syncrv1.Event_JobStarted{JobStarted: &syncrv1.JobStarted{JobId: job}}}
}

func progress(job string, bytes int64) *syncrv1.Event {
	return &syncrv1.Event{Payload: &syncrv1.Event_Progress{Progress: &syncrv1.Progress{JobId: job, Stats: &syncrv1.JobStats{Bytes: bytes}}}}
}

func result(job string) *syncrv1.Event {
	return &syncrv1.Event{Payload: &syncrv1.Event_JobResult{JobResult: &syncrv1.JobResult{JobId: job, Status: syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED}}}
}

func mustAppend(t *testing.T, o *Outbox, ev *syncrv1.Event) int64 {
	t.Helper()
	seq, err := o.Append(ev)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return seq
}

// describe renders events compactly: "1:hello 2:start(a) 3:progress(a,10)".
func describe(events []*syncrv1.Event) string {
	s := ""
	for i, e := range events {
		if i > 0 {
			s += " "
		}
		switch p := e.GetPayload().(type) {
		case *syncrv1.Event_Hello:
			s += fmt.Sprintf("%d:hello", e.GetSeq())
		case *syncrv1.Event_JobStarted:
			s += fmt.Sprintf("%d:start(%s)", e.GetSeq(), p.JobStarted.GetJobId())
		case *syncrv1.Event_Progress:
			s += fmt.Sprintf("%d:progress(%s,%d)", e.GetSeq(), p.Progress.GetJobId(), p.Progress.GetStats().GetBytes())
		case *syncrv1.Event_JobResult:
			s += fmt.Sprintf("%d:result(%s)", e.GetSeq(), p.JobResult.GetJobId())
		default:
			s += fmt.Sprintf("%d:?", e.GetSeq())
		}
	}
	return s
}

func TestNewStartsEpochWithHello(t *testing.T) {
	t.Parallel()
	hello := &syncrv1.Hello{WorkerVersion: "v-test"}
	o := New(hello, Options{})
	if _, err := uuid.Parse(o.Epoch()); err != nil {
		t.Errorf("epoch %q is not a UUID: %v", o.Epoch(), err)
	}
	if other := New(hello, Options{}); other.Epoch() == o.Epoch() {
		t.Errorf("two outboxes share epoch %q", o.Epoch())
	}
	events, more := o.Pending(0, 0)
	if len(events) != 1 || more || events[0].GetSeq() != 1 || events[0].GetHello().GetWorkerVersion() != "v-test" {
		t.Fatalf("Pending(0) = %s (more %v), want just the hello at seq 1", describe(events), more)
	}
	if events[0].GetTime() == nil {
		t.Error("hello has no time")
	}
	if New(nil, Options{Epoch: "fixed"}).Epoch() != "fixed" {
		t.Error("Options.Epoch not used")
	}
}

func TestAppendAssignsSeqAndTime(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		o := New(nil, Options{Epoch: "e"})
		time.Sleep(time.Minute)
		if seq := mustAppend(t, o, started("a")); seq != 2 {
			t.Errorf("first append seq = %d, want 2", seq)
		}
		if seq := mustAppend(t, o, progress("a", 1)); seq != 3 {
			t.Errorf("second append seq = %d, want 3", seq)
		}
		events, _ := o.Pending(1, 0)
		if got := describe(events); got != "2:start(a) 3:progress(a,1)" {
			t.Errorf("Pending(1) = %s", got)
		}
		if got := events[0].GetTime().AsTime(); !got.Equal(time.Now()) {
			t.Errorf("event time = %v, want %v", got, time.Now())
		}
	})
}

func TestAppendRejectsInvalidEvents(t *testing.T) {
	t.Parallel()
	o := New(nil, Options{})
	for _, ev := range []*syncrv1.Event{nil, {}, {Payload: &syncrv1.Event_Hello{Hello: &syncrv1.Hello{}}}} {
		if _, err := o.Append(ev); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("Append(%v) error = %v, want ErrInvalidEvent", ev, err)
		}
	}
	if st := o.Stats(); st.Head != 1 || st.Backlog != 1 {
		t.Errorf("stats after refused appends = %+v", st)
	}
}

func TestPendingLimitAndMore(t *testing.T) {
	t.Parallel()
	o := New(nil, Options{})
	for i := range 5 {
		mustAppend(t, o, progress("a", int64(i)))
	}
	tests := []struct {
		after int64
		limit int
		want  string
		more  bool
	}{
		{after: 0, limit: 0, want: "1:hello 2:progress(a,0) 3:progress(a,1) 4:progress(a,2) 5:progress(a,3) 6:progress(a,4)"},
		{after: 0, limit: 2, want: "1:hello 2:progress(a,0)", more: true},
		{after: 4, limit: 2, want: "5:progress(a,3) 6:progress(a,4)"},
		{after: 6, limit: 2, want: ""},
		{after: 99, limit: 0, want: ""},
		{after: -5, limit: 1, want: "1:hello", more: true},
	}
	for _, tt := range tests {
		events, more := o.Pending(tt.after, tt.limit)
		if got := describe(events); got != tt.want || more != tt.more {
			t.Errorf("Pending(%d, %d) = %q more=%v, want %q more=%v", tt.after, tt.limit, got, more, tt.want, tt.more)
		}
	}
}

func TestAckSemantics(t *testing.T) {
	t.Parallel()
	o := New(nil, Options{})
	for i := range 4 {
		mustAppend(t, o, progress("a", int64(i))) // seqs 2..5
	}

	if n, err := o.Ack(3); n != 3 || err != nil {
		t.Fatalf("Ack(3) = %d, %v; want 3 dropped", n, err)
	}
	// Duplicate and backwards acks are no-ops.
	for _, seq := range []int64{3, 2, 0, -1} {
		if n, err := o.Ack(seq); n != 0 || err != nil {
			t.Errorf("Ack(%d) after Ack(3) = %d, %v; want no-op", seq, n, err)
		}
	}
	// An ack beyond the head is refused and changes nothing.
	if n, err := o.Ack(6); n != 0 || !errors.Is(err, ErrAckAhead) {
		t.Errorf("Ack(6) with head 5 = %d, %v; want ErrAckAhead", n, err)
	}
	if events, _ := o.Pending(0, 0); describe(events) != "4:progress(a,2) 5:progress(a,3)" {
		t.Errorf("after acks: %s", describe(events))
	}
	if st := o.Stats(); st.Acked != 3 || st.Head != 5 || st.Backlog != 2 {
		t.Errorf("stats = %+v", st)
	}
	if n, err := o.Ack(5); n != 2 || err != nil {
		t.Errorf("Ack(5) = %d, %v", n, err)
	}
	if events, more := o.Pending(0, 0); len(events) != 0 || more {
		t.Errorf("after full ack: %s", describe(events))
	}
	// Seqs keep increasing after everything was acked.
	if seq := mustAppend(t, o, result("a")); seq != 6 {
		t.Errorf("seq after drain = %d, want 6", seq)
	}
}

func TestAckInsideCoalescingGap(t *testing.T) {
	t.Parallel()
	o := New(nil, Options{CoalesceAt: 1, HighWater: 100})
	mustAppend(t, o, progress("a", 1)) // 2
	mustAppend(t, o, progress("a", 2)) // 3 replaces 2
	mustAppend(t, o, started("b"))     // 4
	// The controller acks seq 2, which was coalesced away: nothing to drop,
	// but the ack level moves.
	if n, err := o.Ack(2); n != 1 || err != nil { // drops the hello only
		t.Errorf("Ack(2) = %d, %v", n, err)
	}
	if events, _ := o.Pending(0, 0); describe(events) != "3:progress(a,2) 4:start(b)" {
		t.Errorf("Pending = %s", describe(events))
	}
}

func TestCoalescing(t *testing.T) {
	t.Parallel()
	o := New(nil, Options{CoalesceAt: 6, HighWater: 100})
	mustAppend(t, o, started("a"))     // 2
	mustAppend(t, o, progress("a", 1)) // 3
	mustAppend(t, o, started("b"))     // 4
	mustAppend(t, o, progress("a", 2)) // 5
	mustAppend(t, o, progress("b", 1)) // 6: backlog reaches 6
	// At the limit: a's progress events collapse into the newest.
	mustAppend(t, o, progress("a", 3)) // 7 replaces 3 and 5: backlog 5
	mustAppend(t, o, progress("b", 2)) // 8: below the limit, appended: backlog 6
	mustAppend(t, o, progress("a", 4)) // 9 replaces 7
	events, _ := o.Pending(0, 0)
	if got, want := describe(events), "1:hello 2:start(a) 4:start(b) 6:progress(b,1) 8:progress(b,2) 9:progress(a,4)"; got != want {
		t.Fatalf("after coalescing:\n got %s\nwant %s", got, want)
	}
	// Below the limit again, progress is not coalesced.
	if _, err := o.Ack(4); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, o, progress("a", 5)) // 10
	mustAppend(t, o, progress("a", 6)) // 11
	events, _ = o.Pending(0, 0)
	if got, want := describe(events), "6:progress(b,1) 8:progress(b,2) 9:progress(a,4) 10:progress(a,5) 11:progress(a,6)"; got != want {
		t.Fatalf("below the limit:\n got %s\nwant %s", got, want)
	}
	if st := o.Stats(); st.Coalesced != 3 {
		t.Errorf("Coalesced = %d, want 3", st.Coalesced)
	}
}

func TestCoalescingOnResultKeepsLatestProgress(t *testing.T) {
	t.Parallel()
	o := New(nil, Options{CoalesceAt: 3, HighWater: 100})
	mustAppend(t, o, started("a"))     // 2
	mustAppend(t, o, progress("a", 1)) // 3
	mustAppend(t, o, progress("b", 1)) // 4
	mustAppend(t, o, progress("a", 2)) // 5 replaces 3
	mustAppend(t, o, result("a"))      // 6: keeps 5
	mustAppend(t, o, result("b"))      // 7: keeps 4
	events, _ := o.Pending(0, 0)
	if got, want := describe(events), "1:hello 2:start(a) 4:progress(b,1) 5:progress(a,2) 6:result(a) 7:result(b)"; got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestReadWithheldResults(t *testing.T) {
	t.Parallel()
	o := New(nil, Options{})
	mustAppend(t, o, started("a")) // 2
	mustAppend(t, o, result("a"))  // 3
	mustAppend(t, o, started("b")) // 4
	mustAppend(t, o, result("b"))  // 5

	ids := func(rs []*syncrv1.JobResult) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.GetJobId())
		}
		return out
	}
	b := o.Read(0, 2)
	if describe(b.Events) != "1:hello 2:start(a)" || !b.More || fmt.Sprint(ids(b.Withheld)) != "[a b]" {
		t.Errorf("Read(0,2) = %s more=%v withheld=%v", describe(b.Events), b.More, ids(b.Withheld))
	}
	b = o.Read(0, 3)
	if fmt.Sprint(ids(b.Withheld)) != "[b]" {
		t.Errorf("Read(0,3) withheld = %v", ids(b.Withheld))
	}
	b = o.Read(5, 0)
	if len(b.Events) != 0 || b.More || len(b.Withheld) != 0 {
		t.Errorf("Read(5,0) = %+v", b)
	}
	b = o.Read(0, 0)
	if b.More || len(b.Withheld) != 0 {
		t.Errorf("Read(0,0) withheld %v", ids(b.Withheld))
	}
}

func TestReadBounded(t *testing.T) {
	t.Parallel()
	o := New(nil, Options{HighWater: 4, CoalesceAt: 4})
	big := func(job string, n int) *syncrv1.Event {
		return &syncrv1.Event{Payload: &syncrv1.Event_JobResult{JobResult: &syncrv1.JobResult{
			JobId: job, Status: syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED, Warnings: []string{strings.Repeat("x", n)},
		}}}
	}
	mustAppend(t, o, started("a"))      // 2
	mustAppend(t, o, big("a", 1000))    // 3
	mustAppend(t, o, started("b"))      // 4
	mustAppend(t, o, big("b", 100_000)) // 5
	mustAppend(t, o, started("c"))      // 6

	// The budget stops before the event that would exceed it; the withheld
	// results and the stats describe the whole outbox.
	b := o.ReadBounded(1, 0, 2000)
	if describe(b.Events) != "2:start(a) 3:result(a) 4:start(b)" || !b.More || len(b.Withheld) != 1 || b.Withheld[0].GetJobId() != "b" {
		t.Errorf("ReadBounded(1,0,2000) = %s more=%v withheld=%d", describe(b.Events), b.More, len(b.Withheld))
	}
	if b.Head != 6 || b.Backlog != 6 || !b.Saturated {
		t.Errorf("stats: head %d backlog %d saturated %v", b.Head, b.Backlog, b.Saturated)
	}
	// An event larger than the budget is still returned on its own.
	b = o.ReadBounded(4, 0, 10)
	if describe(b.Events) != "5:result(b)" || !b.More {
		t.Errorf("ReadBounded(4,0,10) = %s more=%v", describe(b.Events), b.More)
	}
	// The event limit applies too; zero means unbounded.
	if b = o.ReadBounded(0, 2, 1<<30); describe(b.Events) != "1:hello 2:start(a)" {
		t.Errorf("ReadBounded(0,2,1G) = %s", describe(b.Events))
	}
	if b = o.ReadBounded(0, 0, 0); len(b.Events) != 6 || b.More {
		t.Errorf("ReadBounded(0,0,0) = %s", describe(b.Events))
	}
	if b = o.ReadBounded(6, 0, 10); len(b.Events) != 0 || b.More || b.Head != 6 {
		t.Errorf("ReadBounded(6,0,10) = %+v", b)
	}
}

func TestNotify(t *testing.T) {
	t.Parallel()
	o := New(nil, Options{})
	select {
	case <-o.Notify():
		t.Fatal("notified before any append")
	default:
	}
	mustAppend(t, o, started("a"))
	mustAppend(t, o, progress("a", 1))
	select {
	case <-o.Notify():
	default:
		t.Fatal("no notification after append")
	}
	select {
	case <-o.Notify():
		t.Fatal("signals are not coalesced")
	default:
	}
}

func TestWaitCapacityAndDrained(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		o := New(nil, Options{CoalesceAt: 2, HighWater: 3})
		mustAppend(t, o, started("a")) // backlog 2
		if err := o.WaitCapacity(t.Context()); err != nil {
			t.Fatalf("WaitCapacity below high water: %v", err)
		}
		mustAppend(t, o, started("b")) // backlog 3 = high water
		if !o.Stats().Saturated {
			t.Error("not saturated at high water")
		}

		var capacity, drained error
		capDone, drainDone := make(chan struct{}), make(chan struct{})
		go func() { capacity = o.WaitCapacity(t.Context()); close(capDone) }()
		go func() { drained = o.WaitDrained(t.Context()); close(drainDone) }()
		synctest.Wait()
		select {
		case <-capDone:
			t.Fatal("WaitCapacity returned while saturated")
		default:
		}

		// Appends never block, even when saturated.
		mustAppend(t, o, result("a"))
		if _, err := o.Ack(1); err != nil { // backlog 3: still saturated
			t.Fatal(err)
		}
		synctest.Wait()
		select {
		case <-capDone:
			t.Fatal("WaitCapacity returned while still saturated")
		default:
		}
		if _, err := o.Ack(2); err != nil { // backlog 2
			t.Fatal(err)
		}
		<-capDone
		if capacity != nil {
			t.Errorf("WaitCapacity = %v", capacity)
		}
		synctest.Wait()
		select {
		case <-drainDone:
			t.Fatal("WaitDrained returned with a backlog")
		default:
		}
		if _, err := o.Ack(o.Stats().Head); err != nil {
			t.Fatal(err)
		}
		<-drainDone
		if drained != nil {
			t.Errorf("WaitDrained = %v", drained)
		}

		// A done context ends the wait with its cause.
		mustAppend(t, o, started("c"))
		cause := errors.New("stop waiting")
		ctx, cancel := context.WithCancelCause(t.Context())
		cancel(cause)
		if err := o.WaitDrained(ctx); !errors.Is(err, cause) {
			t.Errorf("WaitDrained(cancelled) = %v, want %v", err, cause)
		}
	})
}

// TestConcurrentAppendsAndAcks hammers the outbox from several producers
// while a consumer reads and acks, then checks that the consumer saw every
// non-Progress event exactly once, in increasing seq order, and the final
// Progress sample of every job. Run with -count=50.
func TestConcurrentAppendsAndAcks(t *testing.T) {
	t.Parallel()
	const producers, perProducer = 8, 300
	o := New(nil, Options{CoalesceAt: 16, HighWater: 64})

	var wg sync.WaitGroup
	for p := range producers {
		wg.Go(func() {
			job := fmt.Sprintf("job-%d", p)
			mustAppend(t, o, started(job))
			for i := range perProducer {
				mustAppend(t, o, progress(job, int64(i+1)))
			}
			mustAppend(t, o, result(job))
		})
	}
	producersDone := make(chan struct{})
	go func() { wg.Wait(); close(producersDone) }()

	var (
		lastSeq   int64
		started   = map[string]int{}
		results   = map[string]int{}
		lastBytes = map[string]int64{}
		hellos    int
	)
	consume := func() {
		events, _ := o.Pending(lastSeq, 7)
		for _, e := range events {
			if e.GetSeq() <= lastSeq {
				t.Fatalf("seq %d after %d", e.GetSeq(), lastSeq)
			}
			lastSeq = e.GetSeq()
			switch p := e.GetPayload().(type) {
			case *syncrv1.Event_Hello:
				hellos++
			case *syncrv1.Event_JobStarted:
				started[p.JobStarted.GetJobId()]++
			case *syncrv1.Event_Progress:
				job := p.Progress.GetJobId()
				if started[job] != 1 || results[job] != 0 {
					t.Fatalf("progress of %s outside start/result", job)
				}
				b := p.Progress.GetStats().GetBytes()
				if b <= lastBytes[job] {
					t.Fatalf("progress of %s went from %d to %d", job, lastBytes[job], b)
				}
				lastBytes[job] = b
			case *syncrv1.Event_JobResult:
				results[p.JobResult.GetJobId()]++
			}
		}
		if len(events) > 0 {
			if _, err := o.Ack(lastSeq); err != nil {
				t.Fatal(err)
			}
		}
	}
	for done := false; !done; {
		select {
		case <-producersDone:
			done = true
		default:
			consume()
		}
	}
	for o.Stats().Backlog > 0 {
		consume()
	}

	if hellos != 1 {
		t.Errorf("hellos = %d", hellos)
	}
	for p := range producers {
		job := fmt.Sprintf("job-%d", p)
		if started[job] != 1 || results[job] != 1 {
			t.Errorf("%s: started %d, results %d; want exactly one each", job, started[job], results[job])
		}
		if lastBytes[job] != perProducer {
			t.Errorf("%s: last progress %d, want the final sample %d", job, lastBytes[job], perProducer)
		}
	}
	if st := o.Stats(); st.Head != 1+producers*(perProducer+2) {
		t.Errorf("head = %d", st.Head)
	}
}

// TestBacklogStaysBoundedAndNeverLosesResults appends a random mix of
// events while a slow, lossy consumer acks occasionally. However the
// backlog evolves, every JobStarted and JobResult must reach the consumer,
// and with few concurrent jobs the backlog stays near CoalesceAt.
func TestBacklogStaysBoundedAndNeverLosesResults(t *testing.T) {
	t.Parallel()
	for seed := range uint64(20) {
		rng := rand.New(rand.NewPCG(seed, 42))
		const coalesceAt, jobs = 32, 200
		o := New(nil, Options{CoalesceAt: coalesceAt, HighWater: 64})
		want := map[string]bool{}
		got := map[string]bool{}
		var acked int64
		running := []string{}
		next := 0
		for next < jobs || len(running) > 0 {
			switch r := rng.IntN(10); {
			case r == 0 && next < jobs && len(running) < 3:
				job := fmt.Sprintf("j%d", next)
				next++
				running = append(running, job)
				mustAppend(t, o, started(job))
				want["start "+job] = true
			case r == 1 && len(running) > 0:
				i := rng.IntN(len(running))
				job := running[i]
				running = append(running[:i], running[i+1:]...)
				mustAppend(t, o, result(job))
				want["result "+job] = true
			case r < 8 && len(running) > 0:
				mustAppend(t, o, progress(running[rng.IntN(len(running))], int64(rng.IntN(1000))))
			default:
				// The consumer reads a page; sometimes the ack is lost.
				events, _ := o.Pending(acked, 1+rng.IntN(5))
				for _, e := range events {
					if s := e.GetJobStarted(); s != nil {
						got["start "+s.GetJobId()] = true
					}
					if r := e.GetJobResult(); r != nil {
						got["result "+r.GetJobId()] = true
					}
				}
				if len(events) > 0 && rng.IntN(3) > 0 {
					acked = events[len(events)-1].GetSeq()
					if _, err := o.Ack(acked); err != nil {
						t.Fatal(err)
					}
				}
			}
			checkProgressBound(t, o, coalesceAt)
		}
		for o.Stats().Backlog > 0 {
			events, _ := o.Pending(acked, 0)
			for _, e := range events {
				if s := e.GetJobStarted(); s != nil {
					got["start "+s.GetJobId()] = true
				}
				if r := e.GetJobResult(); r != nil {
					got["result "+r.GetJobId()] = true
				}
			}
			acked = events[len(events)-1].GetSeq()
			if _, err := o.Ack(acked); err != nil {
				t.Fatal(err)
			}
		}
		for k := range want {
			if !got[k] {
				t.Errorf("seed %d: %s never delivered", seed, k)
			}
		}
		if len(got) != len(want) {
			t.Errorf("seed %d: delivered %d distinct start/result events, want %d", seed, len(got), len(want))
		}
	}
}

// checkProgressBound asserts the coalescing bound: the backlog never holds
// more Progress events than CoalesceAt plus one per job (without coalescing
// they would grow with every sample).
func checkProgressBound(t *testing.T, o *Outbox, coalesceAt int) {
	t.Helper()
	events, _ := o.Pending(0, 0)
	n, jobs := 0, map[string]bool{}
	for _, e := range events {
		if p := e.GetProgress(); p != nil {
			n++
			jobs[p.GetJobId()] = true
		}
	}
	if n > coalesceAt+len(jobs) {
		t.Fatalf("%d progress events of %d jobs in the backlog, bound %d", n, len(jobs), coalesceAt+len(jobs))
	}
}
