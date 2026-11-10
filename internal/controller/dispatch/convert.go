package dispatch

import (
	"fmt"
	"time"

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

func infoFromHello(h *syncrv1.Hello, at time.Time) workers.Info {
	info := workers.Info{
		Version: h.GetWorkerVersion(), OS: h.GetOs(), Arch: h.GetArch(), Hostname: h.GetHostname(),
		JobKinds: h.GetJobKinds(), MaxConcurrentJobs: int(h.GetMaxConcurrentJobs()),
		LocalRoots: h.GetLocalRoots(), Features: h.GetFeatures(), ProbeOps: h.GetProbeOps(), ReportedAt: at,
	}
	for _, t := range h.GetTools() {
		info.Tools = append(info.Tools, workers.Tool{Name: t.GetName(), Version: t.GetVersion()})
	}
	return info
}

// outcomeFromResult maps a worker's JobResult to the outcome the job
// service records (which turns an unrequested CANCELLED into interrupted).
func outcomeFromResult(r *syncrv1.JobResult) jobs.Outcome {
	o := jobs.Outcome{
		Stats:      statsFromProto(r.GetStats()),
		ExitCode:   int(r.GetExitCode()),
		StartedAt:  timeOf(r.GetStartedAt()),
		FinishedAt: timeOf(r.GetFinishedAt()),
	}
	if r.GetError() != nil {
		o.Error = jobs.JobError{Code: link.ErrorCodeName(r.GetError().GetCode()), Message: r.GetError().GetMessage()}
	}
	switch r.GetStatus() {
	case syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED:
		o.Status = domain.JobSucceeded
		o.Error = jobs.JobError{}
		o.Warnings = r.GetWarnings()
		if r.GetOutput() != nil {
			if b, err := (proto.MarshalOptions{Deterministic: true}).Marshal(r.GetOutput()); err == nil {
				o.Output = b
			}
		}
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
