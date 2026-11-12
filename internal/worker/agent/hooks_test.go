package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// hookScript is the behavior of a fake hook: it returns the run's status
// and message. It may block and must return when ctx is done if it means to
// be stoppable.
type hookScript func(ctx context.Context, env map[string]string) (syncrv1.HookStatus, string)

// hookCall is one recorded run of a fake hook.
type hookCall struct {
	name  string
	phase syncrv1.HookPhase
	env   map[string]string
}

// fakeHooks implements Hooks with scripts instead of commands. Hooks
// without a script succeed at once.
type fakeHooks struct {
	defined []string

	mu      sync.Mutex
	scripts map[string]hookScript
	calls   []hookCall
}

func newFakeHooks(defined ...string) *fakeHooks {
	return &fakeHooks{defined: defined, scripts: map[string]hookScript{}}
}

func (f *fakeHooks) set(name string, s hookScript) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts[name] = s
}

func (f *fakeHooks) Infos() []*syncrv1.HookInfo {
	out := make([]*syncrv1.HookInfo, 0, len(f.defined))
	for _, name := range f.defined {
		out = append(out, &syncrv1.HookInfo{Name: name, Description: "the " + name + " hook", Timeout: durationpb.New(time.Minute)})
	}
	return out
}

func (f *fakeHooks) Check(h *syncrv1.JobHooks) error {
	for _, names := range [][]string{h.GetBefore(), h.GetOnSuccess(), h.GetOnFailure(), h.GetAfter()} {
		for _, name := range names {
			if !slices.Contains(f.defined, name) {
				return fmt.Errorf("this worker defines no hook named %q", name)
			}
		}
	}
	return nil
}

func (f *fakeHooks) Run(ctx context.Context, name string, phase syncrv1.HookPhase, env map[string]string) *syncrv1.HookRun {
	f.mu.Lock()
	f.calls = append(f.calls, hookCall{name: name, phase: phase, env: env})
	s := f.scripts[name]
	f.mu.Unlock()
	run := &syncrv1.HookRun{
		Name: name, Phase: phase, Status: syncrv1.HookStatus_HOOK_STATUS_SUCCEEDED,
		StartedAt: timestamppb.Now(), Duration: durationpb.New(time.Second), Output: "output of " + name,
	}
	if s != nil {
		run.Status, run.Message = s(ctx, env)
	}
	if run.Status != syncrv1.HookStatus_HOOK_STATUS_SUCCEEDED {
		run.ExitCode = 1
	}
	return run
}

// ran renders the recorded runs: "before:stop-app after:start-app".
func (f *fakeHooks) ran() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		parts = append(parts, phaseName(c.phase)+":"+c.name)
	}
	return strings.Join(parts, " ")
}

// envOf returns the environment of the only run of name.
func (f *fakeHooks) envOf(t *testing.T, name string) map[string]string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var found map[string]string
	for _, c := range f.calls {
		if c.name == name {
			if found != nil {
				t.Fatalf("hook %s ran twice", name)
			}
			found = c.env
		}
	}
	if found == nil {
		t.Fatalf("hook %s did not run", name)
	}
	return found
}

func phaseName(p syncrv1.HookPhase) string {
	return strings.ToLower(strings.TrimPrefix(p.String(), "HOOK_PHASE_"))
}

// failing is a hook that exits unsuccessfully.
func failing(context.Context, map[string]string) (syncrv1.HookStatus, string) {
	return syncrv1.HookStatus_HOOK_STATUS_FAILED, "exited with code 3"
}

// blocking returns a hook that runs until release is closed, or until its
// context is done if it is stoppable.
func blocking(release <-chan struct{}) hookScript {
	return func(ctx context.Context, _ map[string]string) (syncrv1.HookStatus, string) {
		select {
		case <-release:
			return syncrv1.HookStatus_HOOK_STATUS_SUCCEEDED, ""
		case <-ctx.Done():
			return syncrv1.HookStatus_HOOK_STATUS_CANCELLED, "stopped: " + context.Cause(ctx).Error()
		}
	}
}

