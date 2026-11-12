package linkcore

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/agent"
	"github.com/Zeptiny/Syncr/internal/worker/outbox"
)

// simController is a minimal controller: a command queue redelivered until
// acked, and an ingest that is idempotent by seq and checks the protocol
// invariants the real controller relies on.
type simController struct {
	t *testing.T

	epoch   string
	pending []*syncrv1.Command // unacked commands, oldest first
	nextCmd int

	seen    map[int64]bool // ingested seqs
	highest int64          // highest ingested seq

	dispatched map[string]bool // StartJob acked by the worker
	started    map[string]int  // ingested JobStarted per job
	results    map[string][]*syncrv1.JobResult
}

func newSimController(t *testing.T) *simController {
	return &simController{
		t:          t,
		seen:       map[int64]bool{},
		dispatched: map[string]bool{},
		started:    map[string]int{},
		results:    map[string][]*syncrv1.JobResult{},
	}
}

func (c *simController) enqueue(cmd func(id string) *syncrv1.Command) {
	c.nextCmd++
	c.pending = append(c.pending, cmd(fmt.Sprintf("cmd-%d", c.nextCmd)))
}

// deliver returns the pending commands, oldest first (copies, as if
// serialized).
func (c *simController) deliver() []*syncrv1.Command {
	return slices.Clone(c.pending)
}

func (c *simController) receiveAcks(acks []*syncrv1.CommandAck) {
	for _, ack := range acks {
		i := slices.IndexFunc(c.pending, func(cmd *syncrv1.Command) bool { return cmd.GetId() == ack.GetCommandId() })
		if i < 0 {
			continue // duplicate ack
		}
		if sj := c.pending[i].GetStartJob(); sj != nil {
			c.dispatched[sj.GetJobId()] = true
		}
		c.pending = slices.Delete(c.pending, i, i+1)
	}
}

// ingest stores new events and checks reconcile safety against state. It
// returns the ack seq.
func (c *simController) ingest(epoch string, events []*syncrv1.Event, state *syncrv1.WorkerState) int64 {
	c.t.Helper()
	if c.epoch != "" && epoch != c.epoch {
		c.t.Fatalf("epoch changed from %q to %q", c.epoch, epoch)
	}
	c.epoch = epoch
	for _, e := range events {
		seq := e.GetSeq()
		if c.seen[seq] {
			continue
		}
		if seq <= c.highest {
			c.t.Fatalf("new seq %d at or below ingested %d", seq, c.highest)
		}
		c.seen[seq], c.highest = true, seq
		switch p := e.GetPayload().(type) {
		case *syncrv1.Event_Hello:
			if seq != 1 {
				c.t.Fatalf("hello at seq %d", seq)
			}
		case *syncrv1.Event_JobStarted:
			job := p.JobStarted.GetJobId()
			c.started[job]++
			if c.started[job] > 1 || len(c.results[job]) > 0 {
				c.t.Fatalf("JobStarted of %s duplicated or after its result", job)
			}
		case *syncrv1.Event_Progress:
			job := p.Progress.GetJobId()
			if c.started[job] != 1 || len(c.results[job]) > 0 {
				c.t.Fatalf("progress of %s outside start/result", job)
			}
		case *syncrv1.Event_JobResult:
			job := p.JobResult.GetJobId()
			c.results[job] = append(c.results[job], p.JobResult)
			if len(c.results[job]) > 1 {
				c.t.Fatalf("second JobResult for %s", job)
			}
		}
	}
	held := map[string]bool{}
	for _, j := range state.GetJobs() {
		held[j.GetJobId()] = true
	}
	// Reconcile: a dispatched job without a result must be held, or the
	// controller would wrongly mark it interrupted; a job with a result must
	// not be held, or the controller would send a pointless cancel.
	for job := range c.dispatched {
		if len(c.results[job]) == 0 && !held[job] {
			c.t.Fatalf("dispatched job %s is neither finished nor in the state %v", job, jobIDs(state))
		}
	}
	for job := range held {
		if len(c.results[job]) > 0 {
			c.t.Fatalf("finished job %s is still in the state", job)
		}
	}
	return c.highest
}

// done reports whether every job got its result and nothing is pending.
func (c *simController) done(jobs int) bool {
	return len(c.pending) == 0 && len(c.results) == jobs
}

type simMode int

const (
	simActive simMode = iota
	simPassive
)

// TestSimulatedControllerExactlyOnce drives a worker through a lossy link:
// requests and responses are dropped at random, commands are redelivered
// until acked, some jobs are cancelled and some StartJobs are sent again
// after they were acked. Every job must run at most once and produce
// exactly one result, and every state snapshot must be safe to reconcile
// with. Deterministic per seed; run with -count=50.
func TestSimulatedControllerExactlyOnce(t *testing.T) {
	t.Parallel()
	for _, mode := range []simMode{simActive, simPassive} {
		for seed := range uint64(6) {
			name := fmt.Sprintf("active/seed%d", seed)
			if mode == simPassive {
				name = fmt.Sprintf("passive/seed%d", seed)
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) { simulate(t, mode, seed) })
			})
		}
	}
}

