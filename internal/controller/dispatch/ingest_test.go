package dispatch_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

func TestControllerErrorCodesExistOnTheWire(t *testing.T) {
	t.Parallel()
	for _, code := range []string{
		jobs.CodeInternal, jobs.CodeCancelled, jobs.CodeWorkerShutdown, jobs.CodeDispatchTimeout,
		jobs.CodeWorkerRestarted, jobs.CodeWorkerLost, jobs.CodeWorkerRemoved, jobs.CodeInvalidSpec, jobs.CodeRemoteUnavailable,
		jobs.CodeRepositoryUnavailable, jobs.CodeWorkerOutdated, jobs.CodeTimeLimit,
	} {
		if link.ParseErrorCode(code) == syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSPECIFIED {
			t.Errorf("jobs code %q has no JobErrorCode", code)
		}
	}
}

func TestStartJobDerivationAndAcks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	wid := e.enroll("w")
	other := e.enroll("other")
	j := e.run(wid)
	const ep = "epoch-a"
	e.hello(wid, ep)

	cmds, err := e.disp.Commands(ctx, wid, ep)
	if err != nil || len(cmds) != 1 {
		t.Fatalf("Commands = (%v, %v), want one StartJob", cmds, err)
	}
	sj := cmds[0].GetStartJob()
	if cmds[0].GetId() != "start:"+j.ID.String() || sj.GetJobId() != j.ID.String() || !proto.Equal(sj.GetSpec(), testSpec("/data")) {
		t.Errorf("command = %v", cmds[0])
	}
	// Redelivered with the same ID until acked; nothing for other workers.
	again, _ := e.disp.Commands(ctx, wid, ep)
	if len(again) != 1 || again[0].GetId() != cmds[0].GetId() {
		t.Errorf("redelivery = %v", again)
	}
	if none, _ := e.disp.Commands(ctx, other, "epoch-o"); len(none) != 0 {
		t.Errorf("another worker got %v", none)
	}

	// Acks of unknown, malformed and foreign commands are ignored.
	junk := []*syncrv1.CommandAck{{CommandId: "start:not-a-uuid"}, {CommandId: "bogus"}, {CommandId: "start:" + domain.NewJobID().String()}}
	if err := e.disp.Ack(ctx, wid, ep, junk); err != nil {
		t.Errorf("Ack(junk) = %v", err)
	}
	if err := e.disp.Ack(ctx, other, "epoch-o", []*syncrv1.CommandAck{{CommandId: cmds[0].GetId()}}); err != nil {
		t.Errorf("foreign ack = %v", err)
	}
	if got := e.job(j.ID); got.Status != domain.JobQueued {
		t.Fatalf("after foreign acks the job is %s", got.Status)
	}

	ack := []*syncrv1.CommandAck{{CommandId: cmds[0].GetId()}}
	for range 3 { // idempotent, also across Report and Poll
		if err := e.disp.Ack(ctx, wid, ep, ack); err != nil {
			t.Fatal(err)
		}
	}
	got := e.job(j.ID)
	if got.Status != domain.JobDispatched || got.Epoch != ep || got.Version != 2 {
		t.Errorf("after acks: %s epoch %q v%d, want dispatched by %s at v2", got.Status, got.Epoch, got.Version, ep)
	}
	if left, _ := e.disp.Commands(ctx, wid, ep); len(left) != 0 {
		t.Errorf("commands after ack = %v", left)
	}
}

func TestCommandsAreCapped(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.cfg.MaxCommands = 2
	e.restart()
	wid := e.enroll("w")
	e.hello(wid, "ep")
	var want []string
	for range 5 {
		want = append(want, "start:"+e.run(wid).ID.String())
	}
	cmds, err := e.disp.Commands(context.Background(), wid, "ep")
	if err != nil || len(cmds) != 2 || cmds[0].GetId() != want[0] || cmds[1].GetId() != want[1] {
		t.Errorf("Commands = %v (%v), want the two oldest", cmds, err)
	}
}

func TestUnreadableSpecFailsTheJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	wid := e.enroll("w")
	e.hello(wid, "ep")
	for name, p := range map[string]jobs.CreateParams{
		"not JSON":          {Kind: "rclone_transfer", Definition: []byte{0xff, 0xff, 0xff}},
		"unknown field":     {Kind: "rclone_transfer", Definition: []byte(`{"rclone_transfer":{"spec":{}}}`)},
		"rejected by Check": {Kind: "rclone_transfer", Definition: []byte(`{"rclone_transfer":{"mode":"RCLONE_TRANSFER_MODE_COPY"}}`)},
		"unknown kind":      {Kind: "restic_backup", Definition: []byte(`{}`)},
		"another kind":      {Kind: "restic_backup", Definition: []byte(`{"rclone_transfer":{"mode":"RCLONE_TRANSFER_MODE_COPY","source":{"path":"/a"},"destination":{"path":"/b"}}}`)},
	} {
		p.WorkerID = wid
		j, err := e.jobs.Create(ctx, testActor(), p)
		if err != nil {
			t.Fatal(err)
		}
		if cmds, err := e.disp.Commands(ctx, wid, "ep"); err != nil || len(cmds) != 0 {
			t.Errorf("%s: Commands = (%v, %v)", name, cmds, err)
		}
		if got := e.job(j.ID); got.Status != domain.JobFailed || got.Error.Code != jobs.CodeInvalidSpec {
			t.Errorf("%s: job = %s %+v", name, got.Status, got.Error)
		}
	}
}

// TestSpecFromDefinition: the StartJob spec is built from the stored
// definition with the owner's resolved remotes at every derivation; the
// secret travels in the spec only, never in the job, its updates or logs.
func TestSpecFromDefinition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	wid := e.enroll("w")
	e.hello(wid, "ep")
	j := e.run(wid)
	if !slices.Equal(j.RemoteIDs, []domain.RemoteID{testRemote()}) || !strings.Contains(j.SearchText, "archive") {
		t.Errorf("job remotes %v, search text %q", j.RemoteIDs, j.SearchText)
	}
	for range 2 { // rebuilt (and resolved) for every derivation
		cmds, err := e.disp.Commands(ctx, wid, "ep")
		if err != nil || len(cmds) != 1 || !proto.Equal(cmds[0].GetStartJob().GetSpec(), testSpec("/data")) {
			t.Fatalf("Commands = (%v, %v)", cmds, err)
		}
	}
	if n := e.remotes.calls(); n != 2 {
		t.Errorf("Resolve called %d times, want once per derivation", n)
	}

	// A job on the worker's filesystem only resolves nothing.
	local := e.runDef(wid, &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransferDefinition{
		Mode: syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_SYNC, Source: &syncrv1.JobLocation{Path: "/data/a/"}, Destination: &syncrv1.JobLocation{Path: `D:\b`},
	}}})
	cmds, err := e.disp.Commands(ctx, wid, "ep")
	if err != nil || len(cmds) != 2 || e.remotes.calls() != 3 {
		t.Fatalf("Commands = (%v, %v), %d resolves", cmds, err, e.remotes.calls())
	}
	want := &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{
		Mode: syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_SYNC, Source: &syncrv1.Location{Path: "/data/a"}, Destination: &syncrv1.Location{Path: `D:\b`},
	}}}
	if cmds[1].GetStartJob().GetJobId() != local.ID.String() || !proto.Equal(cmds[1].GetStartJob().GetSpec(), want) {
		t.Errorf("local job command = %v", cmds[1])
	}

	// The secret is nowhere but in the commands.
	stored := e.job(j.ID)
	for what, text := range map[string]string{
		"definition":  string(stored.Definition),
		"search text": stored.SearchText,
		"logs":        e.logs.String(),
		"job":         fmt.Sprintf("%+v", stored),
	} {
		if strings.Contains(text, testSecret) {
			t.Errorf("the remote's secret is in the %s", what)
		}
	}
	e.rec.mu.Lock()
	defer e.rec.mu.Unlock()
	if len(e.rec.history[j.ID]) == 0 {
		t.Error("no job update was published")
	}
}

