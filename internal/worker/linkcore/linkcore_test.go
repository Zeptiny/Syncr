package linkcore

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/agent"
	"github.com/Zeptiny/Syncr/internal/worker/outbox"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// stepRunner is a fake runner: each job emits one sample per step and
// succeeds after its number of steps, or stops when cancelled. A job whose
// steps are negative blocks until cancelled.
type stepRunner struct {
	stepDelay time.Duration

	mu    sync.Mutex
	steps map[string]int
	runs  map[string]int
}

func newStepRunner(stepDelay time.Duration) *stepRunner {
	return &stepRunner{stepDelay: stepDelay, steps: map[string]int{}, runs: map[string]int{}}
}

func (*stepRunner) Kind() string { return "rclone_transfer" }

func (r *stepRunner) Run(ctx context.Context, job runner.Job, emit runner.EmitFunc) (runner.Result, error) {
	r.mu.Lock()
	r.runs[job.ID]++
	steps, ok := r.steps[job.ID]
	r.mu.Unlock()
	if !ok {
		steps = 1
	}
	var p runner.Progress
	for i := 0; steps < 0 || i < steps; i++ {
		select {
		case <-ctx.Done():
			return runner.Result{ExitCode: -1, Stats: p}, context.Cause(ctx)
		case <-time.After(r.stepDelay):
		}
		p.Bytes++
		emit(p)
	}
	return runner.Result{Stats: p}, nil
}

func (r *stepRunner) setSteps(job string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps[job] = n
}

func (r *stepRunner) runCount(job string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs[job]
}

func newCore(t *testing.T, rn runner.Runner, cfg agent.Config) (*Core, *agent.Agent) {
	t.Helper()
	reg := runner.NewRegistry()
	if err := reg.Register(rn); err != nil {
		t.Fatal(err)
	}
	cfg.Runners = reg
	a, err := agent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return New(a, a.Outbox(), nil), a
}

func shutdown(t *testing.T, a *agent.Agent) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if err := a.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

func startCmd(cmdID, jobID string) *syncrv1.Command {
	return &syncrv1.Command{Id: cmdID, Command: &syncrv1.Command_StartJob{StartJob: &syncrv1.StartJob{
		JobId: jobID,
		Spec:  &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{}}},
	}}}
}

func cancelCmd(cmdID, jobID string) *syncrv1.Command {
	return &syncrv1.Command{Id: cmdID, Command: &syncrv1.Command_CancelJob{CancelJob: &syncrv1.CancelJob{JobId: jobID}}}
}

func ackIDs(acks []*syncrv1.CommandAck) []string {
	ids := make([]string, 0, len(acks))
	for _, a := range acks {
		ids = append(ids, a.GetCommandId())
	}
	return ids
}

func seqs(evs []*syncrv1.Event) []int64 {
	out := make([]int64, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.GetSeq())
	}
	return out
}

func jobIDs(st *syncrv1.WorkerState) []string {
	out := make([]string, 0, len(st.GetJobs()))
	for _, j := range st.GetJobs() {
		out = append(out, j.GetJobId())
	}
	return out
}