func simulate(t *testing.T, mode simMode, seed uint64) {
	const (
		jobs     = 30
		dropRate = 0.3
		maxBatch = 5
	)
	rng := rand.New(rand.NewPCG(seed, 7))
	rn := newStepRunner(150 * time.Millisecond)
	for i := range jobs {
		rn.setSteps(fmt.Sprintf("job-%d", i), 1+rng.IntN(15))
	}
	core, a := newCore(t, rn, agent.Config{
		MaxConcurrentJobs: 3,
		ProgressInterval:  300 * time.Millisecond,
		Outbox:            outbox.Options{CoalesceAt: 8, HighWater: 16},
	})
	defer shutdown(t, a)
	ctrl := newSimController(t)
	drop := func() bool { return rng.Float64() < dropRate }
	cancelled := map[string]bool{}

	created, rounds, saturated := 0, 0, 0
	for round := 0; ; round++ {
		rounds = round
		if a.Outbox().Stats().Saturated {
			saturated++
		}
		if round > 20000 {
			t.Fatalf("no convergence: %d results, %d pending commands, outbox %+v", len(ctrl.results), len(ctrl.pending), a.Outbox().Stats())
		}
		// The controller creates jobs, cancels some and, rarely, sends a
		// StartJob again for a job it already dispatched.
		if created < jobs && rng.IntN(3) == 0 {
			job := fmt.Sprintf("job-%d", created)
			created++
			ctrl.enqueue(func(id string) *syncrv1.Command { return startCmd(id, job) })
		}
		if created > 0 && rng.IntN(6) == 0 {
			job := fmt.Sprintf("job-%d", rng.IntN(created))
			cancelled[job] = true
			ctrl.enqueue(func(id string) *syncrv1.Command { return cancelCmd(id, job) })
		}
		if created > 0 && rng.IntN(20) == 0 {
			job := fmt.Sprintf("job-%d", rng.IntN(created))
			if ctrl.dispatched[job] {
				ctrl.enqueue(func(id string) *syncrv1.Command { return startCmd(id, job) })
			}
		}

		switch mode {
		case simActive:
			poll := core.BuildPoll(0)
			if !drop() {
				ctrl.receiveAcks(poll.GetAcks())
				resp := &syncrv1.PollResponse{Commands: ctrl.deliver()}
				if !drop() {
					core.ApplyPollResponse(t.Context(), poll, resp)
				}
			}
			report := core.BuildReport(maxBatch)
			if !drop() {
				ctrl.receiveAcks(report.GetAcks())
				ack := ctrl.ingest(report.GetEpoch(), report.GetEvents(), report.GetState())
				if !drop() {
					if _, err := core.ApplyReportResponse(report, &syncrv1.ReportResponse{AckSeq: ack}); err != nil {
						t.Fatalf("ApplyReportResponse: %v", err)
					}
				}
			}
		case simPassive:
			if cmds := ctrl.deliver(); len(cmds) > 0 && !drop() {
				resp := core.HandleCommandRequest(t.Context(), &syncrv1.CommandRequest{Commands: cmds})
				if !drop() {
					ctrl.receiveAcks(resp.GetAcks())
				}
			}
			if !drop() {
				resp := core.HandlePull(&syncrv1.PullRequest{Epoch: ctrl.epoch, AfterSeq: ctrl.highest, MaxEvents: maxBatch})
				if !drop() {
					ctrl.ingest(resp.GetEpoch(), resp.GetEvents(), resp.GetState())
				}
			}
		}

		if created == jobs && ctrl.done(jobs) && core.PendingAcks() == 0 && core.Drained() {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	for i := range jobs {
		job := fmt.Sprintf("job-%d", i)
		results := ctrl.results[job]
		if len(results) != 1 {
			t.Errorf("%s: %d results", job, len(results))
			continue
		}
		r := results[0]
		if runs := rn.runCount(job); runs > 1 || (runs == 1) != (ctrl.started[job] == 1) {
			t.Errorf("%s: ran %d times, %d JobStarted", job, runs, ctrl.started[job])
		}
		switch r.GetStatus() {
		case syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED:
			if rn.runCount(job) != 1 {
				t.Errorf("%s succeeded without running", job)
			}
		case syncrv1.JobResultStatus_JOB_RESULT_STATUS_CANCELLED:
			if !cancelled[job] {
				t.Errorf("%s cancelled without a CancelJob", job)
			}
		default:
			t.Errorf("%s: unexpected result %v", job, r)
		}
	}
	st := a.Outbox().Stats()
	if st.Backlog != 0 {
		t.Errorf("outbox not drained: %+v", st)
	}
	nCancelled := 0
	for _, rs := range ctrl.results {
		if rs[0].GetStatus() == syncrv1.JobResultStatus_JOB_RESULT_STATUS_CANCELLED {
			nCancelled++
		}
	}
	t.Logf("rounds %d, events %d, coalesced %d, rounds saturated %d, cancelled results %d", rounds, st.Head, st.Coalesced, saturated, nCancelled)
}
