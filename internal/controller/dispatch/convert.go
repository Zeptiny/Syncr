package dispatch

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// timeOf converts a wire timestamp; nil or invalid gives the zero time.
func timeOf(ts *timestamppb.Timestamp) time.Time {
	if ts == nil || ts.CheckValid() != nil {
		return time.Time{}
	}
	return ts.AsTime()
}

func statsFromProto(p *syncrv1.JobStats) jobs.Stats {
	return jobs.Stats{
		Bytes: p.GetBytes(), TotalBytes: p.GetTotalBytes(),
		Files: p.GetFiles(), TotalFiles: p.GetTotalFiles(),
		Checks: p.GetChecks(), Deletes: p.GetDeletes(), Renames: p.GetRenames(), Errors: p.GetErrors(),
		Elapsed: p.GetElapsed().AsDuration(),
	}
}

// progressPhase returns the phase of a worker's progress sample
// (Progress.phase) when it is one the controller knows (link
// ProgressPhaseMirror, ProgressPhaseBackup); "" otherwise, so a
// misbehaving worker cannot put arbitrary text in front of users.
func progressPhase(p string) string {
	switch p {
	case link.ProgressPhaseMirror, link.ProgressPhaseBackup:
		return p
	}
	return ""
}

func infoFromHello(h *syncrv1.Hello, at time.Time) workers.Info {
	info := workers.Info{
		Version: h.GetWorkerVersion(), OS: h.GetOs(), Arch: h.GetArch(), Hostname: h.GetHostname(),
		JobKinds: h.GetJobKinds(), MaxConcurrentJobs: int(h.GetMaxConcurrentJobs()),
		LocalRoots: h.GetLocalRoots(), Features: h.GetFeatures(), ProbeOps: h.GetProbeOps(), ReportedAt: at,
	}
	for _, t := range h.GetTools() {
		info.Tools = append(info.Tools, workers.Tool{Name: t.GetName(), Version: t.GetVersion()})
	}
	// The workers service keeps the ones a job could name (valid names,
	// bounded).
	for _, hk := range h.GetHooks() {
		hook := workers.Hook{Name: hk.GetName(), Description: hk.GetDescription()}
		if d := hk.GetTimeout(); d != nil && d.CheckValid() == nil {
			hook.Timeout = d.AsDuration()
		}
		info.Hooks = append(info.Hooks, hook)
	}
	// The workers service bounds the reasons (one line, 256 bytes).
	if rs := h.GetRemoteSources(); rs != nil {
		info.RemoteSources = workers.RemoteSources{
			Mirror: rs.GetMirror(), Mount: rs.GetMount(), MirrorReason: rs.GetMirrorReason(), MountReason: rs.GetMountReason(),
		}
	}
	return info
}

// outcomeFromResult maps a worker's JobResult to the outcome the job
// service records (which turns an unrequested CANCELLED into interrupted).
// A job its worker stopped at its time limit (TIME_LIMIT, which workers
// report as FAILED) is failed whatever status came with the code, so it
// notifies and fails its schedule's run like any other failure
// (jobs.CodeTimeLimit).
//
// The output and the warnings are the operation's, so they are kept
// whatever the status: a job whose operation succeeded can still fail or be
// stopped in a hook that follows it (jobs.CodeHookFailed), and its backup
// exists all the same. A worker reports neither for an operation that did
// not succeed. The hook runs are kept as reported, within bounds
// (hookRunsFromProto).
func outcomeFromResult(r *syncrv1.JobResult) jobs.Outcome {
	o := jobs.Outcome{
		Stats:      statsFromProto(r.GetStats()),
		ExitCode:   int(r.GetExitCode()),
		StartedAt:  timeOf(r.GetStartedAt()),
		FinishedAt: timeOf(r.GetFinishedAt()),
		Warnings:   r.GetWarnings(),
		HookRuns:   hookRunsFromProto(r.GetHookRuns()),
	}
	if r.GetOutput() != nil {
		if b, err := (proto.MarshalOptions{Deterministic: true}).Marshal(r.GetOutput()); err == nil {
			o.Output = b
		}
	}
	if r.GetError() != nil {
		o.Error = jobs.JobError{Code: link.ErrorCodeName(r.GetError().GetCode()), Message: r.GetError().GetMessage()}
	}
	if r.GetError().GetCode() == syncrv1.JobErrorCode_JOB_ERROR_CODE_TIME_LIMIT &&
		r.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED {
		o.Status = domain.JobFailed
		return o
	}
	switch r.GetStatus() {
	case syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED:
		o.Status = domain.JobSucceeded
		o.Error = jobs.JobError{}
	case syncrv1.JobResultStatus_JOB_RESULT_STATUS_FAILED:
		o.Status = domain.JobFailed
		if o.Error.IsZero() {
			o.Error = jobs.JobError{Code: jobs.CodeInternal, Message: "the worker reported a failure without details"}
		}
	case syncrv1.JobResultStatus_JOB_RESULT_STATUS_CANCELLED:
		o.Status = domain.JobCancelled
	default:
		o.Status = domain.JobFailed
		o.Error = jobs.JobError{Code: jobs.CodeInternal, Message: fmt.Sprintf("the worker reported an unknown result status %d", r.GetStatus())}
	}
	return o
}

// hookRunsFromProto converts the hook runs of a worker's result, bounded so
// that a misbehaving worker cannot bloat a job's record: at most
// jobs.MaxHookRuns runs, each with its texts as valid UTF-8 within the
// link's limits. A run whose phase or status this controller does not know
// keeps the others: its phase or status is stored empty.
func hookRunsFromProto(runs []*syncrv1.HookRun) []jobs.HookRun {
	if len(runs) == 0 {
		return nil
	}
	runs = runs[:min(len(runs), jobs.MaxHookRuns)]
	out := make([]jobs.HookRun, 0, len(runs))
	for _, r := range runs {
		run := jobs.HookRun{
			Name:            clipText(r.GetName(), link.MaxHookNameLen),
			Phase:           link.HookPhaseName(r.GetPhase()),
			Status:          link.HookStatusName(r.GetStatus()),
			ExitCode:        int(r.GetExitCode()),
			StartedAt:       timeOf(r.GetStartedAt()),
			Message:         clipText(r.GetMessage(), link.MaxHookMessageBytes),
			Output:          clipText(r.GetOutput(), link.MaxHookOutputBytes),
			OutputTruncated: r.GetOutputTruncated(),
		}
		if d := r.GetDuration(); d != nil && d.CheckValid() == nil {
			run.Duration = max(d.AsDuration(), 0)
		}
		if len(run.Output) < len(r.GetOutput()) {
			run.OutputTruncated = true
		}
		out = append(out, run)
	}
	return out
}

// clipText returns s as valid UTF-8 of at most n bytes, cut on a character
// boundary.
func clipText(s string, n int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
