package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// script is the behavior of a fake run.
type script func(ctx context.Context, job runner.Job, emit runner.EmitFunc) (runner.Result, error)

// fakeRunner runs scripts instead of tools and counts runs per job.
type fakeRunner struct {
	kind string

	mu      sync.Mutex
	runs    map[string]int
	scripts map[string]script
	def     script
}

func newFakeRunner(def script) *fakeRunner {
	return &fakeRunner{kind: "rclone_transfer", runs: map[string]int{}, scripts: map[string]script{}, def: def}
}

func (f *fakeRunner) Kind() string { return f.kind }

func (f *fakeRunner) Run(ctx context.Context, job runner.Job, emit runner.EmitFunc) (runner.Result, error) {
	f.mu.Lock()
	f.runs[job.ID]++
	s, ok := f.scripts[job.ID]
	if !ok {
		s = f.def
	}
	f.mu.Unlock()
	return s(ctx, job, emit)
}

func (f *fakeRunner) set(job string, s script) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts[job] = s
}

func (f *fakeRunner) runCount(job string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[job]
}

// succeed returns a successful result at once.
func succeed(_ context.Context, _ runner.Job, emit runner.EmitFunc) (runner.Result, error) {
	emit(runner.Progress{Bytes: 1})
	return runner.Result{ExitCode: 0, Stats: runner.Progress{Bytes: 1}}, nil
}

// canceled is what a proc-based runner returns after its context is done.
func canceled(ctx context.Context) error {
	return &proc.CanceledError{Path: "/bin/tool", ExitCode: -1, Cause: context.Cause(ctx)}
}

// gate returns a script that emits one sample, then blocks until release is
// closed (success) or ctx is done (cancelled, after stopDelay of "grace").
func gate(release <-chan struct{}, stopDelay time.Duration) script {
	return func(ctx context.Context, _ runner.Job, emit runner.EmitFunc) (runner.Result, error) {
		emit(runner.Progress{Bytes: 10})
		select {
		case <-release:
			return runner.Result{ExitCode: 0, Stats: runner.Progress{Bytes: 20}}, nil
		case <-ctx.Done():
			time.Sleep(stopDelay)
			return runner.Result{ExitCode: -1, Stats: runner.Progress{Bytes: 10}}, canceled(ctx)
		}
	}
}

func transferSpec() *syncrv1.JobSpec {
	return &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{}}}
}

func startCmd(cmdID, jobID string) *syncrv1.Command {
	return &syncrv1.Command{Id: cmdID, Command: &syncrv1.Command_StartJob{StartJob: &syncrv1.StartJob{JobId: jobID, Spec: transferSpec()}}}
}

func cancelCmd(cmdID, jobID string) *syncrv1.Command {
	return &syncrv1.Command{Id: cmdID, Command: &syncrv1.Command_CancelJob{CancelJob: &syncrv1.CancelJob{JobId: jobID}}}
}

// newAgent builds an agent over fr. It must be called inside the synctest
// bubble of the test; the caller shuts it down.
func newAgent(t *testing.T, fr *fakeRunner, cfg Config) *Agent {
	t.Helper()
	reg := runner.NewRegistry()
	if err := reg.Register(fr); err != nil {
		t.Fatal(err)
	}
	cfg.Runners = reg
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// shutdown stops a at the end of a test, failing it if jobs linger.
func shutdown(t *testing.T, a *Agent) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if err := a.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// handle runs commands and checks that each was acked in order.
func handle(t *testing.T, a *Agent, cmds ...*syncrv1.Command) {
	t.Helper()
	acks := a.HandleCommands(t.Context(), cmds)
	var want, got []string
	for _, c := range cmds {
		if c.GetId() != "" {
			want = append(want, c.GetId())
		}
	}
	for _, ack := range acks {
		got = append(got, ack.GetCommandId())
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("acks = %v, want %v", got, want)
	}
}

// events returns all unacked events after the Hello.
func events(a *Agent) []*syncrv1.Event {
	evs, _ := a.Outbox().Pending(1, 0)
	return evs
}

// describe renders events without seqs: "start(a) progress(a,10)
// result(a,SUCCEEDED)".
func describe(evs []*syncrv1.Event) string {
	parts := make([]string, 0, len(evs))
	for _, e := range evs {
		switch p := e.GetPayload().(type) {
		case *syncrv1.Event_Hello:
			parts = append(parts, "hello")
		case *syncrv1.Event_JobStarted:
			parts = append(parts, fmt.Sprintf("start(%s)", p.JobStarted.GetJobId()))
		case *syncrv1.Event_Progress:
			parts = append(parts, fmt.Sprintf("progress(%s,%d)", p.Progress.GetJobId(), p.Progress.GetStats().GetBytes()))
		case *syncrv1.Event_JobResult:
			status := strings.TrimPrefix(p.JobResult.GetStatus().String(), "JOB_RESULT_STATUS_")
			parts = append(parts, fmt.Sprintf("result(%s,%s)", p.JobResult.GetJobId(), status))
		}
	}
	return strings.Join(parts, " ")
}

// resultOf returns the JobResult of job among the unacked events.
func resultOf(t *testing.T, a *Agent, job string) *syncrv1.JobResult {
	t.Helper()
	var found *syncrv1.JobResult
	for _, e := range events(a) {
		if r := e.GetJobResult(); r.GetJobId() == job {
			if found != nil {
				t.Fatalf("two results for %s", job)
			}
			found = r
		}
	}
	if found == nil {
		t.Fatalf("no result for %s in %s", job, describe(events(a)))
	}
	return found
}

// stateOf renders a WorkerState: "a(running) b(queued)".
func stateOf(st *syncrv1.WorkerState) string {
	parts := make([]string, 0, len(st.GetJobs()))
	for _, j := range st.GetJobs() {
		s := "queued"
		if j.GetStartedAt() != nil {
			s = "running"
		}
		parts = append(parts, fmt.Sprintf("%s(%s)", j.GetJobId(), s))
	}
	return strings.Join(parts, " ")
}