// TestSpecResolveErrors: at dispatch time a deleted or foreign remote
// fails the queued job (remote_unavailable), unusable credentials fail it
// (internal), and a transient error fails only the call: the next poll
// sends the job.
func TestSpecResolveErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("deleted remote", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		wid := e.enroll("w")
		e.hello(wid, "ep")
		j := e.run(wid)
		e.remotes.remove(testRemote())
		if cmds, err := e.disp.Commands(ctx, wid, "ep"); err != nil || len(cmds) != 0 {
			t.Errorf("Commands = (%v, %v)", cmds, err)
		}
		if got := e.job(j.ID); got.Status != domain.JobFailed || got.Error.Code != jobs.CodeRemoteUnavailable || strings.Contains(got.Error.Message, testRemote().String()) {
			t.Errorf("job = %s %+v", got.Status, got.Error)
		}
	})
	t.Run("foreign remote", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		wid := e.enroll("w")
		e.hello(wid, "ep")
		j := e.run(wid)
		// The remote changed hands (as far as the resolver can tell).
		e.remotes.mu.Lock()
		e.remotes.owners[testRemote()] = domain.NewUserID()
		e.remotes.mu.Unlock()
		if _, err := e.disp.Commands(ctx, wid, "ep"); err != nil {
			t.Fatal(err)
		}
		if got := e.job(j.ID); got.Status != domain.JobFailed || got.Error.Code != jobs.CodeRemoteUnavailable {
			t.Errorf("job = %s %+v", got.Status, got.Error)
		}
	})
	t.Run("undecryptable credentials", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		wid := e.enroll("w")
		e.hello(wid, "ep")
		j := e.run(wid)
		e.remotes.failNext(fmt.Errorf("decrypt the secrets of remote %s: %w", testRemote(), domain.ErrFailedPrecondition))
		if _, err := e.disp.Commands(ctx, wid, "ep"); err != nil {
			t.Fatal(err)
		}
		if got := e.job(j.ID); got.Status != domain.JobFailed || got.Error.Code != jobs.CodeInternal {
			t.Errorf("job = %s %+v", got.Status, got.Error)
		}
	})
	t.Run("transient error", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		wid := e.enroll("w")
		w := e.newFakeWorker(wid)
		w.report()
		j := e.run(wid)
		e.remotes.failNext(errors.New("connection refused"))
		_, err := w.disp().Poll(ctx, wid, &syncrv1.PollRequest{Epoch: w.epoch, Wait: durationpb.New(0)})
		if err == nil || e.job(j.ID).Status != domain.JobQueued {
			t.Fatalf("first poll = %v, job %s: want an error and the job still queued", err, e.job(j.ID).Status)
		}
		cmds := w.poll()
		if len(cmds) != 1 || cmds[0].GetStartJob().GetJobId() != j.ID.String() || !proto.Equal(cmds[0].GetStartJob().GetSpec(), testSpec("/data")) {
			t.Errorf("second poll = %v", cmds)
		}
		if strings.Contains(e.logs.String(), testSecret) {
			t.Error("the remote's secret is in the logs")
		}
	})
}

// specBuilderFunc adapts a function to dispatch.SpecBuilder.
type specBuilderFunc func(context.Context, jobs.Job) (*syncrv1.JobSpec, error)

func (f specBuilderFunc) Build(ctx context.Context, j jobs.Job, _ workers.Info) (*syncrv1.JobSpec, error) {
	return f(ctx, j)
}