func TestActiveReportAndPoll(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rn := newStepRunner(time.Second)
		rn.setSteps("a", 2)
		core, a := newCore(t, rn, agent.Config{Outbox: outbox.Options{Epoch: "ep"}})
		defer shutdown(t, a)

		// The first Report is the Hello (seq 1) and an empty state.
		req := core.BuildReport(0)
		if req.GetEpoch() != "ep" || !slices.Equal(seqs(req.GetEvents()), []int64{1}) || req.GetEvents()[0].GetHello() == nil ||
			len(req.GetAcks()) != 0 || req.GetState() == nil || len(req.GetState().GetJobs()) != 0 {
			t.Fatalf("first report = %v", req)
		}

		// A Poll delivers a StartJob; its ack goes out with the next requests.
		poll := core.BuildPoll(25 * time.Second)
		if poll.GetEpoch() != "ep" || poll.GetWait().AsDuration() != 25*time.Second || len(poll.GetAcks()) != 0 {
			t.Fatalf("poll = %v", poll)
		}
		acks := core.ApplyPollResponse(t.Context(), poll, &syncrv1.PollResponse{Commands: []*syncrv1.Command{startCmd("c1", "a")}})
		if !slices.Equal(ackIDs(acks), []string{"c1"}) {
			t.Fatalf("acks = %v", ackIDs(acks))
		}
		synctest.Wait()
		// A failed Poll (never applied) keeps the ack pending.
		lost := core.BuildPoll(0)
		if !slices.Equal(ackIDs(lost.GetAcks()), []string{"c1"}) {
			t.Fatalf("poll acks = %v", ackIDs(lost.GetAcks()))
		}
		report := core.BuildReport(0)
		if !slices.Equal(ackIDs(report.GetAcks()), []string{"c1"}) || !slices.Equal(jobIDs(report.GetState()), []string{"a"}) ||
			!slices.Equal(seqs(report.GetEvents()), []int64{1, 2}) {
			t.Fatalf("report = %v", report)
		}
		// The Report succeeds: acks delivered, events acked up to 2.
		more, err := core.ApplyReportResponse(report, &syncrv1.ReportResponse{AckSeq: 2})
		if err != nil || more {
			t.Fatalf("ApplyReportResponse = %v, %v", more, err)
		}
		if core.PendingAcks() != 0 || len(core.BuildPoll(0).GetAcks()) != 0 {
			t.Error("acks still pending after a successful report")
		}

		time.Sleep(5 * time.Second) // a: progress 1, progress 2, result
		r1 := core.BuildReport(2)
		if !slices.Equal(seqs(r1.GetEvents()), []int64{3, 4}) || !slices.Equal(jobIDs(r1.GetState()), []string{"a"}) {
			t.Fatalf("limited report: seqs %v state %v", seqs(r1.GetEvents()), jobIDs(r1.GetState()))
		}
		r2 := core.BuildReport(0)
		if !slices.Equal(seqs(r2.GetEvents()), []int64{3, 4, 5}) || len(r2.GetState().GetJobs()) != 0 {
			t.Fatalf("full report: seqs %v state %v", seqs(r2.GetEvents()), jobIDs(r2.GetState()))
		}
		// Responses arrive out of order and duplicated.
		if more, err := core.ApplyReportResponse(r2, &syncrv1.ReportResponse{AckSeq: 5}); err != nil || more {
			t.Errorf("r2 response = %v, %v", more, err)
		}
		if more, err := core.ApplyReportResponse(r1, &syncrv1.ReportResponse{AckSeq: 4}); err != nil || more {
			t.Errorf("stale r1 response = %v, %v", more, err)
		}
		if more, err := core.ApplyReportResponse(r2, &syncrv1.ReportResponse{AckSeq: 5}); err != nil || more {
			t.Errorf("duplicate r2 response = %v, %v", more, err)
		}
		if !core.Drained() {
			t.Error("not drained")
		}
		// An ack beyond the head is reported and ignored.
		handled := core.ApplyPollResponse(t.Context(), core.BuildPoll(0), &syncrv1.PollResponse{Commands: []*syncrv1.Command{startCmd("c2", "b")}})
		if len(handled) != 1 {
			t.Fatal("c2 not acked")
		}
		r3 := core.BuildReport(0)
		if more, err := core.ApplyReportResponse(r3, &syncrv1.ReportResponse{AckSeq: 99}); !errors.Is(err, outbox.ErrAckAhead) || !more {
			t.Errorf("ack ahead = %v, %v", more, err)
		}
		if core.PendingAcks() != 0 {
			t.Error("acks of a successful report must be delivered even when its event ack is implausible")
		}
		if got := seqs(core.BuildReport(0).GetEvents()); len(got) == 0 || got[0] != 6 {
			t.Errorf("events after a refused ack = %v", got)
		}
		// A response for another epoch is ignored.
		if _, err := core.ApplyReportResponse(&syncrv1.ReportRequest{Epoch: "old"}, &syncrv1.ReportResponse{AckSeq: 6}); err == nil {
			t.Error("foreign epoch accepted")
		}
	})
}

