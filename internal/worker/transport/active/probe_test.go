package active

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/linkcore"
)

// CompleteProbe implements ControllerLinkService: it fails with the next
// scripted error, if any, and records the result otherwise.
func (f *fakeController) CompleteProbe(_ context.Context, req *connect.Request[syncrv1.CompleteProbeRequest]) (*connect.Response[syncrv1.CompleteProbeResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probeCalls++
	f.auth = append(f.auth, req.Header().Get("Authorization"))
	if len(f.probeErrs) > 0 {
		err := f.probeErrs[0]
		f.probeErrs = f.probeErrs[1:]
		return nil, err
	}
	f.probes = append(f.probes, req.Msg.GetResult())
	f.probeEpochs = append(f.probeEpochs, req.Msg.GetEpoch())
	return connect.NewResponse(&syncrv1.CompleteProbeResponse{}), nil
}

func (f *fakeController) probeResults() (ids []string, calls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.probes {
		ids = append(ids, r.GetProbeId())
	}
	return ids, f.probeCalls
}

// slowProber answers each probe after delay.
type slowProber struct{ delay time.Duration }

func (p slowProber) Probe(ctx context.Context, rp *syncrv1.RunProbe) *syncrv1.ProbeResult {
	select {
	case <-ctx.Done():
		return &syncrv1.ProbeResult{Status: syncrv1.ProbeStatus_PROBE_STATUS_CANCELLED}
	case <-time.After(p.delay):
	}
	return &syncrv1.ProbeResult{Status: syncrv1.ProbeStatus_PROBE_STATUS_OK, Output: &syncrv1.ProbeResult_Listing{
		Listing: &syncrv1.ProbeListing{Path: rp.GetSpec().GetListLocal().GetPath()},
	}}
}

func runProbeCmd(id string) *syncrv1.Command {
	return &syncrv1.Command{Id: "probe:" + id, Command: &syncrv1.Command_RunProbe{RunProbe: &syncrv1.RunProbe{
		ProbeId: id, Spec: &syncrv1.ProbeSpec{Op: &syncrv1.ProbeSpec_ListLocal{ListLocal: &syncrv1.ProbeListLocal{Path: "/" + id}}},
	}}}
}

// TestProbeResultIsDelivered: a run_probe command is acked on the next
// Poll and its result is delivered with CompleteProbe, once, with the
// worker's epoch and credentials.
func TestProbeResultIsDelivered(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctrl := newFakeController()
		a, core, tr := setupWith(t, ctrl, stepRunner{}, nil, linkcore.WithProber(slowProber{delay: 2 * time.Second}))
		defer shutdownAgent(t, a)
		defer core.Close()
		stop := run(t, tr)
		defer stop()
		time.Sleep(time.Second)
		synctest.Wait()

		ctrl.queue(runProbeCmd("p1"))
		time.Sleep(time.Second)
		synctest.Wait()
		ctrl.mu.Lock()
		acked := slices.Clone(ctrl.acked)
		ctrl.mu.Unlock()
		if !slices.Contains(acked, "probe:p1") {
			t.Errorf("acked = %v, want the probe command acked before the probe ends", acked)
		}
		if ids, _ := ctrl.probeResults(); len(ids) != 0 {
			t.Fatalf("a result arrived before the probe finished: %v", ids)
		}

		time.Sleep(2 * time.Second)
		synctest.Wait()
		ids, calls := ctrl.probeResults()
		if !slices.Equal(ids, []string{"p1"}) || calls != 1 {
			t.Fatalf("delivered = %v in %d calls, want p1 once", ids, calls)
		}
		ctrl.mu.Lock()
		res, epoch := ctrl.probes[0], ctrl.probeEpochs[0]
		ctrl.mu.Unlock()
		if res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_OK || res.GetListing().GetPath() != "/p1" || epoch != a.Epoch() {
			t.Errorf("result = %v with epoch %q, want the worker's epoch %q", res, epoch, a.Epoch())
		}
		ctrl.mu.Lock()
		for _, h := range ctrl.auth {
			if h != link.ActiveAuthorization("w1", "s3cret") {
				t.Errorf("a call without the worker's credentials: %q", h)
			}
		}
		ctrl.mu.Unlock()

		// Nothing is sent again.
		time.Sleep(time.Minute)
		synctest.Wait()
		if _, again := ctrl.probeResults(); again != 1 {
			t.Errorf("%d CompleteProbe calls, want 1", again)
		}
		if len(core.PendingProbeResults()) != 0 {
			t.Error("the delivered result is still pending")
		}
	})
}

