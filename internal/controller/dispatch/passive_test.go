package dispatch_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// fakePassive serves a fakeWorker's WorkerLinkService in process.
type fakePassive struct {
	mu    sync.Mutex
	w     *fakeWorker
	fail  int // fail the next n calls
	calls []string
	at    []time.Duration
	start time.Time
}

func (c *fakePassive) record(call string) error {
	c.calls = append(c.calls, call)
	c.at = append(c.at, time.Since(c.start))
	if c.fail > 0 {
		c.fail--
		return errors.New("connection refused")
	}
	return nil
}

func (c *fakePassive) Pull(_ context.Context, req *syncrv1.PullRequest) (*syncrv1.PullResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("pull"); err != nil {
		return nil, err
	}
	w := c.w
	if req.GetEpoch() == w.epoch && req.GetAfterSeq() <= w.seq {
		w.outbox = slices.DeleteFunc(w.outbox, func(ev *syncrv1.Event) bool { return ev.GetSeq() <= req.GetAfterSeq() })
	}
	n := len(w.outbox)
	if m := int(req.GetMaxEvents()); m > 0 && m < n {
		n = m
	}
	resp := &syncrv1.PullResponse{Epoch: w.epoch, Events: slices.Clone(w.outbox[:n]), HasMore: n < len(w.outbox), State: w.state()}
	if n > 0 {
		resp.State.LastSeq = w.outbox[n-1].GetSeq()
	}
	for _, ev := range w.outbox[n:] { // results not in this batch: still listed
		if r := ev.GetJobResult(); r != nil {
			resp.State.Jobs = append(resp.State.Jobs, &syncrv1.ActiveJob{JobId: r.GetJobId()})
		}
	}
	return resp, nil
}

func (c *fakePassive) Command(_ context.Context, req *syncrv1.CommandRequest) (*syncrv1.CommandResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("command"); err != nil {
		return nil, err
	}
	c.w.acks = nil
	c.w.handle(req.GetCommands())
	resp := &syncrv1.CommandResponse{Epoch: c.w.epoch, Acks: c.w.acks}
	c.w.acks = nil
	return resp, nil
}

func (c *fakePassive) do(fn func(w *fakeWorker)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c.w)
}

func (c *fakePassive) history() ([]string, []time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.calls), slices.Clone(c.at)
}

func (c *fakePassive) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls, c.at, c.start = nil, nil, time.Now()
}

func registerPassive(t *testing.T, e *env) domain.WorkerID {
	t.Helper()
	w, err := e.workers.RegisterPassive(context.Background(), workers.PassiveParams{
		Name: "nas", URL: "https://nas:7443", CertFingerprint: strings.Repeat("ab", 32), Secret: strings.Repeat("p", 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	return w.ID
}

func startLoop(t *testing.T, e *env, wid domain.WorkerID) (*fakePassive, func()) {
	t.Helper()
	c := &fakePassive{w: e.newFakeWorker(wid), start: time.Now()}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- e.disp.NewPassiveLoop(wid, c).Run(ctx) }()
	return c, func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run = %v", err)
		}
	}
}

func TestPassiveLoopRunsAJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		wid := registerPassive(t, e)
		c, stop := startLoop(t, e, wid)
		defer stop()

		synctest.Wait()
		calls, at := c.history()
		if !slices.Equal(calls, []string{"pull"}) || at[0] != 0 {
			t.Fatalf("first round: %v %v", calls, at)
		}
		w := e.worker(wid)
		if w.Link.Epoch != c.w.epoch || w.Info.Version != "v2.0.0-test" || e.workers.Status(w) != domain.WorkerOnline {
			t.Errorf("after the first pull: link %+v, info %+v", w.Link, w.Info)
		}

		// A new job wakes the idle loop at once.
		time.Sleep(4 * time.Second)
		c.reset()
		j := e.run(wid)
		synctest.Wait()
		if calls, at := c.history(); !slices.Equal(calls, []string{"pull", "command"}) || at[0] != 0 {
			t.Fatalf("after RunJob: %v %v", calls, at)
		}
		if got := e.job(j.ID); got.Status != domain.JobDispatched || got.Epoch != c.w.epoch {
			t.Fatalf("job = %s %q", got.Status, got.Epoch)
		}

		// Busy: about once a second.
		c.do(func(w *fakeWorker) { w.start(j.ID.String()); w.progress(j.ID.String(), 1<<30) })
		c.reset()
		time.Sleep(time.Second)
		synctest.Wait()
		if got := e.job(j.ID); got.Status != domain.JobRunning {
			t.Fatalf("job = %s", got.Status)
		}
		c.do(func(w *fakeWorker) { w.finish(j.ID.String(), syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED, nil) })
		time.Sleep(time.Second)
		synctest.Wait()
		if got := e.job(j.ID); got.Status != domain.JobSucceeded {
			t.Fatalf("job = %s", got.Status)
		}
		// The next pull, a second later, acknowledges the result; then the
		// worker is idle again and the next pull comes 15s after that.
		time.Sleep(16 * time.Second)
		synctest.Wait()
		_, at = c.history()
		if want := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 18 * time.Second}; !slices.Equal(at, want) {
			t.Errorf("pulls at %v, want %v", at, want)
		}
		if wk := e.worker(wid); wk.Link.AckSeq != 4 {
			t.Errorf("ack seq = %d, want 4", wk.Link.AckSeq)
		}
	})
}

func TestPassiveLoopDrainsBacklogAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.cfg.Passive.MaxEvents = 1
		e.restart()
		wid := registerPassive(t, e)
		c := &fakePassive{w: e.newFakeWorker(wid), start: time.Now()}
		j := e.run(wid)
		// The worker already accepted, ran and finished the job before the
		// controller's first pull (e.g. after a controller restart).
		c.do(func(w *fakeWorker) {
			w.handle([]*syncrv1.Command{{Id: "start:" + j.ID.String(), Command: &syncrv1.Command_StartJob{StartJob: &syncrv1.StartJob{JobId: j.ID.String()}}}})
			w.start(j.ID.String())
			w.finish(j.ID.String(), syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED, nil)
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = e.disp.NewPassiveLoop(wid, c).Run(ctx) }()
		synctest.Wait()
		calls, at := c.history()
		if len(calls) != 3 || at[2] != 0 {
			t.Errorf("calls %v at %v, want three pulls at once (has_more)", calls, at)
		}
		if got := e.job(j.ID); got.Status != domain.JobSucceeded {
			t.Errorf("job = %s (the withheld result must not count as lost)", got.Status)
		}
	})
}

func TestPassiveLoopBacksOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		wid := registerPassive(t, e)
		c := &fakePassive{w: e.newFakeWorker(wid), start: time.Now(), fail: 6}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- e.disp.NewPassiveLoop(wid, c).Run(ctx) }()
		synctest.Wait()
		e.run(wid) // a wake-up does not cut a backoff short
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		_, at := c.history()
		want := []time.Duration{0, 1 * time.Second, 3 * time.Second, 7 * time.Second, 15 * time.Second, 31 * time.Second, 61 * time.Second, 61 * time.Second, 62 * time.Second}
		if !slices.Equal(at[:len(want)], want) {
			t.Errorf("calls at %v, want %v (backoff 1,2,4,8,16,30s, then pull+command, then busy 1s)", at, want)
		}
		if w := e.worker(wid); w.Link.Epoch != c.w.epoch {
			t.Errorf("link = %+v", w.Link)
		}
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run = %v", err)
		}
	})
}

func TestPassiveFailuresDoNotCountAsContact(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		wid := registerPassive(t, e)
		c := &fakePassive{w: e.newFakeWorker(wid), start: time.Now(), fail: 1000}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = e.disp.NewPassiveLoop(wid, c).Run(ctx) }()
		time.Sleep(time.Minute)
		synctest.Wait()
		if st := e.workers.Status(e.worker(wid)); st != domain.WorkerPending {
			t.Errorf("status after failing calls = %s, want pending", st)
		}
	})
}

// Probe implements dispatch.PassiveClient (m5: Track B tests probes).
func (c *fakePassive) Probe(context.Context, *syncrv1.ProbeRequest) (*syncrv1.ProbeResponse, error) {
	return nil, errors.ErrUnsupported
}
