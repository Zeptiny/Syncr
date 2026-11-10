package linkcore

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/agent"
)

// fakeProber answers each probe after delay, or when its context ends.
type fakeProber struct {
	delay time.Duration

	mu        sync.Mutex
	runs      []string
	cancelled []string
}

func (f *fakeProber) Probe(ctx context.Context, p *syncrv1.RunProbe) *syncrv1.ProbeResult {
	f.mu.Lock()
	f.runs = append(f.runs, p.GetProbeId())
	f.mu.Unlock()
	select {
	case <-ctx.Done():
		f.mu.Lock()
		f.cancelled = append(f.cancelled, p.GetProbeId())
		f.mu.Unlock()
		return &syncrv1.ProbeResult{Status: syncrv1.ProbeStatus_PROBE_STATUS_CANCELLED}
	case <-time.After(f.delay):
	}
	return &syncrv1.ProbeResult{Status: syncrv1.ProbeStatus_PROBE_STATUS_OK, Output: &syncrv1.ProbeResult_Listing{
		Listing: &syncrv1.ProbeListing{Path: p.GetSpec().GetListLocal().GetPath()},
	}}
}

func (f *fakeProber) ran() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.runs)
}

func (f *fakeProber) wasCancelled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.cancelled)
}

func newProbeCore(t *testing.T, p Prober) (*Core, *agent.Agent) {
	t.Helper()
	core, a := newCore(t, newStepRunner(time.Second), agent.Config{})
	if p != nil {
		WithProber(p)(core)
	}
	return core, a
}

func runProbe(id, path string) *syncrv1.RunProbe {
	return &syncrv1.RunProbe{ProbeId: id, Spec: &syncrv1.ProbeSpec{Op: &syncrv1.ProbeSpec_ListLocal{ListLocal: &syncrv1.ProbeListLocal{Path: path}}}}
}

func runProbeCmd(id string) *syncrv1.Command {
	return &syncrv1.Command{Id: "probe:" + id, Command: &syncrv1.Command_RunProbe{RunProbe: runProbe(id, "/"+id)}}
}

func cancelProbeCmd(id string) *syncrv1.Command {
	return &syncrv1.Command{Id: "probe-cancel:" + id, Command: &syncrv1.Command_CancelProbe{CancelProbe: &syncrv1.CancelProbe{ProbeId: id}}}
}

func poll(c *Core, cmds ...*syncrv1.Command) []string {
	req := c.BuildPoll(0)
	return ackIDs(c.ApplyPollResponse(context.Background(), req, &syncrv1.PollResponse{Commands: cmds}))
}

func resultIDs(rs []*syncrv1.ProbeResult) []string {
	ids := make([]string, len(rs))
	for i, r := range rs {
		ids[i] = r.GetProbeId()
	}
	return ids
}

func notified(c *Core) bool {
	select {
	case <-c.ProbeNotify():
		return true
	default:
		return false
	}
}

// TestActiveProbeRunsOnceAndWaitsForDelivery: a run_probe command is acked,
// runs outside the agent, and its result stays pending until delivered; a
// redelivered command does not run it again.
func TestActiveProbeRunsOnceAndWaitsForDelivery(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fp := &fakeProber{delay: 2 * time.Second}
		core, a := newProbeCore(t, fp)
		defer shutdown(t, a)
		defer core.Close()

		// A job command in the same Poll still reaches the agent.
		if got := poll(core, runProbeCmd("p1"), startCmd("start:j", "j")); !slices.Equal(got, []string{"probe:p1", "start:j"}) {
			t.Fatalf("acks = %v", got)
		}
		synctest.Wait()
		if len(core.PendingProbeResults()) != 0 || notified(core) {
			t.Fatal("a result before the probe finished")
		}
		if n := len(a.Snapshot(0, 10, 1<<20).State.GetJobs()); n != 1 {
			t.Errorf("the agent runs %d jobs, want the one job (probes take no job slot)", n)
		}
		// Redelivered before and after it finishes: acked, not run again.
		if got := poll(core, runProbeCmd("p1")); !slices.Equal(got, []string{"probe:p1"}) {
			t.Errorf("redelivery acks = %v", got)
		}
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if !notified(core) {
			t.Error("no notification for the finished probe")
		}
		pending := core.PendingProbeResults()
		if len(pending) != 1 || pending[0].GetProbeId() != "p1" || pending[0].GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_OK ||
			pending[0].GetListing().GetPath() != "/p1" {
			t.Fatalf("pending = %v", pending)
		}
		poll(core, runProbeCmd("p1"))
		synctest.Wait()
		if got := fp.ran(); !slices.Equal(got, []string{"p1"}) {
			t.Errorf("probe runs = %v, want one", got)
		}
		// Still pending until delivered, then gone for good.
		if len(core.PendingProbeResults()) != 1 {
			t.Error("the result did not stay pending")
		}
		core.ProbeDelivered("p1")
		core.ProbeDelivered("unknown")
		if len(core.PendingProbeResults()) != 0 {
			t.Error("a delivered result is still pending")
		}
		poll(core, runProbeCmd("p1"))
		synctest.Wait()
		if got := fp.ran(); len(got) != 1 {
			t.Errorf("a delivered probe ran again: %v", got)
		}
	})
}