// hookedStartCmd starts job with hooks.
func hookedStartCmd(cmdID, jobID string, h *syncrv1.JobHooks) *syncrv1.Command {
	c := startCmd(cmdID, jobID)
	c.GetStartJob().GetSpec().Hooks = h
	return c
}

// runsOf renders a result's hook runs: "before:stop-app=SUCCEEDED".
func runsOf(r *syncrv1.JobResult) string {
	parts := make([]string, 0, len(r.GetHookRuns()))
	for _, run := range r.GetHookRuns() {
		parts = append(parts, fmt.Sprintf("%s:%s=%s", phaseName(run.GetPhase()), run.GetName(), strings.TrimPrefix(run.GetStatus().String(), "HOOK_STATUS_")))
	}
	return strings.Join(parts, " ")
}

// allPhases names one or two hooks in every phase.
func allPhases() *syncrv1.JobHooks {
	return &syncrv1.JobHooks{
		Before:    []string{"stop-app", "dump-db"},
		OnSuccess: []string{"ping", "tidy"},
		OnFailure: []string{"alert", "page"},
		After:     []string{"start-db", "start-app"},
	}
}

func allHooks() *fakeHooks {
	return newFakeHooks("stop-app", "dump-db", "ping", "tidy", "alert", "page", "start-db", "start-app")
}

// withOutput is a runner that succeeds with a kind-specific output and a
// warning.
func withOutput(_ context.Context, _ runner.Job, emit runner.EmitFunc) (runner.Result, error) {
	emit(runner.Progress{Bytes: 7})
	return runner.Result{
		ExitCode: 0, Stats: runner.Progress{Bytes: 7},
		Output:   &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticBackup{ResticBackup: &syncrv1.ResticBackupOutput{SnapshotId: "1a2b3c4d"}}},
		Warnings: []string{"2 items could not be read"},
	}, nil
}

func TestHelloListsTheHooks(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a := newAgent(t, newFakeRunner(succeed), Config{Hooks: newFakeHooks("dump-db", "start-app")})
		defer shutdown(t, a)
		evs, _ := a.Outbox().Pending(0, 0)
		hooks := evs[0].GetHello().GetHooks()
		if len(hooks) != 2 || hooks[0].GetName() != "dump-db" || hooks[0].GetDescription() != "the dump-db hook" ||
			hooks[0].GetTimeout().AsDuration() != time.Minute || hooks[1].GetName() != "start-app" {
			t.Errorf("Hello.hooks = %v", hooks)
		}

		none := newAgent(t, newFakeRunner(succeed), Config{})
		defer shutdown(t, none)
		evs, _ = none.Outbox().Pending(0, 0)
		if got := evs[0].GetHello().GetHooks(); len(got) != 0 {
			t.Errorf("Hello.hooks of a worker without hooks = %v", got)
		}
	})
}

func TestJobWithoutHooksRunsAsBefore(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		hooks := allHooks()
		a := newAgent(t, newFakeRunner(withOutput), Config{Hooks: hooks})
		defer shutdown(t, a)
		handle(t, a, startCmd("c1", "a"), hookedStartCmd("c2", "b", &syncrv1.JobHooks{}))
		synctest.Wait()
		for _, id := range []string{"a", "b"} {
			r := resultOf(t, a, id)
			if r.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED || len(r.GetHookRuns()) != 0 || r.GetOutput() == nil {
				t.Errorf("result of %s = %v", id, r)
			}
		}
		if got := hooks.ran(); got != "" {
			t.Errorf("hooks ran for jobs that name none: %s", got)
		}
	})
}