// TestSpecBuildErrors: a job whose remote is gone fails with
// remote_unavailable, an unbuildable one with invalid_spec, one without a
// spec builder with invalid_spec; any other error is retried.
func TestSpecBuildErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		name     string
		specs    dispatch.SpecBuilder
		wantCode string // "" means the job stays queued and Commands fails
	}{
		{"remote gone", specBuilderFunc(func(context.Context, jobs.Job) (*syncrv1.JobSpec, error) {
			return nil, fmt.Errorf("resolve: %w", domain.ErrNotFound)
		}), jobs.CodeRemoteUnavailable},
		{"unusable credentials", specBuilderFunc(func(context.Context, jobs.Job) (*syncrv1.JobSpec, error) {
			return nil, fmt.Errorf("decrypt: %w", domain.ErrFailedPrecondition)
		}), jobs.CodeInternal},
		{"invalid definition", specBuilderFunc(func(context.Context, jobs.Job) (*syncrv1.JobSpec, error) {
			return nil, fmt.Errorf("build: %w", domain.ErrInvalidArgument)
		}), jobs.CodeInvalidSpec},
		{"no builder", nil, jobs.CodeInvalidSpec},
		{"database down", specBuilderFunc(func(context.Context, jobs.Job) (*syncrv1.JobSpec, error) {
			return nil, errors.New("connection refused")
		}), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			wid := e.enroll("w")
			e.hello(wid, "ep")
			j := e.run(wid)
			disp, err := dispatch.New(dispatch.Deps{Jobs: e.jobs, Workers: e.workers, Specs: tt.specs, Clock: e.clock, Hub: e.hub}, e.cfg)
			if err != nil {
				t.Fatal(err)
			}
			cmds, err := disp.Commands(ctx, wid, "ep")
			if tt.wantCode == "" {
				if err == nil || e.job(j.ID).Status != domain.JobQueued {
					t.Errorf("Commands = (%v, %v), job %s: want an error and the job still queued", cmds, err, e.job(j.ID).Status)
				}
				return
			}
			if err != nil || len(cmds) != 0 {
				t.Errorf("Commands = (%v, %v)", cmds, err)
			}
			if got := e.job(j.ID); got.Status != domain.JobFailed || got.Error.Code != tt.wantCode {
				t.Errorf("job = %s %+v, want failed with %s", got.Status, got.Error, tt.wantCode)
			}
		})
	}
}

// TestIngestIsIdempotent replays the same outbox under different delivery
// faults; the outcome must be identical to one clean delivery.
func TestIngestIsIdempotent(t *testing.T) {
	t.Parallel()
	// seq: 1 Hello, 2 JobStarted, 3 Progress, (4 coalesced away), 5 Progress, 6 JobResult
	tests := []struct {
		name    string
		batches [][]int64 // seqs per delivered Report; -1 marks a lost response
		acks    []int64   // expected ack_seq per delivered batch
	}{
		{"once, in order", [][]int64{{1, 2, 3, 5, 6}}, []int64{6}},
		{"duplicated", [][]int64{{1, 2, 3, 5, 6}, {1, 2, 3, 5, 6}}, []int64{6, 6}},
		{"reordered within a batch", [][]int64{{6, 3, 1, 5, 2}}, []int64{6}},
		{"newer batch first, older retry later", [][]int64{{1, 2, 3, 5, 6}, {1, 2, 3}}, []int64{6, 6}},
		{"response lost, everything resent", [][]int64{{1, 2, 3}, {1, 2, 3, 5, 6}}, []int64{3, 6}},
		{"overlapping batches", [][]int64{{1, 2}, {2, 3, 5}, {5, 6}}, []int64{2, 5, 6}},
		{"duplicate seq inside a batch", [][]int64{{1, 2, 2, 3, 5, 6, 6}}, []int64{6}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			wid := e.enroll("w")
			j := e.run(wid)
			w := e.newFakeWorker(wid)
			hello := w.outbox[0]
			w.report() // the Hello: no StartJob before it
			w.poll()   // StartJob, acked with the next request
			id := j.ID.String()
			w.start(id)
			w.progress(id, 10)
			w.seq++ // seq 4: a coalesced progress event that never reaches the controller
			w.progress(id, 30)
			w.finish(id, syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED, nil)
			bySeq := map[int64]*syncrv1.Event{1: hello}
			for _, ev := range w.outbox {
				bySeq[ev.GetSeq()] = ev
			}

			for i, seqs := range tt.batches {
				req := &syncrv1.ReportRequest{Epoch: w.epoch, Acks: w.acks, State: w.state()}
				for _, s := range seqs {
					req.Events = append(req.Events, bySeq[s])
				}
				resp, err := e.disp.Report(context.Background(), wid, req)
				if err != nil || resp.GetAckSeq() != tt.acks[i] {
					t.Fatalf("batch %d: ack = (%d, %v), want %d", i, resp.GetAckSeq(), err, tt.acks[i])
				}
			}

			got := e.job(j.ID)
			if got.Status != domain.JobSucceeded || got.Version != 4 || got.Stats.Bytes != 1<<33 {
				t.Errorf("job = %s v%d bytes %d, want succeeded v4 (each change applied once)", got.Status, got.Version, got.Stats.Bytes)
			}
			want := []domain.JobStatus{domain.JobQueued, domain.JobDispatched, domain.JobRunning, domain.JobSucceeded}
			if st := e.rec.statuses(j.ID); !slices.Equal(st, want) {
				t.Errorf("published statuses = %v", st)
			}
			if info := e.worker(wid).Info; info.Version != "v2.0.0-test" || len(info.LocalRoots) != 1 {
				t.Errorf("worker info = %+v", info)
			}
			if wk := e.worker(wid); wk.Link.AckSeq != 6 || wk.Link.Epoch != w.epoch {
				t.Errorf("link = %+v", wk.Link)
			}
		})
	}
}