func TestActiveProbeCancel(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fp := &fakeProber{delay: time.Minute}
		core, a := newProbeCore(t, fp)
		defer shutdown(t, a)
		defer core.Close()
		poll(core, runProbeCmd("p1"), runProbeCmd("p2"))
		synctest.Wait()
		// Canceling an unknown probe is acked and ignored.
		if got := poll(core, cancelProbeCmd("p1"), cancelProbeCmd("nope")); !slices.Equal(got, []string{"probe-cancel:p1", "probe-cancel:nope"}) {
			t.Errorf("acks = %v", got)
		}
		synctest.Wait()
		if got := fp.wasCancelled(); !slices.Equal(got, []string{"p1"}) {
			t.Errorf("cancelled = %v, want p1 only", got)
		}
		pending := core.PendingProbeResults()
		if len(pending) != 1 || pending[0].GetProbeId() != "p1" || pending[0].GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_CANCELLED {
			t.Errorf("pending = %v", pending)
		}
		// Canceling a finished probe changes nothing.
		poll(core, cancelProbeCmd("p1"))
		time.Sleep(ProbeResultRetention)
		synctest.Wait()
		if got := resultIDs(core.PendingProbeResults()); !slices.Equal(got, []string{"p2"}) {
			// p1 expired meanwhile (see TestProbeResultsExpire); p2 finished.
			t.Errorf("pending = %v", got)
		}
	})
}

// TestCancelBeforeRun: the controller cancels a probe it gave up on as soon
// as its run_probe may have been sent, so the cancel can come first: in an
// earlier Poll, in the same one (either order), or before a redelivery.
// The probe never starts, and the cancelled ID is remembered for
// ProbeDedupeRetention.
func TestCancelBeforeRun(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fp := &fakeProber{delay: time.Minute}
		core, a := newProbeCore(t, fp)
		defer shutdown(t, a)
		defer core.Close()

		// An earlier Poll.
		if got := poll(core, cancelProbeCmd("early")); !slices.Equal(got, []string{"probe-cancel:early"}) {
			t.Errorf("acks = %v", got)
		}
		if got := poll(core, runProbeCmd("early")); !slices.Equal(got, []string{"probe:early"}) {
			t.Errorf("acks = %v", got)
		}
		// The same Poll, in both orders.
		if got := poll(core, cancelProbeCmd("before"), runProbeCmd("before"), runProbeCmd("after"), cancelProbeCmd("after")); !slices.Equal(got,
			[]string{"probe-cancel:before", "probe:before", "probe:after", "probe-cancel:after"}) {
			t.Errorf("acks = %v", got)
		}
		synctest.Wait()
		if got := fp.ran(); len(got) != 0 {
			t.Fatalf("cancelled probes ran: %v", got)
		}
		if got := core.PendingProbeResults(); len(got) != 0 {
			t.Errorf("results of probes that never ran: %v", got)
		}

		// Started, cancelled, then redelivered: stopped and not run again.
		poll(core, runProbeCmd("started"))
		synctest.Wait()
		poll(core, cancelProbeCmd("started"))
		synctest.Wait()
		poll(core, runProbeCmd("started"))
		synctest.Wait()
		if got, stopped := fp.ran(), fp.wasCancelled(); !slices.Equal(got, []string{"started"}) || !slices.Equal(stopped, []string{"started"}) {
			t.Errorf("runs = %v, cancelled = %v; want one run, stopped", got, stopped)
		}

		// Remembered for the dedupe window, then forgotten.
		time.Sleep(ProbeDedupeRetention - time.Second)
		poll(core, runProbeCmd("early"))
		synctest.Wait()
		if slices.Contains(fp.ran(), "early") {
			t.Error("a cancelled ID was forgotten early")
		}
		time.Sleep(time.Second)
		poll(core, runProbeCmd("early"))
		synctest.Wait()
		if !slices.Contains(fp.ran(), "early") {
			t.Error("a cancelled ID is never forgotten")
		}
	})
}