func TestUndefinedHookRejectsTheJob(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		for name, cfg := range map[string]Config{
			"another hook":           {Hooks: newFakeHooks("stop-app")},
			"a worker without hooks": {},
		} {
			fr := newFakeRunner(succeed)
			a := newAgent(t, fr, cfg)
			handle(t, a, hookedStartCmd("c1", "a", &syncrv1.JobHooks{Before: []string{"stop-app"}, After: []string{"start-app"}}))
			synctest.Wait()
			r := resultOf(t, a, "a")
			if r.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_FAILED ||
				r.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC ||
				!strings.Contains(r.GetError().GetMessage(), "hook") || r.GetStartedAt() != nil || len(r.GetHookRuns()) != 0 {
				t.Errorf("%s: result = %v", name, r)
			}
			// Nothing ran: not the job, not the hook the worker does define.
			if got := describe(events(a)); got != "result(a,FAILED)" || fr.runCount("a") != 0 {
				t.Errorf("%s: events = %s, runner ran %d times", name, got, fr.runCount("a"))
			}
			if h, ok := cfg.Hooks.(*fakeHooks); ok && h.ran() != "" {
				t.Errorf("%s: hooks ran: %s", name, h.ran())
			}
			shutdown(t, a)
		}
	})
}

func TestHooksRunAroundASuccessfulJob(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		hooks := allHooks()
		var order []string
		var mu sync.Mutex
		note := func(what string) {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, what)
		}
		for _, name := range hooks.defined {
			hooks.set(name, func(context.Context, map[string]string) (syncrv1.HookStatus, string) {
				note(name)
				return syncrv1.HookStatus_HOOK_STATUS_SUCCEEDED, ""
			})
		}
		fr := newFakeRunner(func(ctx context.Context, job runner.Job, emit runner.EmitFunc) (runner.Result, error) {
			note("operation")
			return withOutput(ctx, job, emit)
		})
		a := newAgent(t, fr, Config{Hooks: hooks})
		defer shutdown(t, a)

		handle(t, a, hookedStartCmd("c1", "0195f0a4-3b7c-7d1e-9f00-000000000001", allPhases()))
		synctest.Wait()

		if got, want := strings.Join(order, " "), "stop-app dump-db operation ping tidy start-db start-app"; got != want {
			t.Errorf("order:\n got %s\nwant %s", got, want)
		}
		r := resultOf(t, a, "0195f0a4-3b7c-7d1e-9f00-000000000001")
		if r.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED || r.GetError() != nil {
			t.Errorf("result = %v", r)
		}
		if got, want := runsOf(r), "before:stop-app=SUCCEEDED before:dump-db=SUCCEEDED on_success:ping=SUCCEEDED on_success:tidy=SUCCEEDED after:start-db=SUCCEEDED after:start-app=SUCCEEDED"; got != want {
			t.Errorf("hook runs:\n got %s\nwant %s", got, want)
		}
		if r.GetHookRuns()[0].GetOutput() != "output of stop-app" {
			t.Errorf("first run = %v, want the hook's output", r.GetHookRuns()[0])
		}
		if r.GetOutput().GetResticBackup().GetSnapshotId() != "1a2b3c4d" || len(r.GetWarnings()) != 1 || r.GetStats().GetBytes() != 7 {
			t.Errorf("result = %v, want the operation's output, warning and counters", r)
		}

		// What each hook is told about the job.
		for name, want := range map[string]map[string]string{
			"stop-app":  {EnvHookPhase: "before", EnvJobStatus: "running", EnvJobErrorCode: ""},
			"ping":      {EnvHookPhase: "on_success", EnvJobStatus: "succeeded", EnvJobErrorCode: ""},
			"start-app": {EnvHookPhase: "after", EnvJobStatus: "succeeded", EnvJobErrorCode: ""},
		} {
			env := hooks.envOf(t, name)
			want[EnvHookName] = name
			want[EnvJobID] = "0195f0a4-3b7c-7d1e-9f00-000000000001"
			want[EnvJobKind] = "rclone_transfer"
			if len(env) != len(want) {
				t.Errorf("%s: environment = %v, want exactly %v", name, env, want)
			}
			for k, v := range want {
				if got, ok := env[k]; !ok || got != v {
					t.Errorf("%s: %s = %q (set: %v), want %q", name, k, got, ok, v)
				}
			}
		}
	})
}