func TestIngestRejectsAndSkips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	wid, other := e.enroll("w"), e.enroll("other")
	foreign := e.run(other)
	w := e.newFakeWorker(wid)
	w.report() // Hello, ack 1

	// A non-positive seq is a protocol violation: nothing is applied.
	bad := &syncrv1.ReportRequest{Epoch: w.epoch, Events: []*syncrv1.Event{{Seq: 0, Payload: &syncrv1.Event_JobStarted{JobStarted: &syncrv1.JobStarted{JobId: foreign.ID.String()}}}}}
	if _, err := e.disp.Report(ctx, wid, bad); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("seq 0: err = %v, want ErrInvalidArgument", err)
	}
	if _, err := e.disp.Report(ctx, wid, &syncrv1.ReportRequest{Epoch: ""}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("empty epoch: err = %v", err)
	}

	// Events about jobs this worker does not own, malformed IDs and
	// unknown event types are skipped and acknowledged.
	w.emit(&syncrv1.Event{Payload: &syncrv1.Event_JobStarted{JobStarted: &syncrv1.JobStarted{JobId: foreign.ID.String()}}})
	w.emit(&syncrv1.Event{Payload: &syncrv1.Event_JobResult{JobResult: &syncrv1.JobResult{JobId: "not-a-uuid", Status: syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED}}})
	w.emit(&syncrv1.Event{Payload: &syncrv1.Event_Progress{Progress: &syncrv1.Progress{JobId: domain.NewJobID().String()}}})
	w.emit(&syncrv1.Event{}) // a payload this controller does not know
	if ack := w.report(); ack != 5 {
		t.Errorf("ack = %d, want 5 (all skipped events acknowledged)", ack)
	}
	if got := e.job(foreign.ID); got.Status != domain.JobQueued {
		t.Errorf("another worker's job became %s", got.Status)
	}
}

