package passive

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/linkcore"
)

// echoProber answers a probe with its path, or waits for the call to end
// when the path is "/wait".
type echoProber struct{ cancelled chan struct{} }

func (p echoProber) Probe(ctx context.Context, rp *syncrv1.RunProbe) *syncrv1.ProbeResult {
	path := rp.GetSpec().GetListLocal().GetPath()
	if path == "/wait" {
		<-ctx.Done()
		close(p.cancelled)
		return &syncrv1.ProbeResult{Status: syncrv1.ProbeStatus_PROBE_STATUS_CANCELLED}
	}
	return &syncrv1.ProbeResult{Status: syncrv1.ProbeStatus_PROBE_STATUS_OK, Output: &syncrv1.ProbeResult_Listing{
		Listing: &syncrv1.ProbeListing{Path: path},
	}}
}

func probeReq(id, path string) *syncrv1.ProbeRequest {
	return &syncrv1.ProbeRequest{Probe: &syncrv1.RunProbe{
		ProbeId: id, Spec: &syncrv1.ProbeSpec{Op: &syncrv1.ProbeSpec_ListLocal{ListLocal: &syncrv1.ProbeListLocal{Path: path}}},
	}}
}

func TestProbe(t *testing.T) {
	t.Parallel()
	prober := echoProber{cancelled: make(chan struct{})}
	f := newFixtureWith(t, 0, linkcore.WithProber(prober))
	ctx := context.Background()

	resp, err := f.client.Probe(ctx, authed(probeReq("p1", "/srv"), secret))
	if err != nil {
		t.Fatal(err)
	}
	if r := resp.Msg.GetResult(); r.GetProbeId() != "p1" || r.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_OK || r.GetListing().GetPath() != "/srv" {
		t.Errorf("result = %v", r)
	}

	// The probe is behind the same credentials as every other call.
	for name, s := range map[string]string{"none": "", "wrong": "not-the-secret"} {
		_, err := f.client.Probe(ctx, authed(probeReq("p2", "/srv"), s))
		if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("%s credentials: %v, want unauthenticated", name, err)
		}
	}

	// A probe without an ID or a spec is refused.
	for _, bad := range []*syncrv1.ProbeRequest{{}, {Probe: &syncrv1.RunProbe{ProbeId: "x"}}, {Probe: &syncrv1.RunProbe{Spec: &syncrv1.ProbeSpec{}}}} {
		if _, err := f.client.Probe(ctx, authed(bad, secret)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("Probe(%v) = %v, want invalid argument", bad, err)
		}
	}

	// Ending the call cancels the probe on the worker.
	callCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := f.client.Probe(callCtx, authed(probeReq("p3", "/wait"), secret))
		done <- err
	}()
	time.Sleep(100 * time.Millisecond) // the call is in flight
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) && connect.CodeOf(err) != connect.CodeCanceled {
		t.Errorf("cancelled call = %v", err)
	}
	select {
	case <-prober.cancelled:
	case <-time.After(10 * time.Second):
		t.Error("the probe was not cancelled when its call ended")
	}
}

func TestProbeWithoutAProber(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 0)
	resp, err := f.client.Probe(context.Background(), authed(probeReq("p1", "/srv"), secret))
	if err != nil {
		t.Fatal(err)
	}
	// The worker answers; the probe itself is what it cannot run.
	if r := resp.Msg.GetResult(); r.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_FAILED || r.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSUPPORTED_KIND {
		t.Errorf("result = %v", r)
	}
}

// commandOnlyCore is a link core from before probes.
type commandOnlyCore struct{}

func (commandOnlyCore) HandleCommandRequest(context.Context, *syncrv1.CommandRequest) *syncrv1.CommandResponse {
	return &syncrv1.CommandResponse{}
}
func (commandOnlyCore) HandlePull(*syncrv1.PullRequest) *syncrv1.PullResponse {
	return &syncrv1.PullResponse{}
}
func (commandOnlyCore) WaitDrained(context.Context) error { return nil }

func TestProbeIsUnimplementedWithoutAProbeCore(t *testing.T) {
	t.Parallel()
	h := &service{core: commandOnlyCore{}}
	_, err := h.Probe(context.Background(), connect.NewRequest(probeReq("p1", "/")))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("err = %v, want unimplemented", err)
	}
}