func TestFailingBeforeHookSkipsTheOperation(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		hooks := allHooks()
		hooks.set("stop-app", failing)
		hooks.set("alert", failing) // a cleanup hook that fails does not stop the others
		fr := newFakeRunner(succeed)
		a := newAgent(t, fr, Config{Hooks: hooks})
		defer shutdown(t, a)

		handle(t, a, hookedStartCmd("c1", "a", allPhases()))
		synctest.Wait()

		if got, want := hooks.ran(), "before:stop-app on_failure:alert on_failure:page after:start-db after:start-app"; got != want {
			t.Errorf("hooks ran:\n got %s\nwant %s", got, want)
		}
		if fr.runCount("a") != 0 {
			t.Error("the operation ran after its before hook failed")
		}
		r := resultOf(t, a, "a")
		if r.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_FAILED ||
			r.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_HOOK_FAILED ||
			r.GetError().GetMessage() != `hook "stop-app" (before) failed: exited with code 3` {
			t.Errorf("result = %v", r)
		}
		if r.GetExitCode() != -1 || r.GetOutput() != nil || r.GetStartedAt() == nil {
			t.Errorf("result = %v, want no tool exit code, no output and a start time", r)
		}
		if got, want := runsOf(r), "before:stop-app=FAILED on_failure:alert=FAILED on_failure:page=SUCCEEDED after:start-db=SUCCEEDED after:start-app=SUCCEEDED"; got != want {
			t.Errorf("hook runs:\n got %s\nwant %s", got, want)
		}
		// The job started (its hook ran), so it is never queued again.
		if got, want := describe(events(a)), "start(a) result(a,FAILED)"; got != want {
			t.Errorf("events = %s, want %s", got, want)
		}
		env := hooks.envOf(t, "page")
		if env[EnvJobStatus] != "failed" || env[EnvJobErrorCode] != "hook_failed" || env[EnvHookPhase] != "on_failure" {
			t.Errorf("on_failure environment = %v", env)
		}
		if env := hooks.envOf(t, "start-app"); env[EnvJobStatus] != "failed" || env[EnvJobErrorCode] != "hook_failed" {
			t.Errorf("after environment = %v", env)
		}
	})
}

func TestFailingOperationRunsTheFailureHooks(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		hooks := allHooks()
		hooks.set("start-db", failing) // the job has failed already: its error stays
		fr := newFakeRunner(func(_ context.Context, _ runner.Job, _ runner.EmitFunc) (runner.Result, error) {
			return runner.Result{ExitCode: 1, Stats: runner.Progress{Bytes: 3}},
				runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "rclone exited with code 1")
		})
		a := newAgent(t, fr, Config{Hooks: hooks})
		defer shutdown(t, a)

		handle(t, a, hookedStartCmd("c1", "a", allPhases()))
		synctest.Wait()

		if got, want := hooks.ran(), "before:stop-app before:dump-db on_failure:alert on_failure:page after:start-db after:start-app"; got != want {
			t.Errorf("hooks ran:\n got %s\nwant %s", got, want)
		}
		r := resultOf(t, a, "a")
		if r.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_FAILED ||
			r.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED ||
			r.GetError().GetMessage() != "rclone exited with code 1" || r.GetExitCode() != 1 || r.GetStats().GetBytes() != 3 {
			t.Errorf("result = %v, want the operation's own failure", r)
		}
		if !strings.Contains(runsOf(r), "after:start-db=FAILED after:start-app=SUCCEEDED") {
			t.Errorf("hook runs = %s, want the failed after hook reported and the next one run", runsOf(r))
		}
		if env := hooks.envOf(t, "alert"); env[EnvJobStatus] != "failed" || env[EnvJobErrorCode] != "execution_failed" {
			t.Errorf("on_failure environment = %v", env)
		}
	})
}

