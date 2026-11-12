package dispatch_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// hooked returns def with hooks.
func hooked(def *syncrv1.JobDefinition, h *syncrv1.JobHooks) *syncrv1.JobDefinition {
	def.Hooks = h
	return def
}

func appHooks() *syncrv1.JobHooks {
	return &syncrv1.JobHooks{Before: []string{"stop-app", "dump-db"}, OnFailure: []string{"alert"}, After: []string{"start-app"}}
}

func hookInfos(names ...string) []*syncrv1.HookInfo {
	out := make([]*syncrv1.HookInfo, len(names))
	for i, n := range names {
		out[i] = &syncrv1.HookInfo{Name: n, Description: "the " + n + " hook", Timeout: durationpb.New(10 * time.Minute)}
	}
	return out
}

func workerHooks(names ...string) []workers.Hook {
	out := make([]workers.Hook, len(names))
	for i, n := range names {
		out[i] = workers.Hook{Name: n}
	}
	return out
}

// finishWith appends result as the job's result and forgets the job, as
// finish does.
func (w *fakeWorker) finishWith(r *syncrv1.JobResult) {
	r.FinishedAt = timestamppb.New(w.clock.Now())
	w.emit(&syncrv1.Event{Payload: &syncrv1.Event_JobResult{JobResult: r}})
	id := r.GetJobId()
	w.held = slices.DeleteFunc(w.held, func(h string) bool { return h == id })
	delete(w.running, id)
	w.finished[id] = true
}