func TestJobResultMapping(t *testing.T) {
	t.Parallel()
	output := &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticBackup{ResticBackup: &syncrv1.ResticBackupOutput{SnapshotId: "abc"}}}
	tests := []struct {
		name     string
		cancel   bool
		result   *syncrv1.JobResult
		status   domain.JobStatus
		code     string
		notified bool
	}{
		{"succeeded", false, &syncrv1.JobResult{Status: syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED, Warnings: []string{"3 files unreadable"}, Output: output}, domain.JobSucceeded, "", false},
		{"failed", false, &syncrv1.JobResult{Status: syncrv1.JobResultStatus_JOB_RESULT_STATUS_FAILED, Error: &syncrv1.JobError{Code: syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED, Message: "outside local_roots"}}, domain.JobFailed, "path_not_allowed", true},
		{"failed without details", false, &syncrv1.JobResult{Status: syncrv1.JobResultStatus_JOB_RESULT_STATUS_FAILED}, domain.JobFailed, jobs.CodeInternal, true},
		{"cancelled on request", true, &syncrv1.JobResult{Status: syncrv1.JobResultStatus_JOB_RESULT_STATUS_CANCELLED, Error: &syncrv1.JobError{Code: syncrv1.JobErrorCode_JOB_ERROR_CODE_CANCELLED}}, domain.JobCancelled, jobs.CodeCancelled, false},
		{"stopped by worker shutdown", false, &syncrv1.JobResult{Status: syncrv1.JobResultStatus_JOB_RESULT_STATUS_CANCELLED, Error: &syncrv1.JobError{Code: syncrv1.JobErrorCode_JOB_ERROR_CODE_WORKER_SHUTDOWN, Message: "shutting down"}}, domain.JobInterrupted, jobs.CodeWorkerShutdown, true},
		{"unknown status", false, &syncrv1.JobResult{Status: syncrv1.JobResultStatus(42)}, domain.JobFailed, jobs.CodeInternal, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			wid := e.enroll("w")
			j := e.run(wid)
			w := e.newFakeWorker(wid)
			w.report()
			w.poll()
			w.start(j.ID.String())
			w.report()
			if tt.cancel {
				if _, err := e.jobs.Cancel(context.Background(), testActor(), j.ID); err != nil {
					t.Fatal(err)
				}
			}
			tt.result.JobId = j.ID.String()
			w.emit(&syncrv1.Event{Payload: &syncrv1.Event_JobResult{JobResult: tt.result}})
			w.held = nil
			w.report()
			got := e.job(j.ID)
			if got.Status != tt.status || got.Error.Code != tt.code {
				t.Errorf("job = %s %q, want %s %q", got.Status, got.Error.Code, tt.status, tt.code)
			}
			if (e.rec.failures() == 1) != tt.notified {
				t.Errorf("notifier calls = %d, want notified %v", e.rec.failures(), tt.notified)
			}
			if tt.status == domain.JobSucceeded {
				var out syncrv1.JobOutput
				if err := proto.Unmarshal(got.Output, &out); err != nil || !proto.Equal(&out, output) || got.Warnings[0] != "3 files unreadable" {
					t.Errorf("output = %v (%v), warnings %v", &out, err, got.Warnings)
				}
			}
		})
	}
}

