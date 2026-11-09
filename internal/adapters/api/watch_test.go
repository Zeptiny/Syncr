package api

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// watcher reads a WatchJob stream.
type watcher struct {
	t      *testing.T
	stream *connect.ServerStreamForClient[syncrv1.WatchJobResponse]
}

func (e *testEnv) watch(ctx context.Context, jobID string) *watcher {
	e.t.Helper()
	stream, err := e.jobsClient().WatchJob(ctx, connect.NewRequest(&syncrv1.WatchJobRequest{JobId: jobID}))
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = stream.Close() })
	return &watcher{t: e.t, stream: stream}
}

// next returns the next message, failing the test if the stream ends.
func (w *watcher) next() *syncrv1.WatchJobResponse {
	w.t.Helper()
	if !w.stream.Receive() {
		w.t.Fatalf("stream ended: %v", w.stream.Err())
	}
	return w.stream.Msg()
}

func (w *watcher) nextJob(want syncrv1.JobStatus) *syncrv1.Job {
	w.t.Helper()
	m := w.next()
	if m.GetJob() == nil || m.GetJob().GetStatus() != want {
		w.t.Fatalf("message = %v, want job in status %v", m, want)
	}
	return m.GetJob()
}

// end asserts that the stream ends and returns its error.
func (w *watcher) end() error {
	w.t.Helper()
	if w.stream.Receive() {
		w.t.Fatalf("unexpected message %v", w.stream.Msg())
	}
	return w.stream.Err()
}

func TestWatchJobLifecycle(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	j := e.runJob()
	id := e.jobID(j)
	w := e.watch(ctx, j.GetId())

	// The current job comes first.
	if first := w.nextJob(syncrv1.JobStatus_JOB_STATUS_QUEUED); first.GetId() != j.GetId() {
		t.Errorf("first = %v", first)
	}

	// The messages below are driven through the methods the dispatcher uses.
	if _, _, err := e.jsvc.MarkRunning(ctx, e.worker, id, testEpoch, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	w.nextJob(syncrv1.JobStatus_JOB_STATUS_DISPATCHED)
	w.nextJob(syncrv1.JobStatus_JOB_STATUS_RUNNING)

	if _, err := e.jsvc.RecordProgress(ctx, e.worker, id, testEpoch, jobs.Sample{Time: e.clock.Now(), Stats: jobs.Stats{Bytes: 42, TotalBytes: 100, Elapsed: time.Second}}); err != nil {
		t.Fatal(err)
	}
	p := w.next().GetProgress()
	if p.GetStats().GetBytes() != 42 || p.GetStats().GetTotalBytes() != 100 || p.GetStats().GetElapsed().AsDuration() != time.Second || !p.GetTime().AsTime().Equal(e.clock.Now()) {
		t.Errorf("progress = %v", p)
	}

	// A cancel request is a status-less change of the job: it is sent again.
	if _, err := e.jsvc.Cancel(ctx, testAdmin, id); err != nil {
		t.Fatal(err)
	}
	if got := w.nextJob(syncrv1.JobStatus_JOB_STATUS_RUNNING); !got.GetCancelRequested() || got.GetStats().GetBytes() != 42 {
		t.Errorf("job after the cancel request = %v", got)
	}

	if _, _, err := e.jsvc.Finish(ctx, e.worker, id, testEpoch, jobs.Outcome{
		Status: domain.JobCancelled, Stats: jobs.Stats{Bytes: 50}, ExitCode: -1,
	}); err != nil {
		t.Fatal(err)
	}
	final := w.nextJob(syncrv1.JobStatus_JOB_STATUS_CANCELLED)
	if final.GetStats().GetBytes() != 50 || final.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_CANCELLED {
		t.Errorf("final job = %v", final)
	}
	if err := w.end(); err != nil {
		t.Errorf("stream ended with %v", err)
	}
}

func TestWatchJobStartsWithRunningProgress(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	j := e.runJob()
	e.startRunning(j)
	if _, err := e.jsvc.RecordProgress(ctx, e.worker, e.jobID(j), testEpoch, jobs.Sample{Time: e.clock.Now(), Stats: jobs.Stats{Bytes: 5}}); err != nil {
		t.Fatal(err)
	}
	w := e.watch(ctx, j.GetId())
	if got := w.nextJob(syncrv1.JobStatus_JOB_STATUS_RUNNING); got.GetStats().GetBytes() != 5 {
		t.Errorf("first job = %v", got)
	}
	if got := w.next().GetProgress().GetStats().GetBytes(); got != 5 {
		t.Errorf("first progress = %d", got)
	}
}

func TestWatchFinishedJobEndsAtOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	j := e.runJob()
	if _, err := e.jsvc.Cancel(t.Context(), testAdmin, e.jobID(j)); err != nil {
		t.Fatal(err)
	}
	w := e.watch(t.Context(), j.GetId())
	w.nextJob(syncrv1.JobStatus_JOB_STATUS_CANCELLED)
	if err := w.end(); err != nil {
		t.Errorf("err = %v", err)
	}
	if n := e.broker.Subscribers(e.jobID(j)); n != 0 {
		t.Errorf("%d subscribers left", n)
	}
}

func TestWatchJobErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for id, want := range map[string]connect.Code{
		"":                         connect.CodeInvalidArgument,
		"junk":                     connect.CodeInvalidArgument,
		domain.NewJobID().String(): connect.CodeNotFound,
	} {
		w := e.watch(t.Context(), id)
		if w.stream.Receive() {
			t.Fatalf("WatchJob(%q) sent %v", id, w.stream.Msg())
		}
		if err := w.stream.Err(); err == nil || codeOf(t, err) != want {
			t.Errorf("WatchJob(%q) = %v, want %v", id, err, want)
		}
	}
}

// waitFor polls cond for up to five seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestWatchJobEndsWhenTheClientCancels(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	j := e.runJob()
	id := e.jobID(j)

	ctx, cancel := context.WithCancel(t.Context())
	w := e.watch(ctx, j.GetId())
	w.nextJob(syncrv1.JobStatus_JOB_STATUS_QUEUED)
	if n := e.broker.Subscribers(id); n != 1 {
		t.Fatalf("%d subscribers while watching", n)
	}

	cancel()
	if w.stream.Receive() {
		t.Fatal("message after cancel")
	}
	wantCode(t, w.stream.Err(), connect.CodeCanceled)
	// The server side lets go of the subscription.
	waitFor(t, "the watch to be released", func() bool { return e.broker.Subscribers(id) == 0 })

	// The job is unaffected and can be watched again.
	w2 := e.watch(t.Context(), j.GetId())
	w2.nextJob(syncrv1.JobStatus_JOB_STATUS_QUEUED)
}

func TestWatchJobSlowClientDoesNotBlockProducers(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	j := e.runJob()
	id := e.jobID(j)
	w := e.watch(ctx, j.GetId())
	w.nextJob(syncrv1.JobStatus_JOB_STATUS_QUEUED)
	e.startRunning(j)
	w.nextJob(syncrv1.JobStatus_JOB_STATUS_DISPATCHED)
	w.nextJob(syncrv1.JobStatus_JOB_STATUS_RUNNING)

	// The client stops reading while the worker reports a lot of progress.
	const samples = 2000
	done := make(chan error, 1)
	go func() {
		for i := range samples {
			if _, err := e.jsvc.RecordProgress(ctx, e.worker, id, testEpoch, jobs.Sample{
				Time: e.clock.Now().Add(time.Duration(i) * time.Millisecond), Stats: jobs.Stats{Bytes: int64(i + 1)},
			}); err != nil {
				done <- err
				return
			}
		}
		_, _, err := e.jsvc.Finish(ctx, e.worker, id, testEpoch, jobs.Outcome{Status: domain.JobSucceeded, Stats: jobs.Stats{Bytes: samples}})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("producers are blocked by a slow client")
	}

	// The client catches up: progress never goes backwards, may skip, and the
	// terminal status is not skipped.
	var last int64
	for {
		m := w.next()
		if p := m.GetProgress(); p != nil {
			if p.GetStats().GetBytes() < last {
				t.Fatalf("progress went back: %d after %d", p.GetStats().GetBytes(), last)
			}
			last = p.GetStats().GetBytes()
			continue
		}
		if m.GetJob().GetStatus() != syncrv1.JobStatus_JOB_STATUS_SUCCEEDED {
			t.Fatalf("message = %v", m)
		}
		break
	}
	if err := w.end(); err != nil {
		t.Error(err)
	}
}