// TestProbeResultIsRetriedAfterTransportErrors: delivery backs off and
// tries again until the controller takes the result.
func TestProbeResultIsRetriedAfterTransportErrors(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctrl := newFakeController()
		unavailable := connect.NewError(connect.CodeUnavailable, errors.New("down"))
		ctrl.probeErrs = []error{unavailable, unavailable}
		a, core, tr := setupWith(t, ctrl, stepRunner{}, nil, linkcore.WithProber(slowProber{}))
		defer shutdownAgent(t, a)
		defer core.Close()
		stop := run(t, tr)
		defer stop()
		ctrl.queue(runProbeCmd("p1"))
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		if ids, calls := ctrl.probeResults(); len(ids) != 0 || calls != 1 {
			t.Fatalf("after the first failure: delivered %v in %d calls", ids, calls)
		}
		time.Sleep(10 * time.Second) // two backoffs: at most 1s, then at most 2s
		synctest.Wait()
		if ids, calls := ctrl.probeResults(); !slices.Equal(ids, []string{"p1"}) || calls != 3 {
			t.Errorf("delivered %v in %d calls, want p1 on the third", ids, calls)
		}
	})
}

// TestProbeResultTheControllerRefusesIsDropped: a result the controller
// will never take is not sent again.
func TestProbeResultTheControllerRefusesIsDropped(t *testing.T) {
	t.Parallel()
	for _, code := range []connect.Code{connect.CodeNotFound, connect.CodeFailedPrecondition, connect.CodeInvalidArgument, connect.CodeUnimplemented} {
		t.Run(code.String(), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				ctrl := newFakeController()
				ctrl.probeErrs = []error{connect.NewError(code, errors.New("no"))}
				a, core, tr := setupWith(t, ctrl, stepRunner{}, nil, linkcore.WithProber(slowProber{}))
				defer shutdownAgent(t, a)
				defer core.Close()
				stop := run(t, tr)
				defer stop()
				ctrl.queue(runProbeCmd("p1"))
				time.Sleep(time.Minute)
				synctest.Wait()
				if ids, calls := ctrl.probeResults(); len(ids) != 0 || calls != 1 {
					t.Errorf("delivered %v in %d calls, want one refused call", ids, calls)
				}
				if len(core.PendingProbeResults()) != 0 {
					t.Error("the refused result is still pending")
				}
			})
		})
	}
}

// TestUndeliverableProbeResultExpires: with the controller unreachable the
// loop keeps backing off and stops once the core dropped the result.
func TestUndeliverableProbeResultExpires(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctrl := newFakeController()
		a, core, tr := setupWith(t, ctrl, stepRunner{}, nil, linkcore.WithProber(slowProber{}))
		defer shutdownAgent(t, a)
		defer core.Close()
		stop := run(t, tr)
		defer stop()
		ctrl.queue(runProbeCmd("p1"))
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		ctrl.mu.Lock()
		ctrl.probes = nil
		for range 100 {
			ctrl.probeErrs = append(ctrl.probeErrs, connect.NewError(connect.CodeUnavailable, errors.New("down")))
		}
		ctrl.mu.Unlock()
		// A second probe whose delivery always fails.
		ctrl.queue(runProbeCmd("p2"))
		time.Sleep(linkcore.ProbeResultRetention + time.Minute)
		synctest.Wait()
		_, calls := ctrl.probeResults()
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if _, later := ctrl.probeResults(); later != calls {
			t.Errorf("%d more CompleteProbe calls after the result expired", later-calls)
		}
		// Backoff, not a spin: about one call per 30s at most once backed off.
		if calls > 20 {
			t.Errorf("%d CompleteProbe calls in %v", calls, linkcore.ProbeResultRetention+time.Minute)
		}
	})
}