// TestSpecsHooks: the spec carries the hooks the definition names; a worker
// that cannot run hooks gets no job with hooks (ErrHooksUnsupported, which
// is ErrWorkerOutdated) and one that lacks a hook is named what it lacks
// (*MissingHooksError), but both still get every job without hooks.
func TestSpecsHooks(t *testing.T) {
	t.Parallel()
	specs, err := dispatch.NewSpecs(newFakeRemotes(), nil, kinds(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	all := workers.Info{Features: link.Features(), Hooks: workerHooks("alert", "dump-db", "start-app", "stop-app", "unused")}
	some := workers.Info{Features: link.Features(), Hooks: workerHooks("stop-app", "start-app")}
	none := workers.Info{Features: link.Features()}
	old := workers.Info{
		Features: slices.DeleteFunc(link.Features(), func(f string) bool { return f == link.FeatureJobHooks }),
		Hooks:    workerHooks("alert", "dump-db", "start-app", "stop-app"), // whatever it claims
	}

	j := storedJob(t, hooked(testDefinition("/data"), appHooks()))
	spec, err := specs.Build(ctx, j, all)
	if err != nil {
		t.Fatalf("Build on a worker with every hook: %v", err)
	}
	if !proto.Equal(spec.GetHooks(), appHooks()) {
		t.Errorf("spec hooks = %v, want %v", spec.GetHooks(), appHooks())
	}

	_, err = specs.Build(ctx, j, some)
	var missing *dispatch.MissingHooksError
	if !errors.As(err, &missing) || !slices.Equal(missing.Hooks, []string{"dump-db", "alert"}) || !errors.Is(err, domain.ErrInvalidArgument) ||
		errors.Is(err, dispatch.ErrWorkerOutdated) {
		t.Fatalf("Build on a worker with some hooks: %v, want a MissingHooksError naming dump-db and alert", err)
	}
	if got, want := missing.Message("nas"), `worker "nas" defines no hooks named "dump-db", "alert": add them to the worker's configuration (hooks) or remove them from the job`; got != want {
		t.Errorf("message:\n got %s\nwant %s", got, want)
	}
	_, err = specs.Build(ctx, storedJob(t, hooked(testDefinition("/data"), &syncrv1.JobHooks{After: []string{"start-app"}})), none)
	if !errors.As(err, &missing) || !slices.Equal(missing.Hooks, []string{"start-app"}) {
		t.Fatalf("Build on a worker without hooks: %v", err)
	}
	if got, want := missing.Message(""), `the worker defines no hook named "start-app": add it to the worker's configuration (hooks) or remove it from the job`; got != want {
		t.Errorf("message:\n got %s\nwant %s", got, want)
	}

	// The feature first: upgrading comes before configuring.
	_, err = specs.Build(ctx, j, old)
	if !errors.Is(err, dispatch.ErrHooksUnsupported) || !errors.Is(err, dispatch.ErrWorkerOutdated) || !errors.Is(err, domain.ErrFailedPrecondition) ||
		errors.Is(err, dispatch.ErrTimeLimitUnsupported) {
		t.Errorf("Build on a worker without the feature: %v, want ErrHooksUnsupported", err)
	}
	if _, err := specs.Build(ctx, storedJob(t, hooked(testDefinition("/data"), appHooks())), workers.Info{}); !errors.Is(err, dispatch.ErrHooksUnsupported) {
		t.Errorf("Build on a worker without features: %v, want ErrHooksUnsupported", err)
	}

	// Jobs without hooks run everywhere, with no hooks in their spec.
	for name, def := range map[string]*syncrv1.JobDefinition{
		"unset": testDefinition("/data"), "empty": hooked(testDefinition("/data"), &syncrv1.JobHooks{}),
	} {
		for _, w := range []workers.Info{all, none, old, {}} {
			spec, err := specs.Build(ctx, storedJob(t, def), w)
			if err != nil || spec.GetHooks() != nil {
				t.Errorf("%s hooks on %+v: spec hooks %v, err %v", name, w.Features, spec.GetHooks(), err)
			}
		}
	}
}

// TestHooksNeedTheFeatureAndTheHooks: a job with hooks on a worker that
// cannot run hooks fails with worker_outdated, on one that does not define
// them with invalid_spec, each saying what to do; a worker that defines
// them is sent their names.
func TestHooksNeedTheFeatureAndTheHooks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	old := e.newFakeWorker(e.enroll("old"))
	old.features = slices.DeleteFunc(link.Features(), func(f string) bool { return f == link.FeatureJobHooks })
	old.restart()
	bare := e.newProbeWorker(e.enroll("bare"))
	nas := e.newProbeWorker(e.enroll("nas"))
	nas.hooks = hookInfos("stop-app", "dump-db", "alert", "start-app")
	nas.restart()
	for _, w := range []*fakeWorker{old, bare, nas} {
		w.report()
	}
	def := func() *syncrv1.JobDefinition { return hooked(testDefinition("/data"), appHooks()) }

	outdated := e.runDef(old.id, def())
	if got := started(old.poll()); len(got) != 0 {
		t.Errorf("the worker without the feature was sent %v", got)
	}
	if got := e.job(outdated.ID); got.Status != domain.JobFailed || got.Error.Code != jobs.CodeWorkerOutdated ||
		got.Error.Message != `worker "old" must be upgraded to run jobs with hooks` {
		t.Errorf("outdated = %s %q %q", got.Status, got.Error.Code, got.Error.Message)
	}

	undefined := e.runDef(bare.id, def())
	if got := started(bare.poll()); len(got) != 0 {
		t.Errorf("the worker without hooks was sent %v", got)
	}
	if got := e.job(undefined.ID); got.Status != domain.JobFailed || got.Error.Code != jobs.CodeInvalidSpec ||
		got.Error.Message != `worker "bare" defines no hooks named "stop-app", "dump-db", "alert", "start-app": add them to the worker's configuration (hooks) or remove them from the job` {
		t.Errorf("undefined = %s %q %q", got.Status, got.Error.Code, got.Error.Message)
	}
	// Both failures are notified like any other.
	if got := e.rec.failures(); got != 2 {
		t.Errorf("failures notified = %d, want 2", got)
	}

	fine := e.runDef(nas.id, def())
	if got := started(nas.poll()); !slices.Equal(got, []string{fine.ID.String()}) {
		t.Fatalf("the worker with the hooks was sent %v, want %v", got, fine.ID)
	}
	if got := nas.specs[fine.ID.String()].GetHooks(); !proto.Equal(got, appHooks()) {
		t.Errorf("spec hooks = %v, want %v", got, appHooks())
	}

	// A job without hooks runs on all of them.
	for _, w := range []*fakeWorker{old, bare} {
		j := e.run(w.id)
		if got := started(w.poll()); !slices.Equal(got, []string{j.ID.String()}) {
			t.Errorf("a job without hooks: the worker was sent %v, want %v", got, j.ID)
		}
		if got := w.specs[j.ID.String()].GetHooks(); got != nil {
			t.Errorf("a job without hooks: spec hooks = %v", got)
		}
	}

	// The hooks are those of the worker's current process: once the
	// operator removed one and restarted the worker, the job fails on it.
	nas.hooks = hookInfos("stop-app", "start-app", "alert")
	nas.restart()
	nas.report()
	later := e.runDef(nas.id, def())
	if got := started(nas.poll()); len(got) != 0 {
		t.Errorf("after the hook was removed the worker was sent %v", got)
	}
	if got := e.job(later.ID); got.Status != domain.JobFailed || got.Error.Code != jobs.CodeInvalidSpec ||
		!strings.Contains(got.Error.Message, `worker "nas" defines no hook named "dump-db"`) {
		t.Errorf("after the hook was removed: %s %q %q", got.Status, got.Error.Code, got.Error.Message)
	}
}

// TestHelloHooksAreStored: the hooks a worker reports are stored with its
// info, the ones a job could name: valid names, each once, sorted, their
// descriptions one line.
func TestHelloHooksAreStored(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.newProbeWorker(e.enroll("nas"))
	w.hooks = []*syncrv1.HookInfo{
		{Name: "start-app", Description: "Start the app\nagain", Timeout: durationpb.New(time.Minute)},
		{Name: "Not A Name", Description: "dropped"},
		{Name: "dump-db", Description: strings.Repeat("é", 300), Timeout: durationpb.New(15 * time.Minute)},
		{Name: "start-app", Description: "a duplicate"},
		{Name: "no-timeout", Timeout: &durationpb.Duration{Seconds: 1, Nanos: -1}},
		{Name: "negative", Timeout: durationpb.New(-time.Second)},
	}
	w.restart()
	w.report()
	got := e.worker(w.id).Info.Hooks
	want := []workers.Hook{
		{Name: "dump-db", Description: strings.Repeat("é", 200), Timeout: 15 * time.Minute},
		{Name: "negative"},
		{Name: "no-timeout"},
		{Name: "start-app", Description: "Start the app again", Timeout: time.Minute},
	}
	if !slices.Equal(got, want) {
		t.Errorf("stored hooks:\n got %+v\nwant %+v", got, want)
	}

	// A worker that reports more hooks than one may define keeps the first.
	for i := range 100 {
		w.hooks = append(w.hooks, &syncrv1.HookInfo{Name: "h" + strings.Repeat("x", i)})
	}
	w.restart()
	w.report()
	if got := len(e.worker(w.id).Info.Hooks); got != domain.MaxWorkerHooks {
		t.Errorf("stored %d hooks, want at most %d", got, domain.MaxWorkerHooks)
	}

	// A restart without hooks forgets them.
	w.hooks = nil
	w.restart()
	w.report()
	if got := e.worker(w.id).Info.Hooks; len(got) != 0 {
		t.Errorf("hooks after a Hello without any = %+v", got)
	}
}

// TestHookRunsAreStored: the hooks a worker ran are stored with the job's
// result, whatever its status; a job a hook failed after its operation
// succeeded keeps the operation's output and warnings.
func TestHookRunsAreStored(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.newProbeWorker(e.enroll("nas"))
	w.hooks = hookInfos("stop-app", "dump-db", "alert", "start-app")
	w.restart()
	w.report()

	start := func() jobs.Job {
		j := e.runDef(w.id, hooked(testDefinition("/data"), appHooks()))
		w.poll()
		w.start(j.ID.String())
		w.report()
		return j
	}
	at := e.clock.Now().Add(-time.Minute)
	run := func(name string, phase syncrv1.HookPhase, st syncrv1.HookStatus) *syncrv1.HookRun {
		return &syncrv1.HookRun{
			Name: name, Phase: phase, Status: st, StartedAt: timestamppb.New(at), Duration: durationpb.New(1500 * time.Millisecond),
			Output: "output of " + name,
		}
	}
	output := &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticBackup{ResticBackup: &syncrv1.ResticBackupOutput{SnapshotId: "1a2b3c4d"}}}

	// A job that succeeded with its hooks.
	ok := start()
	w.finishWith(&syncrv1.JobResult{
		JobId: ok.ID.String(), Status: syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED, Output: output,
		HookRuns: []*syncrv1.HookRun{
			run("stop-app", syncrv1.HookPhase_HOOK_PHASE_BEFORE, syncrv1.HookStatus_HOOK_STATUS_SUCCEEDED),
			run("start-app", syncrv1.HookPhase_HOOK_PHASE_AFTER, syncrv1.HookStatus_HOOK_STATUS_SUCCEEDED),
		},
	})
	w.report()
	got := e.job(ok.ID)
	want := []jobs.HookRun{
		{Name: "stop-app", Phase: domain.HookBefore, Status: domain.HookSucceeded, StartedAt: at, Duration: 1500 * time.Millisecond, Output: "output of stop-app"},
		{Name: "start-app", Phase: domain.HookAfter, Status: domain.HookSucceeded, StartedAt: at, Duration: 1500 * time.Millisecond, Output: "output of start-app"},
	}
	if got.Status != domain.JobSucceeded || !sameRuns(got.HookRuns, want) || len(got.Output) == 0 {
		t.Errorf("succeeded job = %s, hook runs %+v, output %d bytes\nwant %+v", got.Status, got.HookRuns, len(got.Output), want)
	}

	// A hook failed the job after its operation succeeded: the snapshot it
	// wrote and its warning are still the job's.
	failed := start()
	timedOut := run("start-app", syncrv1.HookPhase_HOOK_PHASE_AFTER, syncrv1.HookStatus_HOOK_STATUS_TIMED_OUT)
	timedOut.ExitCode, timedOut.Message, timedOut.OutputTruncated = -1, "timed out after 10m0s", true
	w.finishWith(&syncrv1.JobResult{
		JobId: failed.ID.String(), Status: syncrv1.JobResultStatus_JOB_RESULT_STATUS_FAILED,
		Error:    &syncrv1.JobError{Code: syncrv1.JobErrorCode_JOB_ERROR_CODE_HOOK_FAILED, Message: `hook "start-app" (after) timed out after 10m0s`},
		Output:   output,
		Warnings: []string{"2 items could not be read"},
		HookRuns: []*syncrv1.HookRun{timedOut},
	})
	w.report()
	got = e.job(failed.ID)
	if got.Status != domain.JobFailed || got.Error.Code != jobs.CodeHookFailed ||
		got.Error.Message != `hook "start-app" (after) timed out after 10m0s` {
		t.Errorf("failed job = %s %q %q", got.Status, got.Error.Code, got.Error.Message)
	}
	var stored syncrv1.JobOutput
	if err := proto.Unmarshal(got.Output, &stored); err != nil || stored.GetResticBackup().GetSnapshotId() != "1a2b3c4d" {
		t.Errorf("failed job output = %v (%v), want the operation's", &stored, err)
	}
	if !slices.Equal(got.Warnings, []string{"2 items could not be read"}) {
		t.Errorf("failed job warnings = %v, want the operation's", got.Warnings)
	}
	if len(got.HookRuns) != 1 || got.HookRuns[0].Status != domain.HookTimedOut || got.HookRuns[0].ExitCode != -1 ||
		got.HookRuns[0].Message != "timed out after 10m0s" || !got.HookRuns[0].OutputTruncated {
		t.Errorf("failed job hook runs = %+v", got.HookRuns)
	}

	// A before hook failed: nothing ran, nothing to keep but the runs.
	skipped := start()
	w.finishWith(&syncrv1.JobResult{
		JobId: skipped.ID.String(), Status: syncrv1.JobResultStatus_JOB_RESULT_STATUS_FAILED, ExitCode: -1,
		Error:    &syncrv1.JobError{Code: syncrv1.JobErrorCode_JOB_ERROR_CODE_HOOK_FAILED, Message: `hook "stop-app" (before) failed: exited with code 3`},
		HookRuns: []*syncrv1.HookRun{run("stop-app", syncrv1.HookPhase_HOOK_PHASE_BEFORE, syncrv1.HookStatus_HOOK_STATUS_FAILED)},
	})
	w.report()
	if got := e.job(skipped.ID); got.Status != domain.JobFailed || got.Output != nil || got.Warnings != nil || len(got.HookRuns) != 1 {
		t.Errorf("job whose before hook failed = %s, output %v, warnings %v, runs %+v", got.Status, got.Output, got.Warnings, got.HookRuns)
	}

	// A cancelled job keeps the runs of the hooks that put things back.
	cancelled := start()
	if _, err := e.jobs.Cancel(context.Background(), testActor(), cancelled.ID); err != nil {
		t.Fatal(err)
	}
	w.finishWith(&syncrv1.JobResult{
		JobId: cancelled.ID.String(), Status: syncrv1.JobResultStatus_JOB_RESULT_STATUS_CANCELLED,
		Error: &syncrv1.JobError{Code: syncrv1.JobErrorCode_JOB_ERROR_CODE_CANCELLED, Message: "job cancelled by the controller"},
		HookRuns: []*syncrv1.HookRun{
			run("stop-app", syncrv1.HookPhase_HOOK_PHASE_BEFORE, syncrv1.HookStatus_HOOK_STATUS_CANCELLED),
			run("start-app", syncrv1.HookPhase_HOOK_PHASE_AFTER, syncrv1.HookStatus_HOOK_STATUS_SUCCEEDED),
		},
	})
	w.report()
	if got := e.job(cancelled.ID); got.Status != domain.JobCancelled || len(got.HookRuns) != 2 || got.HookRuns[0].Status != domain.HookCancelled {
		t.Errorf("cancelled job = %s, runs %+v", got.Status, got.HookRuns)
	}
}