func TestWatchJobEndsOnShutdown(t *testing.T) {
	t.Parallel()
	base, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	e := newEnv(t, func(d *Deps) { d.BaseContext = base })
	j := e.runJob()
	w := e.watch(t.Context(), j.GetId())
	w.nextJob(syncrv1.JobStatus_JOB_STATUS_QUEUED)

	shutdown()
	if w.stream.Receive() {
		t.Fatalf("message after shutdown: %v", w.stream.Msg())
	}
	wantCode(t, w.stream.Err(), connect.CodeUnavailable)
	waitFor(t, "the watch to be released", func() bool { return e.broker.Subscribers(e.jobID(j)) == 0 })
}

func TestWatchJobMaxDuration(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(d *Deps) { d.WatchMaxDuration = 50 * time.Millisecond })
	j := e.runJob()
	w := e.watch(t.Context(), j.GetId())
	w.nextJob(syncrv1.JobStatus_JOB_STATUS_QUEUED)
	if w.stream.Receive() {
		t.Fatalf("unexpected message %v", w.stream.Msg())
	}
	wantCode(t, w.stream.Err(), connect.CodeDeadlineExceeded)
	waitFor(t, "the watch to be released", func() bool { return e.broker.Subscribers(e.jobID(j)) == 0 })
}

func TestWatchJobKeepalive(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(d *Deps) { d.WatchKeepalive = 10 * time.Millisecond })
	j := e.runJob()
	w := e.watch(t.Context(), j.GetId())
	first := w.nextJob(syncrv1.JobStatus_JOB_STATUS_QUEUED)
	// An idle job keeps producing its snapshot.
	for range 3 {
		if got := w.nextJob(syncrv1.JobStatus_JOB_STATUS_QUEUED); got.GetId() != first.GetId() {
			t.Fatalf("keepalive = %v", got)
		}
	}
}

