package e2e

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/linkcore"
)

// scanProber is a worker's probe executor for the heavy probes: it answers
// a measurement that stopped early (complete=false), as at the soft
// deadline, or BUSY while busy is set, as a worker already scanning. It
// records the timeout of each probe.
type scanProber struct {
	busy     atomic.Bool
	timeouts chan time.Duration
}

var _ linkcore.Prober = (*scanProber)(nil)

func (p *scanProber) Probe(_ context.Context, rp *syncrv1.RunProbe) *syncrv1.ProbeResult {
	p.timeouts <- rp.GetTimeout().AsDuration()
	if p.busy.Load() {
		return &syncrv1.ProbeResult{Status: syncrv1.ProbeStatus_PROBE_STATUS_FAILED, Error: &syncrv1.JobError{
			Code: syncrv1.JobErrorCode_JOB_ERROR_CODE_BUSY, Message: "the worker is already measuring, searching or comparing; try again in a moment",
		}}
	}
	path := rp.GetSpec().GetMeasureLocal().GetPath()
	return &syncrv1.ProbeResult{Status: syncrv1.ProbeStatus_PROBE_STATUS_OK, Elapsed: durationpb.New(27 * time.Second), Output: &syncrv1.ProbeResult_Measurement{
		Measurement: &syncrv1.ProbeMeasurement{Path: path, Bytes: 1 << 40, Files: 340112, Dirs: 900, Complete: false,
			Children: []*syncrv1.ProbeChildMeasurement{{Name: "photos", Bytes: 1 << 40, Files: 340112, Dirs: 899}}},
	}}
}

func TestActiveScan(t *testing.T) {
	t.Parallel()
	testScan(t, false)
}

func TestPassiveScan(t *testing.T) {
	t.Parallel()
	testScan(t, true)
}

// testScan measures a worker's folder over one link mode: the partial
// result of a walk that stopped at the soft deadline is delivered as a
// success, a busy worker is unavailable, and a worker built before the
// heavy probes is never sent one.
func testScan(t *testing.T, passive bool) {
	p := &scanProber{timeouts: make(chan time.Duration, 8)}
	c := newController(t)
	pw := newProbeWorker(t, c, passive, upToDate(p))
	resp, err := c.probeService().MeasureWorkerFolder(t.Context(), testActor(), pw.id, "/data")
	if err != nil {
		t.Fatal(err)
	}
	m := resp.GetMeasurement()
	if m.GetPath() != "/data" || m.GetComplete() || m.GetFiles() != 340112 || len(m.GetChildren()) != 1 ||
		resp.GetElapsed().AsDuration() != 27*time.Second || resp.GetWorker().GetId() != pw.id.String() {
		t.Errorf("measurement = %v", resp)
	}
	if got := <-p.timeouts; got != scanTimeout {
		t.Errorf("the probe's timeout = %v, want the scan timeout %v", got, scanTimeout)
	}

	p.busy.Store(true)
	_, err = c.probeService().MeasureWorkerFolder(t.Context(), testActor(), pw.id, "/data")
	if !errors.Is(err, domain.ErrUnavailable) || !strings.Contains(err.Error(), "is already measuring or searching; try again in a moment") {
		t.Errorf("a busy worker = %v, want Unavailable", err)
	}
	<-p.timeouts

	// A worker of Milestone 6: it lists no heavy probes.
	c = newController(t)
	old := newProbeWorker(t, c, passive, workerOptions{
		prober: p, features: link.Features(), probeOps: slices.DeleteFunc(link.ProbeOps(), link.IsHeavyProbeOp),
	})
	_, err = c.probeService().MeasureWorkerFolder(t.Context(), testActor(), old.id, "/data")
	if !errors.Is(err, domain.ErrFailedPrecondition) || !strings.Contains(err.Error(), "upgrade") {
		t.Errorf("an old worker = %v, want FailedPrecondition", err)
	}
	select {
	case <-p.timeouts:
		t.Error("the old worker was sent a scan")
	default:
	}
}
