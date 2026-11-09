package e2e

import (
	"context"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
	wactive "github.com/Zeptiny/Syncr/internal/worker/transport/active"
)

// TestActiveJobSucceeds: enroll over HTTP, connect, run a job to success.
func TestActiveJobSucceeds(t *testing.T) {
	t.Parallel()
	c := newController(t)
	id, secret := c.enrollActive("desktop")
	rn := newFakeRunner()
	startActive(t, rn, activeOptions(c, id, secret))

	w := c.waitWorker(id, "hello", func(w workers.Worker) bool { return w.Info.Hostname == "e2e" })
	if w.Info.Version != "test" || len(w.Info.JobKinds) != 1 || w.Link.Epoch == "" {
		t.Errorf("worker info %+v link %+v", w.Info, w.Link)
	}
	job := c.run(id, testDefinition())
	j := c.waitStatus(job, domain.JobSucceeded)
	if j.Stats.Bytes != 100 || j.Stats.Files != 2 || j.Epoch != w.Link.Epoch || rn.runCount(job) != 1 {
		t.Errorf("job %+v, runs %d", j, rn.runCount(job))
	}
	js, _ := c.svc()
	if samples, err := js.Samples(context.Background(), testActor(), job); err != nil || len(samples) == 0 {
		t.Errorf("samples = %v, %v", samples, err)
	}

	// The worker got the spec built from the definition, with the remote's
	// credentials; the controller stored and logged none of them.
	spec := rn.spec(job)
	tr := spec.GetRcloneTransfer()
	if len(spec.GetRemotes()) != 1 || spec.GetRemotes()[0].GetAlias() != "r1" || spec.GetRemotes()[0].GetSecrets()["secret_access_key"] != testSecret ||
		tr.GetSource().GetPath() != "/data" || tr.GetDestination().GetRemote() != "r1" || tr.GetDestination().GetPath() != "bucket/backup" {
		t.Errorf("worker spec = %v", spec)
	}
	for what, text := range map[string]string{"logs": c.logs.String(), "definition": string(j.Definition), "job": fmt.Sprintf("%+v", j)} {
		if strings.Contains(text, testSecret) || strings.Contains(text, "AKIA-E2E") {
			t.Errorf("the remote's credentials are in the controller's %s", what)
		}
	}
}

// TestActiveRemoteDeletedBeforeDispatch: a queued job whose remote is
// deleted before its worker connects fails with remote_unavailable and is
// never sent.
func TestActiveRemoteDeletedBeforeDispatch(t *testing.T) {
	t.Parallel()
	c := newController(t)
	id, secret := c.enrollActive("desktop")
	job := c.run(id, testDefinition())
	c.remotes.remove(s3Remote())
	rn := newFakeRunner()
	startActive(t, rn, activeOptions(c, id, secret))
	j := c.waitStatus(job, domain.JobFailed)
	if j.Error.Code != jobs.CodeRemoteUnavailable || rn.runCount(job) != 0 {
		t.Errorf("job error %+v, runs %d", j.Error, rn.runCount(job))
	}
}

// TestActiveCancelMidJob cancels a running job from the controller.
func TestActiveCancelMidJob(t *testing.T) {
	t.Parallel()
	c := newController(t)
	id, secret := c.enrollActive("desktop")
	rn := newFakeRunner()
	startActive(t, rn, activeOptions(c, id, secret))

	job := c.run(id, heldDefinition())
	c.waitStatus(job, domain.JobRunning)
	c.cancel(job)
	j := c.waitStatus(job, domain.JobCancelled)
	if j.Error.Code != jobs.CodeCancelled || j.Error.Message != "job cancelled by the controller" || rn.runCount(job) != 1 {
		t.Errorf("job error %+v, runs %d", j.Error, rn.runCount(job))
	}
}

