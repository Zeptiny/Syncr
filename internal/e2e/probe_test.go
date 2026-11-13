package e2e

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/linkcore"
)

// waitPath is the folder fakeProber never finishes listing: it answers only
// once its probe was cancelled.
const waitPath = "/wait"

// fakeProber is a worker's probe executor: it lists any folder at once
// (one file, notes.txt), except waitPath. It reports the probes it starts
// and the ones whose context ended before they answered.
type fakeProber struct {
	started, cancelled chan string
}

func newFakeProber() *fakeProber {
	return &fakeProber{started: make(chan string, 64), cancelled: make(chan string, 64)}
}

var _ linkcore.Prober = (*fakeProber)(nil)

func (p *fakeProber) Probe(ctx context.Context, rp *syncrv1.RunProbe) *syncrv1.ProbeResult {
	p.started <- rp.GetProbeId()
	path := rp.GetSpec().GetListLocal().GetPath()
	if path == waitPath {
		<-ctx.Done()
		p.cancelled <- rp.GetProbeId()
		return &syncrv1.ProbeResult{Status: syncrv1.ProbeStatus_PROBE_STATUS_CANCELLED,
			Error: &syncrv1.JobError{Code: syncrv1.JobErrorCode_JOB_ERROR_CODE_CANCELLED, Message: "probe cancelled"}}
	}
	return &syncrv1.ProbeResult{Status: syncrv1.ProbeStatus_PROBE_STATUS_OK, Output: &syncrv1.ProbeResult_Listing{Listing: &syncrv1.ProbeListing{
		Path: path, Entries: []*syncrv1.ProbeEntry{{Name: "notes.txt", Path: path + "/notes.txt", Type: syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_FILE, Size: 42}},
	}}}
}

// next waits for a probe ID on ch.
func next(t *testing.T, ch <-chan string, what string) string {
	t.Helper()
	select {
	case id := <-ch:
		return id
	case <-time.After(waitTimeout):
		t.Fatalf("no probe %s", what)
		return ""
	}
}

// probeWorker is a worker the controller probes, in one link mode, with a
// way to restart its process (a new agent, so a new epoch, at the same
// place).
type probeWorker struct {
	c       *controller
	id      domain.WorkerID
	prober  *fakeProber
	restart func(t *testing.T)
	epoch   func() string
}

// newProbeWorker starts a worker reporting o (with a fakeProber unless o
// has no features or a prober of its own) and waits until its Hello is
// stored.
func newProbeWorker(t *testing.T, c *controller, passive bool, o workerOptions) *probeWorker {
	t.Helper()
	pw := &probeWorker{c: c, prober: newFakeProber()}
	if o.features != nil && o.prober == nil {
		o.prober = pw.prober
	}
	if passive {
		certFile, keyFile, _ := selfSigned(t)
		ln := listen(t)
		addr := ln.Addr().String()
		secret := pairingSecret()
		w := startPassiveOnWith(t, newFakeRunner(), o, certFile, keyFile, secret, ln)
		_, ws := c.svc()
		reg, err := ws.RegisterPassive(context.Background(), workers.PassiveParams{Name: "server", URL: w.url, CertFingerprint: w.fingerprint, Secret: secret})
		if err != nil {
			t.Fatal(err)
		}
		pw.id = reg.ID
		c.wakeSupervisor()
		pw.epoch = w.agent.Epoch
		pw.restart = func(t *testing.T) {
			t.Helper()
			// The process stops as the worker's shutdown does: probes
			// first (their calls are answered as cancelled), then the
			// server.
			w.core.Close()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_ = w.server.Shutdown(ctx)
			ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			w = startPassiveOnWith(t, newFakeRunner(), o, certFile, keyFile, secret, ln)
			pw.epoch = w.agent.Epoch
		}
	} else {
		id, secret := c.enrollActive("desktop")
		pw.id = id
		w := startActiveWith(t, newFakeRunner(), o, activeOptions(c, id, secret))
		pw.epoch = w.agent.Epoch
		pw.restart = func(t *testing.T) {
			t.Helper()
			w.kill()
			w = startActiveWith(t, newFakeRunner(), o, activeOptions(c, id, secret))
			pw.epoch = w.agent.Epoch
		}
	}
	pw.waitHello(t)
	return pw
}

// waitHello waits until the controller stored the Hello of the worker's
// current process.
func (pw *probeWorker) waitHello(t *testing.T) {
	t.Helper()
	pw.c.waitWorker(pw.id, "the current process's hello", func(w workers.Worker) bool {
		return w.Link.Epoch == pw.epoch() && w.InfoCurrent() && w.Info.Hostname == "e2e"
	})
}

// list lists path on the worker as the API's ListWorkerFiles does.
func (pw *probeWorker) list(ctx context.Context, path string) (*syncrv1.ListWorkerFilesResponse, error) {
	return pw.c.probeService().ListWorkerFiles(ctx, testActor(), pw.id, path, false)
}