func TestPollAckBookkeeping(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rn := newStepRunner(time.Second)
		core, a := newCore(t, rn, agent.Config{})
		defer shutdown(t, a)

		p1 := core.BuildPoll(0)
		core.ApplyPollResponse(t.Context(), p1, &syncrv1.PollResponse{Commands: []*syncrv1.Command{startCmd("c1", "a"), startCmd("c2", "b")}})
		// The controller redelivers c1 (it lost the ack) along with c3.
		p2 := core.BuildPoll(0)
		if !slices.Equal(ackIDs(p2.GetAcks()), []string{"c1", "c2"}) {
			t.Fatalf("p2 acks = %v", ackIDs(p2.GetAcks()))
		}
		acks := core.ApplyPollResponse(t.Context(), p2, &syncrv1.PollResponse{Commands: []*syncrv1.Command{startCmd("c1", "a"), cancelCmd("c3", "b")}})
		if !slices.Equal(ackIDs(acks), []string{"c1", "c3"}) {
			t.Fatalf("acks = %v", ackIDs(acks))
		}
		// p2 delivered c1 and c2; c1 was acked again and c3 is new.
		if got := ackIDs(core.BuildPoll(0).GetAcks()); !slices.Equal(got, []string{"c1", "c3"}) {
			t.Errorf("pending acks = %v", got)
		}
		// An empty response still marks the request's acks delivered.
		core.ApplyPollResponse(t.Context(), core.BuildPoll(0), &syncrv1.PollResponse{})
		if core.PendingAcks() != 0 {
			t.Errorf("pending acks = %d", core.PendingAcks())
		}
		synctest.Wait()
		if rn.runCount("a") != 1 || rn.runCount("b") > 1 {
			t.Errorf("runs a=%d b=%d", rn.runCount("a"), rn.runCount("b"))
		}
	})
}

func TestPassiveCommandAndPull(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rn := newStepRunner(time.Second)
		rn.setSteps("a", 3)
		core, a := newCore(t, rn, agent.Config{Outbox: outbox.Options{Epoch: "ep"}})
		defer shutdown(t, a)

		resp := core.HandleCommandRequest(t.Context(), &syncrv1.CommandRequest{Commands: []*syncrv1.Command{startCmd("c1", "a")}})
		if resp.GetEpoch() != "ep" || !slices.Equal(ackIDs(resp.GetAcks()), []string{"c1"}) {
			t.Fatalf("command response = %v", resp)
		}
		// The response was lost: the controller redelivers, the job runs once.
		resp = core.HandleCommandRequest(t.Context(), &syncrv1.CommandRequest{Commands: []*syncrv1.Command{startCmd("c1", "a")}})
		if !slices.Equal(ackIDs(resp.GetAcks()), []string{"c1"}) {
			t.Fatalf("redelivered command response = %v", resp)
		}
		time.Sleep(10 * time.Second) // hello, start, 3 progress, result = seqs 1..6

		pull := func(epoch string, after int64, maxEvents int32) *syncrv1.PullResponse {
			t.Helper()
			r := core.HandlePull(&syncrv1.PullRequest{Epoch: epoch, AfterSeq: after, MaxEvents: maxEvents})
			if r.GetEpoch() != "ep" || r.GetState() == nil {
				t.Fatalf("pull response without epoch or state: %v", r)
			}
			return r
		}
		// Unknown epoch: after_seq is ignored and nothing is acked.
		r := pull("", 4, 2)
		if !slices.Equal(seqs(r.GetEvents()), []int64{1, 2}) || !r.GetHasMore() || !slices.Equal(jobIDs(r.GetState()), []string{"a"}) {
			t.Fatalf("pull(foreign) = seqs %v more %v state %v", seqs(r.GetEvents()), r.GetHasMore(), jobIDs(r.GetState()))
		}
		r = pull("other", 4, 0)
		if !slices.Equal(seqs(r.GetEvents()), []int64{1, 2, 3, 4, 5, 6}) || r.GetHasMore() || len(r.GetState().GetJobs()) != 0 {
			t.Fatalf("pull(other) = seqs %v more %v", seqs(r.GetEvents()), r.GetHasMore())
		}
		// after_seq acks.
		r = pull("ep", 2, 2)
		if !slices.Equal(seqs(r.GetEvents()), []int64{3, 4}) || !r.GetHasMore() || !slices.Equal(jobIDs(r.GetState()), []string{"a"}) {
			t.Fatalf("pull(2) = seqs %v more %v", seqs(r.GetEvents()), r.GetHasMore())
		}
		if st := a.Outbox().Stats(); st.Acked != 2 || st.Backlog != 4 {
			t.Errorf("after pull(2): %+v", st)
		}
		// A duplicated, older pull: nothing is un-acked; events start at the
		// oldest unacked one.
		r = pull("ep", 1, 0)
		if !slices.Equal(seqs(r.GetEvents()), []int64{3, 4, 5, 6}) {
			t.Errorf("pull(1) after pull(2) = %v", seqs(r.GetEvents()))
		}
		// after_seq beyond the head is not trusted.
		r = pull("ep", 50, 0)
		if !slices.Equal(seqs(r.GetEvents()), []int64{3, 4, 5, 6}) || a.Outbox().Stats().Acked != 2 {
			t.Errorf("pull(50) = %v, acked %d", seqs(r.GetEvents()), a.Outbox().Stats().Acked)
		}
		r = pull("ep", 6, 0)
		if len(r.GetEvents()) != 0 || r.GetHasMore() || !core.Drained() {
			t.Errorf("pull(6) = %v", seqs(r.GetEvents()))
		}
		// Command after the job finished: acked, not run again.
		resp = core.HandleCommandRequest(t.Context(), &syncrv1.CommandRequest{Commands: []*syncrv1.Command{startCmd("c1", "a"), cancelCmd("c2", "a")}})
		if !slices.Equal(ackIDs(resp.GetAcks()), []string{"c1", "c2"}) || rn.runCount("a") != 1 {
			t.Errorf("late commands: acks %v, runs %d", ackIDs(resp.GetAcks()), rn.runCount("a"))
		}
		if !core.Drained() {
			t.Error("late commands produced events")
		}
	})
}