// TestProbeRetentionFollowsTheCap: the retention of probe state is a
// multiple of the longest probe, and results expire before their IDs.
func TestProbeRetentionFollowsTheCap(t *testing.T) {
	t.Parallel()
	if ProbeResultRetention != 2*link.MaxProbeTimeout || ProbeDedupeRetention != 3*link.MaxProbeTimeout {
		t.Errorf("retention = %v, %v; want 2 and 3 times %v", ProbeResultRetention, ProbeDedupeRetention, link.MaxProbeTimeout)
	}
	if ProbeResultRetention <= link.MaxProbeTimeout+link.ProbeGrace {
		t.Error("a result may expire while the controller still waits for it")
	}
}

func TestProbeResultsExpire(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fp := &fakeProber{delay: time.Second}
		core, a := newProbeCore(t, fp)
		defer shutdown(t, a)
		defer core.Close()
		poll(core, runProbeCmd("p1"))
		time.Sleep(time.Second + ProbeResultRetention - time.Millisecond)
		synctest.Wait()
		if len(core.PendingProbeResults()) != 1 {
			t.Fatal("the result expired early")
		}
		time.Sleep(2 * time.Millisecond)
		if len(core.PendingProbeResults()) != 0 {
			t.Error("an undelivered result never expires")
		}
		// The ID is still remembered, so a late redelivery does not run it.
		poll(core, runProbeCmd("p1"))
		synctest.Wait()
		if got := fp.ran(); len(got) != 1 {
			t.Errorf("runs = %v", got)
		}
		// Long after, the ID is forgotten.
		time.Sleep(ProbeDedupeRetention)
		poll(core, runProbeCmd("p1"))
		synctest.Wait()
		if got := fp.ran(); len(got) != 2 {
			t.Errorf("runs = %v, want a second run once the ID was forgotten", got)
		}
	})
}

func TestProbesWithoutAProberAreUnsupported(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		core, a := newProbeCore(t, nil)
		defer shutdown(t, a)
		defer core.Close()
		if got := poll(core, runProbeCmd("p1")); !slices.Equal(got, []string{"probe:p1"}) {
			t.Errorf("acks = %v", got)
		}
		synctest.Wait()
		for _, res := range append(core.PendingProbeResults(), core.HandleProbe(t.Context(), &syncrv1.ProbeRequest{Probe: runProbe("p2", "/")}).GetResult()) {
			if res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_FAILED || res.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSUPPORTED_KIND {
				t.Errorf("result = %v, want FAILED/UNSUPPORTED_KIND", res)
			}
		}
	})
}

func TestPassiveProbe(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fp := &fakeProber{delay: time.Second}
		core, a := newProbeCore(t, fp)
		defer shutdown(t, a)
		defer core.Close()
		resp := core.HandleProbe(t.Context(), &syncrv1.ProbeRequest{Probe: runProbe("p1", "/srv")})
		if r := resp.GetResult(); r.GetProbeId() != "p1" || r.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_OK || r.GetListing().GetPath() != "/srv" {
			t.Errorf("result = %v", r)
		}
		// Nothing is kept for a passive probe.
		if len(core.PendingProbeResults()) != 0 {
			t.Error("a passive probe left a pending result")
		}
		// Ending the call ends the probe.
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()
		resp = core.HandleProbe(ctx, &syncrv1.ProbeRequest{Probe: runProbe("p2", "/")})
		if resp.GetResult().GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_CANCELLED || !slices.Equal(fp.wasCancelled(), []string{"p2"}) {
			t.Errorf("result = %v, cancelled = %v", resp.GetResult(), fp.wasCancelled())
		}
	})
}

func TestCloseCancelsProbesInFlight(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fp := &fakeProber{delay: time.Hour}
		core, a := newProbeCore(t, fp)
		defer shutdown(t, a)
		poll(core, runProbeCmd("p1"))
		passive := make(chan *syncrv1.ProbeResponse, 1)
		go func() {
			passive <- core.HandleProbe(context.Background(), &syncrv1.ProbeRequest{Probe: runProbe("p2", "/")})
		}()
		synctest.Wait()
		core.Close() // returns once the active probe ended
		if got := (<-passive).GetResult().GetStatus(); got != syncrv1.ProbeStatus_PROBE_STATUS_CANCELLED {
			t.Errorf("passive probe status = %v", got)
		}
		got := fp.wasCancelled()
		slices.Sort(got)
		if !slices.Equal(got, []string{"p1", "p2"}) {
			t.Errorf("cancelled = %v", got)
		}
		// After Close, a new active probe is refused and a passive one is
		// cancelled at once.
		poll(core, runProbeCmd("p3"))
		synctest.Wait()
		if slices.Contains(fp.ran(), "p3") {
			t.Error("a probe started after Close")
		}
		if got := core.HandleProbe(context.Background(), &syncrv1.ProbeRequest{Probe: runProbe("p4", "/")}).GetResult().GetStatus(); got != syncrv1.ProbeStatus_PROBE_STATUS_CANCELLED {
			t.Errorf("passive probe after Close = %v", got)
		}
	})
}