func TestWatchJobUnaryTimeoutDoesNotApply(t *testing.T) {
	t.Parallel()
	// Unary calls get a short deadline; the stream outlives it.
	e := newEnv(t, func(d *Deps) { d.RequestTimeout = 20 * time.Millisecond })
	j := e.runJob()
	w := e.watch(t.Context(), j.GetId())
	w.nextJob(syncrv1.JobStatus_JOB_STATUS_QUEUED)
	time.Sleep(100 * time.Millisecond)
	if _, _, err := e.jsvc.MarkRunning(t.Context(), e.worker, e.jobID(j), testEpoch, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	w.nextJob(syncrv1.JobStatus_JOB_STATUS_DISPATCHED)
}

func TestWatchJobOutlivesServerTimeouts(t *testing.T) {
	t.Parallel()
	// Server-wide read and write timeouts far below the stream's lifetime
	// must not end it: the handler extends its connection's deadlines.
	e := newEnvServer(t, func(srv *http.Server) {
		srv.ReadTimeout, srv.WriteTimeout = 100*time.Millisecond, 100*time.Millisecond
	}, func(d *Deps) { d.WatchKeepalive = 40 * time.Millisecond })
	j := e.runJob()
	w := e.watch(t.Context(), j.GetId())
	w.nextJob(syncrv1.JobStatus_JOB_STATUS_QUEUED)
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		w.nextJob(syncrv1.JobStatus_JOB_STATUS_QUEUED) // keepalives keep coming
	}
	if _, _, err := e.jsvc.MarkRunning(t.Context(), e.worker, e.jobID(j), testEpoch, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	for {
		if m := w.next(); m.GetJob().GetStatus() == syncrv1.JobStatus_JOB_STATUS_DISPATCHED {
			break
		}
	}
}

// createJob queues a job of owner directly through the jobs service.
func (e *testEnv) createJob(owner domain.Actor) jobs.Job {
	e.t.Helper()
	j, err := e.jsvc.Create(e.t.Context(), owner, jobs.CreateParams{
		WorkerID: e.worker, Kind: "rclone_transfer", Definition: []byte(`{"rclone_transfer":{"mode":"RCLONE_TRANSFER_MODE_COPY"}}`),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func TestWatchJobEndsWhenTheAPITokenIsRevoked(t *testing.T) {
	t.Parallel()
	mutate, token, usvc := withSessionAuthService(t)
	e := newEnv(t, mutate, func(d *Deps) { d.StreamReauthInterval = 10 * time.Millisecond })
	ctx := t.Context()
	j := e.createJob(testAdmin)
	stream, err := e.jobsAs("Bearer "+token).WatchJob(ctx, connect.NewRequest(&syncrv1.WatchJobRequest{JobId: j.ID.String()}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	w := &watcher{t: t, stream: stream}
	w.nextJob(syncrv1.JobStatus_JOB_STATUS_QUEUED)

	// Valid credentials survive many re-authentications.
	time.Sleep(60 * time.Millisecond)
	if _, _, err := e.jsvc.MarkRunning(ctx, e.worker, j.ID, testEpoch, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	w.nextJob(syncrv1.JobStatus_JOB_STATUS_DISPATCHED)
	w.nextJob(syncrv1.JobStatus_JOB_STATUS_RUNNING)

	toks, err := usvc.ListAPITokens(ctx, testAdmin)
	if err != nil || len(toks) != 1 {
		t.Fatalf("ListAPITokens = (%v, %v)", toks, err)
	}
	if err := usvc.DeleteAPIToken(ctx, testAdmin, toks[0].ID); err != nil {
		t.Fatal(err)
	}
	wantCode(t, w.end(), connect.CodeUnauthenticated)
	waitFor(t, "the watch to be released", func() bool { return e.broker.Subscribers(j.ID) == 0 })
}

// switchAuth authenticates every call as its current actor; none fails
// the call as unauthenticated.
type switchAuth struct{ actor atomic.Pointer[domain.Actor] }

func (a *switchAuth) Authenticate(ctx context.Context, _ http.Header, _ connect.Spec) (context.Context, error) {
	actor := a.actor.Load()
	if actor == nil {
		return ctx, fmt.Errorf("%w: revoked", domain.ErrUnauthenticated)
	}
	return WithCaller(ctx, Caller{Actor: *actor, Method: AuthSession}), nil
}

func TestWatchJobEndsWhenTheCallerLosesAccess(t *testing.T) {
	t.Parallel()
	demoted := domain.Actor{UserID: testAdmin.UserID, Role: domain.RoleUser}
	for name, tt := range map[string]struct {
		owner domain.Actor
		then  *domain.Actor
		want  connect.Code // 0: the stream goes on
	}{
		"signed out":                {testAdmin, nil, connect.CodeUnauthenticated},
		"admin demoted":             {testUser, &demoted, connect.CodePermissionDenied},
		"demoted, watching own job": {testAdmin, &demoted, 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			auth := &switchAuth{}
			admin := testAdmin
			auth.actor.Store(&admin)
			e := newEnv(t, func(d *Deps) { d.Auth, d.StreamReauthInterval = auth, 10*time.Millisecond })
			ctx := t.Context()
			j := e.createJob(tt.owner)
			w := e.watch(ctx, j.ID.String())
			w.nextJob(syncrv1.JobStatus_JOB_STATUS_QUEUED)

			auth.actor.Store(tt.then)
			if tt.want == 0 {
				time.Sleep(60 * time.Millisecond)
				if _, _, err := e.jsvc.MarkRunning(ctx, e.worker, j.ID, testEpoch, e.clock.Now()); err != nil {
					t.Fatal(err)
				}
				w.nextJob(syncrv1.JobStatus_JOB_STATUS_DISPATCHED)
				return
			}
			wantCode(t, w.end(), tt.want)
			waitFor(t, "the watch to be released", func() bool { return e.broker.Subscribers(j.ID) == 0 })
		})
	}
}