func TestFailingSuccessHookFailsTheJobButKeepsItsOutput(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		hooks := allHooks()
		hooks.set("ping", func(context.Context, map[string]string) (syncrv1.HookStatus, string) {
			return syncrv1.HookStatus_HOOK_STATUS_TIMED_OUT, "timed out after 10m0s"
		})
		a := newAgent(t, newFakeRunner(withOutput), Config{Hooks: hooks})
		defer shutdown(t, a)

		handle(t, a, hookedStartCmd("c1", "a", allPhases()))
		synctest.Wait()

		if got, want := hooks.ran(), "before:stop-app before:dump-db on_success:ping on_failure:alert on_failure:page after:start-db after:start-app"; got != want {
			t.Errorf("hooks ran:\n got %s\nwant %s", got, want)
		}
		r := resultOf(t, a, "a")
		if r.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_FAILED ||
			r.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_HOOK_FAILED ||
			r.GetError().GetMessage() != `hook "ping" (on_success) timed out after 10m0s` {
			t.Errorf("result = %v", r)
		}
		// The backup exists: its snapshot is still reported.
		if r.GetOutput().GetResticBackup().GetSnapshotId() != "1a2b3c4d" || len(r.GetWarnings()) != 1 || r.GetExitCode() != 0 {
			t.Errorf("result = %v, want the operation's output, warning and exit code", r)
		}
	})
}

func TestFailingAfterHookFailsASuccessfulJob(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		hooks := allHooks()
		hooks.set("start-db", failing)
		hooks.set("start-app", failing)
		a := newAgent(t, newFakeRunner(withOutput), Config{Hooks: hooks})
		defer shutdown(t, a)

		handle(t, a, hookedStartCmd("c1", "a", allPhases()))
		synctest.Wait()

		// Every after hook is tried, and the failure hooks are not: they
		// have had their turn before the after hooks.
		if got, want := hooks.ran(), "before:stop-app before:dump-db on_success:ping on_success:tidy after:start-db after:start-app"; got != want {
			t.Errorf("hooks ran:\n got %s\nwant %s", got, want)
		}
		r := resultOf(t, a, "a")
		if r.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_FAILED ||
			r.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_HOOK_FAILED ||
			r.GetError().GetMessage() != `hook "start-db" (after) failed: exited with code 3` {
			t.Errorf("result = %v, want the first failed after hook named", r)
		}
		if r.GetOutput().GetResticBackup().GetSnapshotId() != "1a2b3c4d" {
			t.Errorf("result = %v, want the operation's output", r)
		}
		// The after hooks ran for a job that had succeeded.
		if env := hooks.envOf(t, "start-app"); env[EnvJobStatus] != "succeeded" || env[EnvJobErrorCode] != "" {
			t.Errorf("after environment = %v", env)
		}
	})
}

func TestCancelDuringABeforeHook(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		hooks := allHooks()
		hooks.set("dump-db", blocking(nil))
		fr := newFakeRunner(succeed)
		a := newAgent(t, fr, Config{Hooks: hooks})
		defer shutdown(t, a)

		handle(t, a, hookedStartCmd("c1", "a", allPhases()))
		synctest.Wait()
		if got := describe(events(a)); got != "start(a)" {
			t.Fatalf("events while the hook runs = %s", got)
		}
		handle(t, a, cancelCmd("c2", "a"))
		synctest.Wait()

		// The operation and the failure hooks are skipped (a cancel is not
		// a failure); the after hooks put things back.
		if got, want := hooks.ran(), "before:stop-app before:dump-db after:start-db after:start-app"; got != want {
			t.Errorf("hooks ran:\n got %s\nwant %s", got, want)
		}
		if fr.runCount("a") != 0 {
			t.Error("the operation ran after the cancel")
		}
		r := resultOf(t, a, "a")
		if r.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_CANCELLED ||
			r.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_CANCELLED || r.GetError().GetMessage() != ErrCancelled.Error() {
			t.Errorf("result = %v", r)
		}
		if got, want := runsOf(r), "before:stop-app=SUCCEEDED before:dump-db=CANCELLED after:start-db=SUCCEEDED after:start-app=SUCCEEDED"; got != want {
			t.Errorf("hook runs:\n got %s\nwant %s", got, want)
		}
		if env := hooks.envOf(t, "start-app"); env[EnvJobStatus] != "cancelled" || env[EnvJobErrorCode] != "cancelled" {
			t.Errorf("after environment = %v", env)
		}
	})
}

