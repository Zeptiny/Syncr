package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// Hooks runs the worker's hooks: the commands its operator defined, which a
// job names in JobSpec.hooks. *hooks.Set (internal/worker/hooks) implements
// it; the agent decides which hooks run when, and what their outcome means
// for the job (see "Hooks" in the package documentation).
//
// Implementations must be safe for concurrent use: jobs run at once.
type Hooks interface {
	// Infos describes the hooks the worker defines, for the Hello event.
	Infos() []*syncrv1.HookInfo

	// Check returns why a job naming h cannot run on this worker (a hook it
	// does not define, a list that is too long), as a message for the job's
	// owner; nil when every hook can be run.
	Check(h *syncrv1.JobHooks) error

	// Run runs the hook name once for phase, with env added to its
	// environment, and blocks until it has ended. It returns how the run
	// went and never nil. When ctx is done the hook is stopped and the run
	// is CANCELLED.
	Run(ctx context.Context, name string, phase syncrv1.HookPhase, env map[string]string) *syncrv1.HookRun
}

// The environment variables that tell a hook about the job it runs for.
// Every value is made of ASCII letters, digits, ".", "_" and "-" only (a
// value that is not is left empty), so a script can use them without
// quoting worries: nothing a user of the controller types ends up in them.
const (
	// EnvHookName is the hook's own name.
	EnvHookName = "SYNCR_HOOK_NAME"
	// EnvHookPhase is the phase the hook runs in: "before", "on_success",
	// "on_failure" or "after".
	EnvHookPhase = "SYNCR_HOOK_PHASE"
	// EnvJobID is the job's ID (a UUID).
	EnvJobID = "SYNCR_JOB_ID"
	// EnvJobKind is the job's kind, e.g. "restic_backup".
	EnvJobKind = "SYNCR_JOB_KIND"
	// EnvJobStatus is where the job stands: "running" for a before hook;
	// afterwards "succeeded", "failed" or "cancelled" (a cancel, or the
	// worker shutting down).
	EnvJobStatus = "SYNCR_JOB_STATUS"
	// EnvJobErrorCode is why the job failed or was cancelled, as the
	// controller shows it (e.g. "execution_failed", "hook_failed",
	// "time_limit", "cancelled", "worker_shutdown"); empty otherwise.
	EnvJobErrorCode = "SYNCR_JOB_ERROR_CODE"
)

// Job statuses a hook reads in EnvJobStatus.
const (
	hookJobRunning   = "running"
	hookJobSucceeded = "succeeded"
	hookJobFailed    = "failed"
	hookJobCancelled = "cancelled"
)

// verdict is what a job amounts to at some point of its run: the status its
// result will report, unless a later step changes it.
type verdict struct {
	status syncrv1.JobResultStatus
	// code and msg say why, for FAILED.
	code syncrv1.JobErrorCode
	msg  string
	// cause is the cancellation cause, for CANCELLED.
	cause error
}

func succeeded() verdict {
	return verdict{status: syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED}
}

func failed(code syncrv1.JobErrorCode, msg string) verdict {
	return verdict{status: syncrv1.JobResultStatus_JOB_RESULT_STATUS_FAILED, code: code, msg: msg}
}

func (v verdict) ok() bool { return v.status == syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED }

// errorCode is the code the result will carry: v's own for FAILED, the
// cancellation's for CANCELLED, UNSPECIFIED for SUCCEEDED.
func (v verdict) errorCode() syncrv1.JobErrorCode {
	switch v.status {
	case syncrv1.JobResultStatus_JOB_RESULT_STATUS_FAILED:
		return v.code
	case syncrv1.JobResultStatus_JOB_RESULT_STATUS_CANCELLED:
		return cancelCode(v.cause)
	}
	return syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSPECIFIED
}

// stopped is the verdict of a job whose context is done: FAILED with
// TIME_LIMIT when the time limit ended it, CANCELLED otherwise (a CancelJob
// or the worker shutting down).
func stopped(ctx context.Context) verdict {
	cause := context.Cause(ctx)
	if errors.Is(cause, ErrTimeLimit) {
		return failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_TIME_LIMIT, cause.Error())
	}
	return verdict{status: syncrv1.JobResultStatus_JOB_RESULT_STATUS_CANCELLED, cause: cause}
}

// operationVerdict is the verdict of a runner's return for a job with
// context ctx.
func operationVerdict(ctx context.Context, err error) verdict {
	switch {
	case errors.Is(err, errRunnerPanicked):
		return failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "internal worker error: the job runner crashed")
	case err == nil:
		return succeeded()
	case ctx.Err() != nil:
		return stopped(ctx)
	}
	return failed(runner.CodeOf(err), err.Error())
}

