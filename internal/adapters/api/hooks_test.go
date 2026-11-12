package api

import (
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// setWorkerHooks makes the env's worker report features and the hooks it
// defines.
func (e *testEnv) setWorkerHooks(features []string, hooks ...workers.Hook) {
	e.t.Helper()
	info := workers.Info{Version: "v2.1.0", OS: "linux", Features: features, Hooks: hooks, ReportedAt: e.clock.Now()}
	if err := e.store.SetWorkerInfo(e.t.Context(), e.worker, "", info); err != nil {
		e.t.Fatal(err)
	}
}

func appHooks() []workers.Hook {
	return []workers.Hook{
		{Name: "start-app", Description: "Start the app again", Timeout: time.Minute},
		{Name: "stop-app", Description: "Stop the app, so its files stay still", Timeout: 2 * time.Minute},
	}
}

// hookedDefinition is testDefinition with hooks.
func hookedDefinition(h *syncrv1.JobHooks) *syncrv1.JobDefinition {
	def := testDefinition()
	def.Hooks = h
	return def
}

// TestRunJobWithHooks: a job may name hooks its worker defines. A name the
// worker's latest report lacks is refused where it is named; a worker that
// cannot run hooks is refused for the hooks as a whole; a worker that has
// not reported yet passes (the dispatcher decides).
func TestRunJobWithHooks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	hooks := &syncrv1.JobHooks{Before: []string{"stop-app"}, After: []string{"start-app"}}

	// Before the worker's first report nothing is known about its hooks.
	early, err := e.runAs(testUser, hookedDefinition(hooks))
	if err != nil || !proto.Equal(early.GetDefinition().GetHooks(), hooks) {
		t.Fatalf("RunJob before the worker reported = %v, %v", early, err)
	}

	e.setWorkerHooks(link.Features(), appHooks()...)
	j, err := e.runAs(testUser, hookedDefinition(hooks))
	if err != nil {
		t.Fatalf("RunJob with the worker's hooks: %v", err)
	}
	if !proto.Equal(j.GetDefinition().GetHooks(), hooks) || len(j.GetHookRuns()) != 0 || j.GetTitle() == "" {
		t.Errorf("job = %v, want its hooks back and no runs yet", j)
	}
	stored, err := e.jsvc.Get(t.Context(), testUser, e.jobID(j))
	if err != nil || !strings.Contains(string(stored.Definition), `"hooks":{"before":["stop-app"],"after":["start-app"]}`) {
		t.Errorf("stored definition = %s, %v", stored.Definition, err)
	}

	// Hooks the worker does not define, each where it is first named.
	_, err = e.runAs(testUser, hookedDefinition(&syncrv1.JobHooks{
		Before: []string{"stop-app", "dump-db"}, OnFailure: []string{"alert"}, After: []string{"start-app", "dump-db"},
	}))
	if codeOf(t, err) != connect.CodeInvalidArgument ||
		violation(err, "definition.hooks.before[1]") != `worker "worker" defines no hook named "dump-db": its operator adds hooks in the worker's configuration` ||
		violation(err, "definition.hooks.on_failure[0]") != `worker "worker" defines no hook named "alert": its operator adds hooks in the worker's configuration` ||
		violation(err, "definition.hooks.after[1]") != "" || violation(err, "definition.hooks.before[0]") != "" {
		t.Errorf("RunJob with hooks the worker lacks = %v (%v)", err, violations(t, err))
	}

	// A name that is none is refused whatever the worker.
	_, err = e.runAs(testUser, hookedDefinition(&syncrv1.JobHooks{Before: []string{"rm -rf /"}}))
	if codeOf(t, err) != connect.CodeInvalidArgument || !strings.Contains(violation(err, "definition.hooks.before[0]"), "is not a hook name") {
		t.Errorf("RunJob with a command for a hook name = %v", err)
	}

	// A worker from before hooks existed.
	e.setWorkerHooks(slices.DeleteFunc(link.Features(), func(f string) bool { return f == link.FeatureJobHooks }))
	_, err = e.runAs(testUser, hookedDefinition(hooks))
	if codeOf(t, err) != connect.CodeInvalidArgument ||
		violation(err, "definition.hooks") != `cannot be used on worker "worker" yet: it must be upgraded to run hooks` {
		t.Errorf("RunJob with hooks on an old worker = %v", err)
	}
	if _, err := e.runAs(testUser, testDefinition()); err != nil {
		t.Errorf("RunJob without hooks on an old worker = %v", err)
	}
	if _, err := e.runAs(testUser, hookedDefinition(&syncrv1.JobHooks{})); err != nil {
		t.Errorf("RunJob with empty hooks on an old worker = %v", err)
	}
}