func TestCancelDuringTheOperationStillRunsTheAfterHooks(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		hooks := allHooks()
		a := newAgent(t, newFakeRunner(gate(nil, time.Second)), Config{Hooks: hooks})
		defer shutdown(t, a)

		handle(t, a, hookedStartCmd("c1", "a", allPhases()))
		synctest.Wait()
		handle(t, a, cancelCmd("c2", "a"))
		time.Sleep(time.Minute)

		if got, want := hooks.ran(), "before:stop-app before:dump-db after:start-db after:start-app"; got != want {
			t.Errorf("hooks ran:\n got %s\nwant %s", got, want)
		}
		r := resultOf(t, a, "a")
		if r.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_CANCELLED || r.GetStats().GetBytes() != 10 || len(r.GetHookRuns()) != 4 {
			t.Errorf("result = %v", r)
		}
	})
}

// A cancel that arrives while the hooks that clean up run changes nothing:
// they finish, and the job keeps the outcome it had.
func TestCancelDoesNotStopCleanupHooks(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		hooks := allHooks()
		release := make(chan struct{})
		hooks.set("start-db", blocking(release))
		a := newAgent(t, newFakeRunner(withOutput), Config{Hooks: hooks})
		defer shutdown(t, a)

		handle(t, a, hookedStartCmd("c1", "a", allPhases()))
		synctest.Wait()
		handle(t, a, cancelCmd("c2", "a"))
		synctest.Wait()
		if got := describe(events(a)); strings.Contains(got, "result") {
			t.Fatalf("the cancel ended the job while its after hook ran: %s", got)
		}
		close(release)
		synctest.Wait()

		r := resultOf(t, a, "a")
		if r.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED {
			t.Errorf("result = %v, want the success decided before the cancel", r)
		}
		if got, want := runsOf(r), "before:stop-app=SUCCEEDED before:dump-db=SUCCEEDED on_success:ping=SUCCEEDED on_success:tidy=SUCCEEDED after:start-db=SUCCEEDED after:start-app=SUCCEEDED"; got != want {
			t.Errorf("hook runs:\n got %s\nwant %s", got, want)
		}
	})
}

func TestTimeLimitCountsBeforeHooksAndRunsTheFailureHooks(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var timers fakeTimers
		hooks := allHooks()
		hooks.set("dump-db", blocking(nil))
		release := make(chan struct{})
		hooks.set("alert", blocking(release))
		fr := newFakeRunner(succeed)
		a := newAgent(t, fr, Config{Hooks: hooks, AfterFunc: timers.afterFunc})
		defer shutdown(t, a)

		c := hookedStartCmd("c1", "a", allPhases())
		c.GetStartJob().GetSpec().MaxRuntime = durationpb.New(time.Hour)
		handle(t, a, c)
		synctest.Wait()
		tm := timers.only(t) // armed when the job started, before its hooks
		timers.fire(tm)
		synctest.Wait()

		// The limit's timer is over once the cleanup hooks run.
		if !timers.isStopped(tm) && !tm.fired {
			t.Error("the time limit is still armed while the cleanup hooks run")
		}
		close(release)
		synctest.Wait()

		if got, want := hooks.ran(), "before:stop-app before:dump-db on_failure:alert on_failure:page after:start-db after:start-app"; got != want {
			t.Errorf("hooks ran:\n got %s\nwant %s", got, want)
		}
		r := resultOf(t, a, "a")
		if r.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_FAILED ||
			r.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_TIME_LIMIT || fr.runCount("a") != 0 {
			t.Errorf("result = %v, runner ran %d times", r, fr.runCount("a"))
		}
		if env := hooks.envOf(t, "page"); env[EnvJobStatus] != "failed" || env[EnvJobErrorCode] != "time_limit" {
			t.Errorf("on_failure environment = %v", env)
		}
	})
}