// hookVerdict is the verdict of a job after its hook run did not succeed
// in a phase that a failing hook decides (before, on_success, after): the
// job's own stop when ctx is done (the hook was stopped with it), else
// HOOK_FAILED naming the hook.
func hookVerdict(ctx context.Context, run *syncrv1.HookRun) verdict {
	if ctx.Err() != nil {
		return stopped(ctx)
	}
	what := "failed: " + run.GetMessage()
	switch run.GetStatus() {
	case syncrv1.HookStatus_HOOK_STATUS_TIMED_OUT:
		what = run.GetMessage() // "timed out after 10m0s"
	case syncrv1.HookStatus_HOOK_STATUS_CANCELLED:
		what = "was stopped"
	}
	return failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_HOOK_FAILED,
		fmt.Sprintf("hook %q (%s) %s", run.GetName(), link.HookPhaseName(run.GetPhase()), what))
}

// hookRunOK reports whether run ended successfully.
func hookRunOK(run *syncrv1.HookRun) bool {
	return run.GetStatus() == syncrv1.HookStatus_HOOK_STATUS_SUCCEEDED
}

// checkHooks rejects a spec whose hooks this worker cannot run.
func (a *Agent) checkHooks(spec *syncrv1.JobSpec) error {
	h := spec.GetHooks()
	if !link.HasHooks(h) {
		return nil
	}
	if a.hooks == nil {
		return errors.New("the job names hooks, but this worker defines none: add them to the worker's configuration (hooks) or remove them from the job")
	}
	return a.hooks.Check(h)
}

// runHooks runs the hooks j names for phase, in order, and returns their
// runs with the first that did not succeed (nil if all did). With
// stopAtFailure the hooks after that one are skipped.
//
// ctx is the job's context for the phases a cancel or the time limit stops
// (before, on_success) and one that outlives it for the hooks that clean up
// (on_failure, after). at is where the job stands, for the hooks'
// environment.
func (a *Agent) runHooks(ctx context.Context, j *job, phase syncrv1.HookPhase, at verdict, stopAtFailure bool) (runs []*syncrv1.HookRun, firstFailed *syncrv1.HookRun) {
	for _, name := range link.HooksOf(j.spec.GetHooks(), phase) {
		run := a.runHook(ctx, j, name, phase, at)
		runs = append(runs, run)
		a.log.Info("hook finished", "job_id", j.id, "hook", run.GetName(), "phase", link.HookPhaseName(phase),
			"status", link.HookStatusName(run.GetStatus()), "exit_code", int(run.GetExitCode()),
			"duration", run.GetDuration().AsDuration().String())
		if hookRunOK(run) {
			continue
		}
		if firstFailed == nil {
			firstFailed = run
		}
		if stopAtFailure {
			break
		}
	}
	return runs, firstFailed
}

// runHook runs one hook, turning a panic or a missing answer of the Hooks
// implementation into a failed run.
func (a *Agent) runHook(ctx context.Context, j *job, name string, phase syncrv1.HookPhase, at verdict) (run *syncrv1.HookRun) {
	started := time.Now()
	broken := func(msg string) *syncrv1.HookRun {
		return &syncrv1.HookRun{
			Name: name, Phase: phase, Status: syncrv1.HookStatus_HOOK_STATUS_FAILED, ExitCode: -1,
			StartedAt: timestamppb.New(started), Duration: durationpb.New(time.Since(started)), Message: msg,
		}
	}
	defer func() {
		if v := recover(); v != nil {
			a.log.Error("hook runner panicked", "job_id", j.id, "hook", name, "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
			run = broken("internal worker error: the hook runner crashed")
		}
	}()
	if run = a.hooks.Run(ctx, name, phase, hookEnv(j, name, phase, at)); run == nil {
		run = broken("internal worker error: the hook runner reported nothing")
	}
	return run
}

// hookEnv is the environment that tells the hook name about j, which
// stands at v, for its run in phase.
func hookEnv(j *job, name string, phase syncrv1.HookPhase, at verdict) map[string]string {
	status, code := hookJobRunning, ""
	if phase != syncrv1.HookPhase_HOOK_PHASE_BEFORE {
		switch at.status {
		case syncrv1.JobResultStatus_JOB_RESULT_STATUS_FAILED:
			status = hookJobFailed
		case syncrv1.JobResultStatus_JOB_RESULT_STATUS_CANCELLED:
			status = hookJobCancelled
		default:
			status = hookJobSucceeded
		}
		if !at.ok() {
			code = link.ErrorCodeName(at.errorCode())
		}
	}
	return map[string]string{
		EnvHookName:     plainValue(name),
		EnvHookPhase:    link.HookPhaseName(phase),
		EnvJobID:        plainValue(j.id),
		EnvJobKind:      plainValue(link.KindOf(j.spec)),
		EnvJobStatus:    status,
		EnvJobErrorCode: plainValue(code),
	}
}

// plainValue returns s if it is made of ASCII letters, digits, ".", "_"
// and "-" only and is at most 128 bytes long, and "" otherwise: the
// promise the Env* variables make.
func plainValue(s string) string {
	if len(s) > 128 {
		return ""
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return ""
		}
	}
	return s
}