// TestJobHookRuns: a finished job lists the hooks its worker ran; one a
// hook failed after its operation succeeded still shows the operation's
// output.
func TestJobHookRuns(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.jobsFor(testUser)
	ctx := t.Context()
	e.setWorkerHooks(link.Features(), appHooks()...)
	j, err := e.runAs(testUser, hookedDefinition(&syncrv1.JobHooks{Before: []string{"stop-app"}, After: []string{"start-app"}}))
	if err != nil {
		t.Fatal(err)
	}
	e.startRunning(j)
	out, err := proto.Marshal(&syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticBackup{ResticBackup: &syncrv1.ResticBackupOutput{SnapshotId: "abc123"}}})
	if err != nil {
		t.Fatal(err)
	}
	at := e.clock.Now().Add(-time.Minute)
	if _, _, err := e.jsvc.Finish(ctx, e.worker, e.jobID(j), testEpoch, jobs.Outcome{
		Status: domain.JobFailed, ExitCode: 0, Output: out, Warnings: []string{"w1"},
		Error: jobs.JobError{Code: jobs.CodeHookFailed, Message: `hook "start-app" (after) failed: exited with code 1`},
		HookRuns: []jobs.HookRun{
			{Name: "stop-app", Phase: domain.HookBefore, Status: domain.HookSucceeded, StartedAt: at, Duration: 1500 * time.Millisecond, Output: "app stopped"},
			{Name: "start-app", Phase: domain.HookAfter, Status: domain.HookFailed, ExitCode: 1, StartedAt: at.Add(30 * time.Second),
				Duration: 2 * time.Second, Message: "exited with code 1", Output: "Error: no such container", OutputTruncated: true},
			{Name: "odd", Phase: "later", Status: "unheard-of", ExitCode: -1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := c.GetJob(ctx, connect.NewRequest(&syncrv1.GetJobRequest{JobId: j.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Msg.GetJob()
	if got.GetStatus() != syncrv1.JobStatus_JOB_STATUS_FAILED || got.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_HOOK_FAILED ||
		got.GetOutput().GetResticBackup().GetSnapshotId() != "abc123" || len(got.GetWarnings()) != 1 {
		t.Errorf("job = %v, want it failed by its hook with the operation's output and warning", got)
	}
	runs := got.GetHookRuns()
	if len(runs) != 3 {
		t.Fatalf("hook runs = %v", runs)
	}
	if r := runs[0]; r.GetName() != "stop-app" || r.GetPhase() != syncrv1.HookPhase_HOOK_PHASE_BEFORE || r.GetStatus() != syncrv1.HookStatus_HOOK_STATUS_SUCCEEDED ||
		r.GetExitCode() != 0 || !r.GetStartedAt().AsTime().Equal(at) || r.GetDuration().AsDuration() != 1500*time.Millisecond ||
		r.GetOutput() != "app stopped" || r.GetOutputTruncated() || r.GetMessage() != "" {
		t.Errorf("first run = %v", r)
	}
	if r := runs[1]; r.GetName() != "start-app" || r.GetPhase() != syncrv1.HookPhase_HOOK_PHASE_AFTER || r.GetStatus() != syncrv1.HookStatus_HOOK_STATUS_FAILED ||
		r.GetExitCode() != 1 || r.GetMessage() != "exited with code 1" || r.GetOutput() != "Error: no such container" || !r.GetOutputTruncated() {
		t.Errorf("second run = %v", r)
	}
	// What this build does not know is unspecified, not an error.
	if r := runs[2]; r.GetPhase() != syncrv1.HookPhase_HOOK_PHASE_UNSPECIFIED || r.GetStatus() != syncrv1.HookStatus_HOOK_STATUS_UNSPECIFIED ||
		r.GetExitCode() != -1 || r.GetStartedAt() != nil {
		t.Errorf("third run = %v", r)
	}

	// The list carries them too (a job's page may be built from it).
	list, err := c.ListJobs(ctx, connect.NewRequest(&syncrv1.ListJobsRequest{}))
	if err != nil || len(list.Msg.GetJobs()) != 1 || len(list.Msg.GetJobs()[0].GetHookRuns()) != 3 {
		t.Errorf("ListJobs = %v, %v", list.Msg.GetJobs(), err)
	}
}

// TestWorkerInfoListsItsHooks: every signed-in user sees which hooks a
// worker offers, with their descriptions and timeouts and nothing else.
func TestWorkerInfoListsItsHooks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.setWorkerHooks(link.Features(), appHooks()...)
	for _, a := range []domain.Actor{testAdmin, testUser} {
		resp, err := e.workersFor(a).GetWorker(t.Context(), connect.NewRequest(&syncrv1.GetWorkerRequest{WorkerId: e.worker.String()}))
		if err != nil {
			t.Fatalf("GetWorker as %s: %v", a.Role, err)
		}
		hooks := resp.Msg.GetWorker().GetInfo().GetHooks()
		if len(hooks) != 2 || hooks[0].GetName() != "start-app" || hooks[0].GetDescription() != "Start the app again" ||
			hooks[0].GetTimeout().AsDuration() != time.Minute || hooks[1].GetName() != "stop-app" || hooks[1].GetTimeout().AsDuration() != 2*time.Minute {
			t.Errorf("hooks as %s = %v", a.Role, hooks)
		}
		if resp.Msg.GetWorker().GetOutdated() {
			t.Errorf("a worker with every feature is shown outdated as %s", a.Role)
		}
	}
	// A worker without the hooks feature is one to upgrade.
	e.setWorkerHooks(slices.DeleteFunc(link.Features(), func(f string) bool { return f == link.FeatureJobHooks }))
	resp, err := e.workersClient().GetWorker(t.Context(), connect.NewRequest(&syncrv1.GetWorkerRequest{WorkerId: e.worker.String()}))
	if err != nil || !resp.Msg.GetWorker().GetOutdated() || len(resp.Msg.GetWorker().GetInfo().GetHooks()) != 0 {
		t.Errorf("a worker from before hooks = %v, %v; want it outdated and without hooks", resp.Msg.GetWorker(), err)
	}
}

// TestScheduleWithHooks: a schedule on a worker is checked like RunJob; one
// on a label is not (a run picks a worker that defines the hooks).
func TestScheduleWithHooks(t *testing.T) {
	t.Parallel()
	e := newScheduleEnv(t)
	ctx := t.Context()
	e.setWorkerHooks(link.Features(), appHooks()...)
	hooks := &syncrv1.JobHooks{Before: []string{"stop-app"}, After: []string{"start-app"}}

	in := e.scheduleInput("Nightly, app stopped")
	in.Definition = hookedDefinition(hooks)
	sc := e.createSchedule(testUser, in)
	if !proto.Equal(sc.GetDefinition().GetHooks(), hooks) {
		t.Errorf("schedule hooks = %v, want %v", sc.GetDefinition().GetHooks(), hooks)
	}

	missing := e.scheduleInput("Missing hook")
	missing.Definition = hookedDefinition(&syncrv1.JobHooks{Before: []string{"dump-db"}})
	_, err := e.schedulesFor(testUser).CreateSchedule(ctx, connect.NewRequest(&syncrv1.CreateScheduleRequest{Input: missing}))
	if codeOf(t, err) != connect.CodeInvalidArgument || !strings.Contains(violation(err, "definition.hooks.before[0]"), `defines no hook named "dump-db"`) {
		t.Errorf("a schedule with a hook its worker lacks = %v", err)
	}
	// Updating a schedule to one is refused too.
	_, err = e.schedulesFor(testUser).UpdateSchedule(ctx, connect.NewRequest(&syncrv1.UpdateScheduleRequest{ScheduleId: sc.GetId(), Version: sc.GetVersion(), Input: missing}))
	if codeOf(t, err) != connect.CodeInvalidArgument || violation(err, "definition.hooks.before[0]") == "" {
		t.Errorf("updating a schedule to a hook its worker lacks = %v", err)
	}

	onLabel := e.scheduleInput("On a label")
	onLabel.Definition = hookedDefinition(&syncrv1.JobHooks{Before: []string{"dump-db"}})
	onLabel.Target = &syncrv1.ScheduleTarget{Target: &syncrv1.ScheduleTarget_WorkerLabel{WorkerLabel: "db"}}
	if _, err := e.schedulesFor(testUser).CreateSchedule(ctx, connect.NewRequest(&syncrv1.CreateScheduleRequest{Input: onLabel})); err != nil {
		t.Errorf("a schedule with hooks on a label = %v, want it accepted", err)
	}

	invalid := e.scheduleInput("Invalid name")
	invalid.Definition = hookedDefinition(&syncrv1.JobHooks{After: []string{"Start App"}})
	_, err = e.schedulesFor(testUser).CreateSchedule(ctx, connect.NewRequest(&syncrv1.CreateScheduleRequest{Input: invalid}))
	if codeOf(t, err) != connect.CodeInvalidArgument || violation(err, "definition.hooks.after[0]") == "" {
		t.Errorf("a schedule with an invalid hook name = %v", err)
	}
}