// The limit does not apply to the hooks that clean up: once they run, its
// timer is stopped and the job's outcome stands.
func TestTimeLimitIsDisarmedForCleanupHooks(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var timers fakeTimers
		hooks := allHooks()
		release := make(chan struct{})
		hooks.set("start-db", blocking(release))
		a := newAgent(t, newFakeRunner(withOutput), Config{Hooks: hooks, AfterFunc: timers.afterFunc})
		defer shutdown(t, a)

		c := hookedStartCmd("c1", "a", allPhases())
		c.GetStartJob().GetSpec().MaxRuntime = durationpb.New(time.Hour)
		handle(t, a, c)
		synctest.Wait()
		tm := timers.only(t)
		if !timers.isStopped(tm) {
			t.Fatal("the time limit is still armed while an after hook runs")
		}
		timers.fire(tm) // does nothing: it was stopped
		close(release)
		synctest.Wait()
		if r := resultOf(t, a, "a"); r.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED {
			t.Errorf("result = %v", r)
		}
	})
}

func TestShutdownRunsTheAfterHooksBeforeReporting(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		hooks := allHooks()
		hooks.set("start-app", func(context.Context, map[string]string) (syncrv1.HookStatus, string) {
			time.Sleep(3 * time.Second) // docker start takes a moment
			return syncrv1.HookStatus_HOOK_STATUS_SUCCEEDED, ""
		})
		a := newAgent(t, newFakeRunner(gate(nil, time.Second)), Config{Hooks: hooks})

		handle(t, a, hookedStartCmd("c1", "a", allPhases()))
		synctest.Wait()
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		if err := a.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		if took := time.Since(start); took != 4*time.Second {
			t.Errorf("Shutdown returned after %s, want the tool's stop and the after hook (4s)", took)
		}

		if got, want := hooks.ran(), "before:stop-app before:dump-db after:start-db after:start-app"; got != want {
			t.Errorf("hooks ran:\n got %s\nwant %s", got, want)
		}
		r := resultOf(t, a, "a")
		if r.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_CANCELLED ||
			r.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_WORKER_SHUTDOWN || len(r.GetHookRuns()) != 4 {
			t.Errorf("result = %v", r)
		}
		if env := hooks.envOf(t, "start-app"); env[EnvJobStatus] != "cancelled" || env[EnvJobErrorCode] != "worker_shutdown" {
			t.Errorf("after environment = %v", env)
		}
	})
}

// An after hook that outlasts the shutdown's patience is left to the
// worker's exit: Shutdown reports that jobs are still running, and no
// result is made up for the job.
func TestShutdownDoesNotWaitForeverForAfterHooks(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		hooks := allHooks()
		release := make(chan struct{})
		hooks.set("start-app", blocking(release))
		a := newAgent(t, newFakeRunner(gate(nil, time.Second)), Config{Hooks: hooks})

		handle(t, a, hookedStartCmd("c1", "a", allPhases()))
		synctest.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		if err := a.Shutdown(ctx); err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown = %v, want it to give up at its deadline", err)
		}
		if got := describe(events(a)); strings.Contains(got, "result") {
			t.Errorf("events = %s, want no result while the hook runs", got)
		}
		close(release)
		shutdown(t, a)
		if r := resultOf(t, a, "a"); r.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_WORKER_SHUTDOWN {
			t.Errorf("result once the hook ended = %v", r)
		}
	})
}