// TestActiveWorkerRestart: the worker process dies mid-job; its successor
// (a new epoch) makes the controller interrupt the job, and a request of
// the dead process is rejected as stale.
func TestActiveWorkerRestart(t *testing.T) {
	t.Parallel()
	c := newController(t)
	id, secret := c.enrollActive("desktop")
	rn := newFakeRunner()
	first := startActive(t, rn, activeOptions(c, id, secret))

	job := c.run(id, heldDefinition())
	c.waitStatus(job, domain.JobRunning)
	oldEpoch := first.agent.Epoch()
	first.kill()

	second := startActive(t, newFakeRunner(), activeOptions(c, id, secret))
	j := c.waitStatus(job, domain.JobInterrupted)
	if j.Error.Code != jobs.CodeWorkerRestarted {
		t.Errorf("job error %+v", j.Error)
	}
	c.waitWorker(id, "new epoch", func(w workers.Worker) bool { return w.Link.Epoch == second.agent.Epoch() })

	// A delayed request of the dead process.
	cl := syncrv1connect.NewControllerLinkServiceClient(c.srv.Client(), c.srv.URL)
	req := connect.NewRequest(&syncrv1.ReportRequest{Epoch: oldEpoch})
	req.Header().Set("Authorization", link.ActiveAuthorization(id.String(), secret))
	_, err := cl.Report(context.Background(), req)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("stale report: %v", err)
	}

	// The new process runs new jobs.
	next := c.run(id, testDefinition())
	c.waitStatus(next, domain.JobSucceeded)
}

// TestActiveControllerRestart: the controller restarts mid-job; buffered
// events are delivered to the new process and the job succeeds.
func TestActiveControllerRestart(t *testing.T) {
	t.Parallel()
	c := newController(t)
	id, secret := c.enrollActive("desktop")
	rn := newFakeRunner()
	startActive(t, rn, activeOptions(c, id, secret))

	job := c.run(id, heldDefinition())
	c.waitStatus(job, domain.JobRunning)
	c.restart()
	rn.release(job)
	j := c.waitStatus(job, domain.JobSucceeded)
	if j.Stats.Bytes != 100 || rn.runCount(job) != 1 {
		t.Errorf("job %+v, runs %d", j, rn.runCount(job))
	}
	// A job queued after the restart reaches the worker through the new
	// controller's long polls.
	next := c.run(id, testDefinition())
	c.waitStatus(next, domain.JobSucceeded)
}

// TestActiveFlakyNetwork loses requests and responses at random (seeded)
// in both directions of both loops; every job still runs exactly once and
// succeeds exactly once.
func TestActiveFlakyNetwork(t *testing.T) {
	t.Parallel()
	for _, seed := range []uint64{1, 2, 3} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			c := newController(t)
			id, secret := c.enrollActive("flaky")
			rn := newFakeRunner()
			flaky := &flakyTransport{dropReq: 0.3, dropRes: 0.3, rng: mrand.New(mrand.NewPCG(seed, 0x5eed))}
			opts := activeOptions(c, id, secret)
			opts.RootCAs = nil
			opts.HTTPClient = c.httpClient(func(next http.RoundTripper) http.RoundTripper {
				flaky.next = next
				return flaky
			})
			startActive(t, rn, opts)

			// Waves of jobs, so that many calls cross the network.
			var ids []domain.JobID
			for range 4 {
				wave := []domain.JobID{c.run(id, heldDefinition()), c.run(id, testDefinition()), c.run(id, testDefinition())}
				c.waitStatus(wave[0], domain.JobRunning)
				c.cancel(wave[0])
				for _, job := range wave[1:] {
					c.waitStatus(job, domain.JobSucceeded)
				}
				c.waitStatus(wave[0], domain.JobCancelled)
				ids = append(ids, wave...)
			}
			for i, job := range ids {
				j := c.job(job)
				if j.Requeues != 0 || (i%3 != 0 && j.Stats.Bytes != 100) {
					t.Errorf("job %s: %s, requeues %d, stats %+v", job, j.Status, j.Requeues, j.Stats)
				}
				if runs := rn.runCount(job); runs != 1 {
					t.Errorf("job %s ran %d times", job, runs)
				}
			}
			if flaky.lost.Load() == 0 {
				t.Error("the network lost nothing")
			}
			t.Logf("seed %d: %d calls lost", seed, flaky.lost.Load())
		})
	}
}

