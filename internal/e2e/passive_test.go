package e2e

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	cpassive "github.com/Zeptiny/Syncr/internal/controller/dispatch/passive"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// enrollPassive enrolls a passive worker over HTTP with the worker's URL
// and certificate fingerprint.
func enrollPassive(t *testing.T, c *controller, url, fingerprint string) (domain.WorkerID, string) {
	t.Helper()
	resp, err := c.enroll(&syncrv1.EnrollRequest{
		EnrollmentToken: c.enrollToken(), Name: "server", Mode: syncrv1.WorkerMode_WORKER_MODE_PASSIVE,
		Url: url, CertFingerprint: fingerprint,
	})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	id, err := domain.ParseWorkerID(resp.GetWorkerId())
	if err != nil {
		t.Fatal(err)
	}
	return id, resp.GetWorkerSecret()
}

// TestPassiveJobSucceeds: the controller drives a passive worker through
// its pinned TLS endpoint: hello, job, cancel.
func TestPassiveJobSucceeds(t *testing.T) {
	t.Parallel()
	c := newController(t)
	rn := newFakeRunner()
	secret := pairingSecret()
	w := startPassive(t, rn, secret)
	_, ws := c.svc()
	reg, err := ws.RegisterPassive(context.Background(), workers.PassiveParams{
		Name: "server", URL: w.url, CertFingerprint: w.fingerprint, Secret: secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := reg.ID
	c.wakeSupervisor()

	got := c.waitWorker(id, "hello", func(w workers.Worker) bool { return w.Info.Hostname == "e2e" })
	if got.Link.Epoch != w.agent.Epoch() {
		t.Errorf("epoch %q, worker %q", got.Link.Epoch, w.agent.Epoch())
	}
	job := c.run(id, testSpec())
	j := c.waitStatus(job, domain.JobSucceeded)
	if j.Stats.Bytes != 100 || rn.runCount(job) != 1 {
		t.Errorf("job %+v runs %d", j, rn.runCount(job))
	}

	held := c.run(id, testSpec())
	rn.holdJob(held)
	c.waitStatus(held, domain.JobRunning)
	c.cancel(held)
	if j := c.waitStatus(held, domain.JobCancelled); j.Error.Code != jobs.CodeCancelled {
		t.Errorf("cancelled job error %+v", j.Error)
	}
}

// TestPassiveEnrollAndControllerRestart enrolls a passive worker through
// EnrollmentService, then restarts the controller mid-job.
func TestPassiveEnrollAndControllerRestart(t *testing.T) {
	t.Parallel()
	c := newController(t)
	rn := newFakeRunner()
	// As setup does: the certificate (and so the fingerprint) and the
	// address exist first; the secret comes back from enrollment.
	certFile, keyFile, fp := selfSigned(t)
	ln := listen(t)
	id, secret := enrollPassive(t, c, "https://"+ln.Addr().String(), fp)
	w := startPassiveOn(t, rn, certFile, keyFile, secret, ln)
	c.wakeSupervisor()

	job := c.run(id, testSpec())
	rn.holdJob(job)
	c.waitStatus(job, domain.JobRunning)
	c.restart()
	rn.release(job)
	if j := c.waitStatus(job, domain.JobSucceeded); rn.runCount(job) != 1 || j.Epoch != w.agent.Epoch() {
		t.Errorf("job %+v runs %d", j, rn.runCount(job))
	}
}

// TestPassiveWrongFingerprint: a worker whose certificate does not match
// the pin is never talked to.
func TestPassiveWrongFingerprint(t *testing.T) {
	t.Parallel()
	c := newController(t)
	rn := newFakeRunner()
	secret := pairingSecret()
	w := startPassive(t, rn, secret)
	_, _, otherFP := selfSigned(t)
	_, ws := c.svc()
	reg, err := ws.RegisterPassive(context.Background(), workers.PassiveParams{
		Name: "impostor", URL: w.url, CertFingerprint: otherFP, Secret: secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.wakeSupervisor()
	job := c.run(reg.ID, testSpec())

	// Direct call: the handshake fails with a clear error.
	target, err := ws.PassiveTarget(context.Background(), reg.ID)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := cpassive.NewClient(target, cpassive.ClientConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	_, err = cl.Pull(context.Background(), &syncrv1.PullRequest{})
	if !errors.Is(err, cpassive.ErrFingerprintMismatch) || connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("pull with a wrong pin: %v", err)
	}

	time.Sleep(300 * time.Millisecond) // several supervisor rounds
	if got := c.job(job); got.Status != domain.JobQueued {
		t.Errorf("job %s", got.Status)
	}
	if got := c.waitWorker(reg.ID, "", func(workers.Worker) bool { return true }); got.Link.Epoch != "" || !got.Link.LastSeen.IsZero() {
		t.Errorf("link %+v", got.Link)
	}
	if rn.runCount(job) != 0 || w.agent.Outbox().Stats().Acked != 0 {
		t.Error("the worker was reached")
	}
}

// TestPassiveWrongSecret: the worker refuses a controller with the wrong
// secret; nothing reaches the agent.
func TestPassiveWrongSecret(t *testing.T) {
	t.Parallel()
	c := newController(t)
	rn := newFakeRunner()
	w := startPassive(t, rn, pairingSecret())
	_, ws := c.svc()
	reg, err := ws.RegisterPassive(context.Background(), workers.PassiveParams{
		Name: "server", URL: w.url, CertFingerprint: w.fingerprint, Secret: pairingSecret(),
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := ws.PassiveTarget(context.Background(), reg.ID)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := cpassive.NewClient(target, cpassive.ClientConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	_, err = cl.Command(context.Background(), &syncrv1.CommandRequest{Commands: []*syncrv1.Command{{
		Id: "start:x", Command: &syncrv1.Command_StartJob{StartJob: &syncrv1.StartJob{JobId: "x", Spec: testSpec()}},
	}}})
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeUnauthenticated || ce.Message() != "invalid controller credentials" {
		t.Errorf("command with a wrong secret: %v", err)
	}
	if st := w.agent.State(); len(st.GetJobs()) != 0 || w.agent.Outbox().Stats().Head != 1 {
		t.Errorf("agent state %v", st)
	}
}

// TestPassiveShutdownDrain: a passive worker shutting down keeps serving
// Pulls until the controller has the CANCELLED result of its running job.
func TestPassiveShutdownDrain(t *testing.T) {
	t.Parallel()
	c := newController(t)
	rn := newFakeRunner()
	secret := pairingSecret()
	w := startPassive(t, rn, secret)
	_, ws := c.svc()
	reg, err := ws.RegisterPassive(context.Background(), workers.PassiveParams{
		Name: "server", URL: w.url, CertFingerprint: w.fingerprint, Secret: secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.wakeSupervisor()
	job := c.run(reg.ID, testSpec())
	rn.holdJob(job)
	c.waitStatus(job, domain.JobRunning)

	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	if err := w.agent.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.server.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := <-w.served; err != nil {
		t.Errorf("serve: %v", err)
	}
	j := c.job(job)
	if j.Status != domain.JobInterrupted || j.Error.Code != jobs.CodeWorkerShutdown {
		t.Errorf("job %s %+v", j.Status, j.Error)
	}
}