// Hooks run in the job's slot: the next job waits for them too.
func TestHooksHoldTheRunSlot(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		hooks := allHooks()
		release := make(chan struct{})
		hooks.set("start-app", blocking(release))
		a := newAgent(t, newFakeRunner(succeed), Config{Hooks: hooks, MaxConcurrentJobs: 1})
		defer shutdown(t, a)

		handle(t, a, hookedStartCmd("c1", "a", &syncrv1.JobHooks{After: []string{"start-app"}}), startCmd("c2", "b"))
		synctest.Wait()
		if got, want := stateOf(a.State()), "a(running) b(queued)"; got != want {
			t.Fatalf("state while the after hook runs = %s, want %s", got, want)
		}
		close(release)
		synctest.Wait()
		if got, want := describe(events(a)), "start(a) progress(a,1) result(a,SUCCEEDED) start(b) progress(b,1) result(b,SUCCEEDED)"; got != want {
			t.Errorf("events:\n got %s\nwant %s", got, want)
		}
	})
}

// brokenHooks misbehaves: it panics for "panics" and reports nothing for
// anything else.
type brokenHooks struct{ fakeHooks }

func (b *brokenHooks) Run(_ context.Context, name string, _ syncrv1.HookPhase, _ map[string]string) *syncrv1.HookRun {
	if name == "panics" {
		panic("boom")
	}
	return nil
}

func TestBrokenHookRunnerFailsTheJobInsteadOfTheWorker(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		hooks := &brokenHooks{fakeHooks: *newFakeHooks("panics", "silent")}
		fr := newFakeRunner(succeed)
		a := newAgent(t, fr, Config{Hooks: hooks})
		defer shutdown(t, a)

		handle(t, a,
			hookedStartCmd("c1", "a", &syncrv1.JobHooks{Before: []string{"panics"}}),
			hookedStartCmd("c2", "b", &syncrv1.JobHooks{After: []string{"silent"}}))
		synctest.Wait()

		ra := resultOf(t, a, "a")
		if ra.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_HOOK_FAILED ||
			!strings.Contains(ra.GetError().GetMessage(), "the hook runner crashed") || fr.runCount("a") != 0 {
			t.Errorf("result of a = %v", ra)
		}
		rb := resultOf(t, a, "b")
		if rb.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_HOOK_FAILED || len(rb.GetHookRuns()) != 1 ||
			rb.GetHookRuns()[0].GetStatus() != syncrv1.HookStatus_HOOK_STATUS_FAILED || rb.GetHookRuns()[0].GetExitCode() != -1 {
			t.Errorf("result of b = %v", rb)
		}
	})
}

// Whatever the controller sends as a job ID, a hook's environment holds
// only plain values.
func TestHookEnvironmentHoldsOnlyPlainValues(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"0195f0a4-3b7c-7d1e-9f00-000000000001": "0195f0a4-3b7c-7d1e-9f00-000000000001",
		"restic_backup":                        "restic_backup",
		"a.b_c-D9":                             "a.b_c-D9",
		"":                                     "",
		"a b":                                  "",
		"$(reboot)":                            "",
		"a;b":                                  "",
		"a\nb":                                 "",
		"é":                                    "",
		strings.Repeat("a", 129):               "",
	} {
		if got := plainValue(in); got != want {
			t.Errorf("plainValue(%q) = %q, want %q", in, got, want)
		}
	}
	synctest.Test(t, func(t *testing.T) {
		hooks := newFakeHooks("stop-app")
		a := newAgent(t, newFakeRunner(succeed), Config{Hooks: hooks})
		defer shutdown(t, a)
		handle(t, a, hookedStartCmd("c1", "$(reboot); x", &syncrv1.JobHooks{Before: []string{"stop-app"}}))
		synctest.Wait()
		env := hooks.envOf(t, "stop-app")
		if env[EnvJobID] != "" {
			t.Errorf("%s = %q, want it left empty", EnvJobID, env[EnvJobID])
		}
		for k, v := range env {
			if plainValue(v) != v {
				t.Errorf("%s = %q is not a plain value", k, v)
			}
		}
	})
}