func TestActiveProbe(t *testing.T) {
	t.Parallel()
	testProbes(t, false)
}

func TestPassiveProbe(t *testing.T) {
	t.Parallel()
	testProbes(t, true)
}

// testProbes runs the probe cases over one link mode: delivery, a caller
// that gives up, a worker that does not answer in time, a worker that
// restarts mid-probe, and a worker too old for probes.
func testProbes(t *testing.T, passive bool) {
	t.Run("delivery", func(t *testing.T) {
		t.Parallel()
		pw := newProbeWorker(t, newController(t), passive, upToDate(nil))
		resp, err := pw.list(t.Context(), "/data")
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetPath() != "/data" || len(resp.GetEntries()) != 1 || resp.GetEntries()[0].GetName() != "notes.txt" ||
			resp.GetWorker().GetId() != pw.id.String() {
			t.Errorf("listing = %v", resp)
		}
		next(t, pw.prober.started, "started")
	})

	t.Run("the caller gives up", func(t *testing.T) {
		t.Parallel()
		pw := newProbeWorker(t, newController(t), passive, upToDate(nil))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { _, err := pw.list(ctx, waitPath); done <- err }()
		id := next(t, pw.prober.started, "started")
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("ListWorkerFiles = %v, want the caller's cancellation", err)
		}
		// The worker stops the probe: a cancel_probe (active) or the end of
		// the call (passive).
		if got := next(t, pw.prober.cancelled, "cancelled"); got != id {
			t.Errorf("cancelled probe %s, want %s", got, id)
		}
	})

	t.Run("no answer in time", func(t *testing.T) {
		t.Parallel()
		pw := newProbeWorker(t, newController(t), passive, upToDate(nil))
		start := time.Now()
		_, err := pw.list(t.Context(), waitPath)
		if !errors.Is(err, domain.ErrUnavailable) || !strings.Contains(err.Error(), "did not answer in time") {
			t.Fatalf("ListWorkerFiles = %v, want Unavailable", err)
		}
		if took := time.Since(start); took < probeTimeout || took > probeTimeout+probeGrace+5*time.Second {
			t.Errorf("gave up after %v, want the probe timeout plus the grace", took)
		}
		id := next(t, pw.prober.started, "started")
		if got := next(t, pw.prober.cancelled, "cancelled"); got != id {
			t.Errorf("cancelled probe %s, want %s", got, id)
		}
		// The worker still answers the next one.
		if _, err := pw.list(t.Context(), "/data"); err != nil {
			t.Errorf("the next listing: %v", err)
		}
	})

	t.Run("worker restart", func(t *testing.T) {
		t.Parallel()
		pw := newProbeWorker(t, newController(t), passive, upToDate(nil))
		done := make(chan error, 1)
		go func() { _, err := pw.list(t.Context(), waitPath); done <- err }()
		next(t, pw.prober.started, "started")
		// A second probe answered after the first started: in active mode
		// its run_probe came in the poll that acked the first one, so the
		// controller knows the process accepted it.
		if _, err := pw.list(t.Context(), "/data"); err != nil {
			t.Fatal(err)
		}
		pw.restart(t)
		// Active: the process that accepted the probe is gone. Passive: it
		// stopped the probe as it shut down.
		want := "restarted"
		if passive {
			want = "stopped on worker"
		}
		if err := <-done; !errors.Is(err, domain.ErrUnavailable) || !strings.Contains(err.Error(), want) {
			t.Fatalf("ListWorkerFiles across a worker restart = %v, want Unavailable (%s)", err, want)
		}
		pw.waitHello(t)
		if resp, err := pw.list(t.Context(), "/data"); err != nil || len(resp.GetEntries()) != 1 {
			t.Errorf("listing on the new process = %v, %v", resp, err)
		}
	})

	t.Run("outdated worker", func(t *testing.T) {
		t.Parallel()
		pw := newProbeWorker(t, newController(t), passive, workerOptions{})
		_, err := pw.list(t.Context(), "/data")
		if !errors.Is(err, domain.ErrFailedPrecondition) || !strings.Contains(err.Error(), "upgrade") {
			t.Errorf("ListWorkerFiles on a worker without probes = %v, want FailedPrecondition", err)
		}
		select {
		case id := <-pw.prober.started:
			t.Errorf("the outdated worker was sent probe %s", id)
		default:
		}
		// One that lacks only the newest feature still runs probes.
		pw = newProbeWorker(t, newController(t), passive, workerOptions{
			features: slices.DeleteFunc(link.Features(), func(f string) bool { return f == link.FeatureJobTimeLimit }),
			probeOps: link.ProbeOps(),
		})
		if _, err := pw.list(t.Context(), "/data"); err != nil {
			t.Errorf("ListWorkerFiles on a worker without time limits = %v", err)
		}
	})
}