func TestPullLimits(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ in, want int }{{0, DefaultMaxEvents}, {-3, DefaultMaxEvents}, {10, 10}, {MaxEvents + 1, MaxEvents}} {
		if got := clampMax(tt.in); got != tt.want {
			t.Errorf("clampMax(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestFinalFlushAfterShutdown(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rn := newStepRunner(time.Second)
		rn.setSteps("a", -1)
		core, a := newCore(t, rn, agent.Config{})
		core.HandleCommandRequest(t.Context(), &syncrv1.CommandRequest{Commands: []*syncrv1.Command{startCmd("c1", "a")}})
		time.Sleep(3 * time.Second)
		shutdown(t, a)

		drained := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			drained <- core.WaitDrained(ctx)
		}()
		r := core.HandlePull(&syncrv1.PullRequest{})
		last := r.GetEvents()[len(r.GetEvents())-1]
		if last.GetJobResult().GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_CANCELLED {
			t.Fatalf("last event = %v", last)
		}
		core.HandlePull(&syncrv1.PullRequest{Epoch: r.GetEpoch(), AfterSeq: last.GetSeq()})
		if err := <-drained; err != nil {
			t.Errorf("WaitDrained = %v", err)
		}

		// With nobody pulling, the wait ends at its deadline.
		core2, a2 := newCore(t, rn, agent.Config{})
		shutdown(t, a2)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := core2.WaitDrained(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("WaitDrained without a consumer = %v", err)
		}
	})
}

func TestNotify(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		core, a := newCore(t, newStepRunner(time.Second), agent.Config{})
		defer shutdown(t, a)
		select {
		case <-core.Notify():
			t.Fatal("signal before any event after the hello")
		default:
		}
		core.HandleCommandRequest(t.Context(), &syncrv1.CommandRequest{Commands: []*syncrv1.Command{startCmd("c1", "a")}})
		select {
		case <-core.Notify():
		default:
			t.Fatal("no signal after JobStarted")
		}
	})
}