// TestActiveBadCredentials: wrong secrets and unknown workers get one
// fixed answer that reveals nothing.
func TestActiveBadCredentials(t *testing.T) {
	t.Parallel()
	c := newController(t)
	id, secret := c.enrollActive("desktop")
	cl := syncrv1connect.NewControllerLinkServiceClient(c.srv.Client(), c.srv.URL)
	for name, auth := range map[string]string{
		"wrong secret":   link.ActiveAuthorization(id.String(), secret+"x"),
		"unknown worker": link.ActiveAuthorization(domain.NewWorkerID().String(), secret),
		"malformed id":   link.ActiveAuthorization("nope", secret),
		"missing":        "",
		"passive scheme": link.PassiveAuthorization(secret),
	} {
		req := connect.NewRequest(&syncrv1.ReportRequest{Epoch: "e1"})
		if auth != "" {
			req.Header().Set("Authorization", auth)
		}
		_, err := cl.Report(context.Background(), req)
		var ce *connect.Error
		if !errors.As(err, &ce) || ce.Code() != connect.CodeUnauthenticated || ce.Message() != "invalid worker credentials" || len(ce.Details()) != 0 {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Nothing was recorded for the worker.
	if w := c.waitWorker(id, "no contact", func(workers.Worker) bool { return true }); w.Link.Epoch != "" || !w.Link.LastSeen.IsZero() {
		t.Errorf("link after rejected calls: %+v", w.Link)
	}

	// A transport with a wrong secret keeps backing off without reaching
	// the controller's state.
	opts := activeOptions(c, id, secret+"x")
	startActive(t, newFakeRunner(), opts)
	time.Sleep(300 * time.Millisecond)
	if w := c.waitWorker(id, "no contact", func(workers.Worker) bool { return true }); w.Link.Epoch != "" {
		t.Errorf("link after a worker with a wrong secret: %+v", w.Link)
	}
}

// TestEnrollmentTokens: used and expired tokens are rejected alike; an
// invalid request does not burn the token.
func TestEnrollmentTokens(t *testing.T) {
	t.Parallel()
	c := newController(t)
	isBadToken := func(err error) bool {
		var ce *connect.Error
		return errors.As(err, &ce) && ce.Code() == connect.CodeUnauthenticated && ce.Message() == "invalid, expired or used enrollment token"
	}

	token := c.enrollToken()
	// Passive without URL: invalid, the token survives.
	if _, err := c.enroll(&syncrv1.EnrollRequest{EnrollmentToken: token, Mode: syncrv1.WorkerMode_WORKER_MODE_PASSIVE}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid enroll: %v", err)
	}
	if _, err := c.enroll(&syncrv1.EnrollRequest{EnrollmentToken: token}); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if _, err := c.enroll(&syncrv1.EnrollRequest{EnrollmentToken: token}); !isBadToken(err) {
		t.Errorf("reused token: %v", err)
	}

	expiring := c.enrollToken()
	c.clock.Advance(2 * time.Hour)
	if _, err := c.enroll(&syncrv1.EnrollRequest{EnrollmentToken: expiring}); !isBadToken(err) {
		t.Errorf("expired token: %v", err)
	}
	if _, err := c.enroll(&syncrv1.EnrollRequest{EnrollmentToken: "syncr_et_made-up"}); !isBadToken(err) {
		t.Errorf("unknown token: %v", err)
	}
	if _, err := c.enroll(&syncrv1.EnrollRequest{EnrollmentToken: token, Name: strings.Repeat("n", 100<<10)}); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("oversized enroll: %v", err)
	}
}

// TestLongPollWakeup: a job created while the worker's Poll waits reaches
// the worker promptly.
func TestLongPollWakeup(t *testing.T) {
	t.Parallel()
	c := newController(t)
	id, secret := c.enrollActive("desktop")
	rn := newFakeRunner()
	opts := activeOptions(c, id, secret)
	opts.PollWait = 20 * time.Second
	opts.Heartbeat = 20 * time.Second
	startActive(t, rn, opts)
	c.waitWorker(id, "hello", func(w workers.Worker) bool { return w.Info.Hostname == "e2e" })
	time.Sleep(100 * time.Millisecond) // the Poll is waiting now

	for range 3 {
		start := time.Now()
		job := c.run(id, testDefinition())
		rn.waitStarted(t, job)
		if took := time.Since(start); took > time.Second {
			t.Errorf("job reached the worker after %v", took)
		}
		c.waitStatus(job, domain.JobSucceeded)
	}
}

// TestActiveShutdownDrain: a graceful worker shutdown delivers the
// CANCELLED result of a running job before the process exits.
func TestActiveShutdownDrain(t *testing.T) {
	t.Parallel()
	c := newController(t)
	id, secret := c.enrollActive("desktop")
	rn := newFakeRunner()
	w := startActive(t, rn, activeOptions(c, id, secret))

	job := c.run(id, heldDefinition())
	c.waitStatus(job, domain.JobRunning)
	if err := w.shutdown(t); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !w.core.Drained() {
		t.Error("outbox not drained")
	}
	// Delivered before the process exits, not reconstructed later.
	j := c.job(job)
	if j.Status != domain.JobInterrupted || j.Error.Code != jobs.CodeWorkerShutdown {
		t.Errorf("job %s %+v", j.Status, j.Error)
	}
}

// TestActiveLostWorkerComesBack: a worker cut off from the controller for
// longer than LostAfter (its process keeps running the job) has its job
// ended by the controller. When the link comes back with the same
// process, the worker is told to stop the job, its late events are
// acknowledged without changing the job, and its outbox drains.
func TestActiveLostWorkerComesBack(t *testing.T) {
	t.Parallel()
	c := newController(t)
	id, secret := c.enrollActive("laptop")
	rn := newFakeRunner()
	w := startActive(t, rn, activeOptions(c, id, secret))
	job := c.run(id, heldDefinition())
	c.waitStatus(job, domain.JobRunning)
	w.kill() // the link is cut; the agent keeps running the job

	c.clock.Advance(2 * time.Hour)
	c.mu.Lock()
	disp := c.disp
	c.mu.Unlock()
	if n, err := disp.ExpireLost(context.Background()); err != nil || n != 1 {
		t.Fatalf("ExpireLost = (%d, %v), want 1", n, err)
	}
	ended := c.job(job)
	if ended.Status != domain.JobInterrupted || ended.Error.Code != jobs.CodeWorkerLost {
		t.Fatalf("job %s %+v", ended.Status, ended.Error)
	}

	tr, err := wactive.New(w.core, activeOptions(c, id, secret))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = tr.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	deadline := time.Now().Add(waitTimeout)
	for len(w.agent.State().GetJobs()) > 0 || !w.core.Drained() {
		if time.Now().After(deadline) {
			t.Fatalf("the worker still holds %v, backlog %d", w.agent.State().GetJobs(), w.agent.Outbox().Stats().Backlog)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if j := c.job(job); j.Status != domain.JobInterrupted || j.Version != ended.Version {
		t.Errorf("job after the worker came back: %s v%d, was %s v%d", j.Status, j.Version, ended.Status, ended.Version)
	}
	if rn.runCount(job) != 1 {
		t.Errorf("runs = %d", rn.runCount(job))
	}
}

// TestActiveShutdownRequeuesWaitingJobs: the jobs waiting in the local
// queue of a worker that stops gracefully never ran, so they go back to
// the controller's queue (not interrupted) and the worker's next process
// runs them.
func TestActiveShutdownRequeuesWaitingJobs(t *testing.T) {
	t.Parallel()
	c := newController(t)
	id, secret := c.enrollActive("desktop")
	rn := newFakeRunner()
	w := startActive(t, rn, activeOptions(c, id, secret))

	var running, waiting []domain.JobID
	for range 4 { // the agent's MaxConcurrentJobs
		running = append(running, c.run(id, heldDefinition()))
	}
	for range 2 {
		waiting = append(waiting, c.run(id, testDefinition()))
	}
	for _, job := range running {
		c.waitStatus(job, domain.JobRunning)
	}
	for _, job := range waiting {
		c.waitStatus(job, domain.JobDispatched)
	}
	if err := w.shutdown(t); err != nil {
		t.Fatalf("drain: %v", err)
	}
	for _, job := range running {
		if j := c.job(job); j.Status != domain.JobInterrupted || j.Error.Code != jobs.CodeWorkerShutdown {
			t.Errorf("running job %s %+v", j.Status, j.Error)
		}
	}
	for _, job := range waiting {
		if j := c.job(job); j.Status != domain.JobQueued || j.Requeues != 1 || rn.runCount(job) != 0 {
			t.Errorf("waiting job %s requeues %d runs %d, want queued again", j.Status, j.Requeues, rn.runCount(job))
		}
	}

	startActive(t, newFakeRunner(), activeOptions(c, id, secret))
	for _, job := range waiting {
		c.waitStatus(job, domain.JobSucceeded)
	}
}