// TestHookRunsAreBounded: what a worker reports about its hooks cannot
// bloat a job's record or break its storage.
func TestHookRunsAreBounded(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.newProbeWorker(e.enroll("nas"))
	w.report()
	j := e.run(w.id)
	w.poll()
	w.start(j.ID.String())
	w.report()

	var runs []*syncrv1.HookRun
	for range 100 {
		runs = append(runs, &syncrv1.HookRun{
			Name:    strings.Repeat("n", 200),
			Phase:   99,
			Status:  77,
			Message: strings.Repeat("é", 2000) + "\xff",
			Output:  strings.Repeat("o", 10000) + "\xff\xfe",
			// Neither a time nor a duration.
			StartedAt: &timestamppb.Timestamp{Seconds: 1, Nanos: -5},
			Duration:  &durationpb.Duration{Seconds: -3},
		})
	}
	w.finishWith(&syncrv1.JobResult{JobId: j.ID.String(), Status: syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED, HookRuns: runs})
	w.report()
	got := e.job(j.ID)
	if got.Status != domain.JobSucceeded || len(got.HookRuns) != jobs.MaxHookRuns {
		t.Fatalf("job = %s with %d hook runs, want %d", got.Status, len(got.HookRuns), jobs.MaxHookRuns)
	}
	r := got.HookRuns[0]
	if len(r.Name) != domain.MaxHookNameLen || len(r.Message) > domain.MaxHookMessageBytes || len(r.Output) != domain.MaxHookOutputBytes || !r.OutputTruncated {
		t.Errorf("run = name %d bytes, message %d bytes, output %d bytes, truncated %v", len(r.Name), len(r.Message), len(r.Output), r.OutputTruncated)
	}
	if !utf8.ValidString(r.Message) || !utf8.ValidString(r.Output) {
		t.Error("the stored texts are not valid UTF-8")
	}
	if r.Phase != "" || r.Status != "" || !r.StartedAt.IsZero() || r.Duration != 0 {
		t.Errorf("run = phase %q, status %q, started %v, duration %v; want what is unknown left empty", r.Phase, r.Status, r.StartedAt, r.Duration)
	}
}

// The controller reads the code workers report by its link name.
func TestHookFailedCodeName(t *testing.T) {
	t.Parallel()
	if got := link.ErrorCodeName(syncrv1.JobErrorCode_JOB_ERROR_CODE_HOOK_FAILED); got != jobs.CodeHookFailed {
		t.Errorf("link name of HOOK_FAILED = %q, jobs.CodeHookFailed = %q", got, jobs.CodeHookFailed)
	}
}

// sameRuns compares hook runs field by field, times by instant.
func sameRuns(a, b []jobs.HookRun) bool {
	return slices.EqualFunc(a, b, func(x, y jobs.HookRun) bool {
		if !x.StartedAt.Equal(y.StartedAt) {
			return false
		}
		x.StartedAt, y.StartedAt = time.Time{}, time.Time{}
		return x == y
	})
}