// TestShutdownGivesBackUnstartedJobs: a worker that shuts down reports the
// jobs waiting in its local queue CANCELLED (WORKER_SHUTDOWN) without a
// JobStarted. They ran nowhere, so they go back to the queue, as after a
// crash, instead of ending interrupted with a notification each; the
// running job is interrupted; a job with a cancel request ends cancelled.
// The worker's next process receives the re-queued jobs.
func TestShutdownGivesBackUnstartedJobs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	wid := e.enroll("w")
	running, waiting, asked := e.run(wid), e.run(wid), e.run(wid)
	unacked := e.run(wid)
	w := e.newFakeWorker(wid)
	w.report()
	w.poll()
	w.acks = w.acks[:3] // the ack of unacked is still in flight
	w.start(running.ID.String())
	w.report()
	if _, err := e.jobs.Cancel(ctx, testActor(), asked.ID); err != nil {
		t.Fatal(err)
	}

	shutdown := &syncrv1.JobError{Code: syncrv1.JobErrorCode_JOB_ERROR_CODE_WORKER_SHUTDOWN, Message: "job cancelled because the worker is shutting down"}
	for _, j := range []jobs.Job{running, waiting, asked, unacked} {
		w.finish(j.ID.String(), syncrv1.JobResultStatus_JOB_RESULT_STATUS_CANCELLED, shutdown)
	}
	w.acks = nil
	w.report()

	for _, tc := range []struct {
		job      jobs.Job
		status   domain.JobStatus
		code     string
		requeues int
	}{
		{running, domain.JobInterrupted, jobs.CodeWorkerShutdown, 0},
		{waiting, domain.JobQueued, "", 1},
		{asked, domain.JobCancelled, jobs.CodeWorkerShutdown, 0},
		{unacked, domain.JobQueued, "", 1},
	} {
		got := e.job(tc.job.ID)
		if got.Status != tc.status || got.Error.Code != tc.code || got.Requeues != tc.requeues || (tc.status == domain.JobQueued && got.Epoch != "") {
			t.Errorf("job %s = %s %q requeues %d epoch %q, want %s %q requeues %d",
				tc.job.ID, got.Status, got.Error.Code, got.Requeues, got.Epoch, tc.status, tc.code, tc.requeues)
		}
	}
	if n := e.rec.failures(); n != 1 {
		t.Errorf("notifier calls = %d, want 1 (the running job)", n)
	}

	// The worker's next process receives the re-queued jobs and runs them.
	w.restart()
	w.report() // its Hello
	cmds := w.poll()
	var ids []string
	for _, c := range cmds {
		ids = append(ids, c.GetStartJob().GetJobId())
	}
	if !slices.Equal(ids, []string{waiting.ID.String(), unacked.ID.String()}) {
		t.Fatalf("StartJobs after the restart = %v", ids)
	}
	w.start(waiting.ID.String())
	w.finish(waiting.ID.String(), syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED, nil)
	w.report()
	if got := e.job(waiting.ID); got.Status != domain.JobSucceeded || got.Epoch != w.epoch {
		t.Errorf("re-queued job = %s by %q", got.Status, got.Epoch)
	}
}

func TestStaleEpochIsRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	wid := e.enroll("w")
	j := e.run(wid)
	w := e.newFakeWorker(wid)
	w.report()
	w.poll()
	w.start(j.ID.String())
	old := w.reportRequest() // will be delivered late
	w.report()
	w.restart()
	w.report() // the new process: the job is interrupted
	before := e.worker(wid)
	e.clock.Advance(1)

	_, err := e.disp.Report(ctx, wid, old)
	if !errors.Is(err, dispatch.ErrStaleEpoch) || !errors.Is(err, domain.ErrFailedPrecondition) {
		t.Fatalf("late request of the old process: err = %v, want ErrStaleEpoch", err)
	}
	for _, call := range []func() error{
		func() error { return e.disp.Ack(ctx, wid, old.GetEpoch(), nil) },
		func() error { _, err := e.disp.Commands(ctx, wid, old.GetEpoch()); return err },
	} {
		if err := call(); !errors.Is(err, dispatch.ErrStaleEpoch) {
			t.Errorf("err = %v, want ErrStaleEpoch", err)
		}
	}
	after := e.worker(wid)
	if after.Link.Epoch != w.epoch || after.Link.AckSeq != before.Link.AckSeq || !after.Link.LastSeen.Equal(before.Link.LastSeen) {
		t.Errorf("a stale request changed the link: %+v -> %+v", before.Link, after.Link)
	}
	if got := e.job(j.ID); got.Status != domain.JobInterrupted {
		t.Errorf("job = %s", got.Status)
	}
}

func TestRemovedWorkerIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	wid := e.enroll("w")
	queued, running := e.run(wid), e.run(wid)
	w := e.newFakeWorker(wid)
	w.report()
	w.poll()
	w.start(running.ID.String())
	w.report()
	if err := e.workers.Remove(ctx, wid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.disp.Report(ctx, wid, w.reportRequest()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Report of a removed worker: err = %v", err)
	}
	if got := e.job(queued.ID); got.Status != domain.JobInterrupted && got.Status != domain.JobFailed {
		t.Errorf("queued/dispatched job = %s", got.Status)
	}
	if got := e.job(running.ID); got.Status != domain.JobInterrupted || got.Error.Code != jobs.CodeWorkerRemoved {
		t.Errorf("running job = %s %q", got.Status, got.Error.Code)
	}
}
