package api

import (
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/link"
)

// This file is the single place where application types and wire messages
// are converted into each other. The conversions are pure.

// ts converts t, mapping the zero time to an unset timestamp.
func ts(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// ---------------------------------------------------------------------------
// Workers
// ---------------------------------------------------------------------------

func workerModeToProto(m domain.WorkerMode) syncrv1.WorkerMode {
	switch m {
	case domain.WorkerActive:
		return syncrv1.WorkerMode_WORKER_MODE_ACTIVE
	case domain.WorkerPassive:
		return syncrv1.WorkerMode_WORKER_MODE_PASSIVE
	default:
		return syncrv1.WorkerMode_WORKER_MODE_UNSPECIFIED
	}
}

func workerStatusToProto(s domain.WorkerStatus) syncrv1.WorkerStatus {
	switch s {
	case domain.WorkerPending:
		return syncrv1.WorkerStatus_WORKER_STATUS_PENDING
	case domain.WorkerOnline:
		return syncrv1.WorkerStatus_WORKER_STATUS_ONLINE
	case domain.WorkerOffline:
		return syncrv1.WorkerStatus_WORKER_STATUS_OFFLINE
	default:
		return syncrv1.WorkerStatus_WORKER_STATUS_UNSPECIFIED
	}
}

// workerToProto converts w with its derived connection status. Secrets
// (SecretEnc) and link internals (epochs, ack cursor) are not exposed.
func workerToProto(w workers.Worker, status domain.WorkerStatus) *syncrv1.Worker {
	return &syncrv1.Worker{
		Id:              w.ID.String(),
		Name:            w.Name,
		Mode:            workerModeToProto(w.Mode),
		Status:          workerStatusToProto(status),
		LastSeen:        ts(w.Link.LastSeen),
		CreatedAt:       ts(w.CreatedAt),
		Url:             w.URL,
		CertFingerprint: w.CertFingerprint,
		Info:            workerInfoToProto(w.Info),
		Labels:          w.Labels,
	}
}

// workerInfoToProto converts what a worker reported in its Hello. It returns
// nil before the first Hello: enrollment pre-fills only OS and Arch, and
// every Hello carries ReportedAt (or at least a version).
func workerInfoToProto(in workers.Info) *syncrv1.WorkerInfo {
	if in.ReportedAt.IsZero() && in.Version == "" {
		return nil
	}
	out := &syncrv1.WorkerInfo{
		WorkerVersion:     in.Version,
		Os:                in.OS,
		Arch:              in.Arch,
		Hostname:          in.Hostname,
		JobKinds:          in.JobKinds,
		MaxConcurrentJobs: int32(min(max(in.MaxConcurrentJobs, 0), 1<<31-1)),
		LocalRoots:        in.LocalRoots,
		ReportedAt:        ts(in.ReportedAt),
	}
	for _, t := range in.Tools {
		out.Tools = append(out.Tools, &syncrv1.ToolVersion{Name: t.Name, Version: t.Version})
	}
	return out
}

// ---------------------------------------------------------------------------
// Jobs
// ---------------------------------------------------------------------------

func jobStatusToProto(s domain.JobStatus) syncrv1.JobStatus {
	switch s {
	case domain.JobQueued:
		return syncrv1.JobStatus_JOB_STATUS_QUEUED
	case domain.JobDispatched:
		return syncrv1.JobStatus_JOB_STATUS_DISPATCHED
	case domain.JobRunning:
		return syncrv1.JobStatus_JOB_STATUS_RUNNING
	case domain.JobSucceeded:
		return syncrv1.JobStatus_JOB_STATUS_SUCCEEDED
	case domain.JobFailed:
		return syncrv1.JobStatus_JOB_STATUS_FAILED
	case domain.JobCancelled:
		return syncrv1.JobStatus_JOB_STATUS_CANCELLED
	case domain.JobInterrupted:
		return syncrv1.JobStatus_JOB_STATUS_INTERRUPTED
	default:
		return syncrv1.JobStatus_JOB_STATUS_UNSPECIFIED
	}
}

// jobStatusFromProto is the inverse of jobStatusToProto. UNSPECIFIED and
// unknown values are an error: a filter must name real statuses.
func jobStatusFromProto(s syncrv1.JobStatus) (domain.JobStatus, error) {
	switch s {
	case syncrv1.JobStatus_JOB_STATUS_QUEUED:
		return domain.JobQueued, nil
	case syncrv1.JobStatus_JOB_STATUS_DISPATCHED:
		return domain.JobDispatched, nil
	case syncrv1.JobStatus_JOB_STATUS_RUNNING:
		return domain.JobRunning, nil
	case syncrv1.JobStatus_JOB_STATUS_SUCCEEDED:
		return domain.JobSucceeded, nil
	case syncrv1.JobStatus_JOB_STATUS_FAILED:
		return domain.JobFailed, nil
	case syncrv1.JobStatus_JOB_STATUS_CANCELLED:
		return domain.JobCancelled, nil
	case syncrv1.JobStatus_JOB_STATUS_INTERRUPTED:
		return domain.JobInterrupted, nil
	default:
		return "", fmt.Errorf("%w: job status %v", domain.ErrInvalidJobStatus, s)
	}
}

func statsToProto(s jobs.Stats) *syncrv1.JobStats {
	return &syncrv1.JobStats{
		Bytes:      s.Bytes,
		TotalBytes: s.TotalBytes,
		Files:      s.Files,
		TotalFiles: s.TotalFiles,
		Checks:     s.Checks,
		Deletes:    s.Deletes,
		Renames:    s.Renames,
		Errors:     s.Errors,
		Elapsed:    durationpb.New(s.Elapsed),
	}
}

func progressToProto(s jobs.Sample) *syncrv1.JobProgress {
	return &syncrv1.JobProgress{Time: ts(s.Time), Stats: statsToProto(s.Stats)}
}

// jobToProto converts j (it holds no secrets).
//
// stats is the job's counters to report, which the caller picks (see
// pickStats): nil leaves them unset. refs names the job's remotes: every
// one of j.RemoteIDs becomes a reference, with the name, type and deleted
// flag refs has for it. The returned error reports a stored definition or
// output that cannot be decoded; the job is still returned, without it, so
// one corrupt record does not break a listing. The caller logs it.
func jobToProto(j jobs.Job, stats *jobs.Stats, refs map[domain.RemoteID]remotes.Ref) (*syncrv1.Job, error) {
	out := &syncrv1.Job{
		Id:              j.ID.String(),
		WorkerId:        j.WorkerID.String(),
		Kind:            j.Kind,
		Status:          jobStatusToProto(j.Status),
		CancelRequested: j.CancelRequested(),
		CreatedAt:       ts(j.CreatedAt),
		DispatchedAt:    ts(j.DispatchedAt),
		StartedAt:       ts(j.StartedAt),
		FinishedAt:      ts(j.FinishedAt),
		Requeues:        int32(min(max(j.Requeues, 0), 1<<31-1)),
		Warnings:        j.Warnings,
	}
	if stats != nil {
		out.Stats = statsToProto(*stats)
	}
	if !j.Error.IsZero() {
		out.Error = &syncrv1.JobError{Code: link.ParseErrorCode(j.Error.Code), Message: j.Error.Message}
	}
	if j.Status.Terminal() && j.ExitCode >= 0 {
		code := int32(min(j.ExitCode, 1<<31-1))
		out.ExitCode = &code
	}
	if !j.OwnerID.IsZero() {
		out.OwnerId = j.OwnerID.String()
	}
	if !j.ScheduleID.IsZero() {
		out.ScheduleId = j.ScheduleID.String()
	}
	for _, id := range j.RemoteIDs {
		r := refs[id]
		out.Remotes = append(out.Remotes, &syncrv1.JobRemoteRef{Id: id.String(), Name: r.Name, BackendType: r.Type, Deleted: r.Deleted})
	}
	var errs []error
	if len(j.Definition) > 0 {
		def, err := jobkind.Unmarshal(j.Definition)
		if err != nil {
			errs = append(errs, fmt.Errorf("decode definition of job %s: %w", j.ID, err))
		} else {
			out.Definition = def
		}
	}
	if len(j.Output) > 0 {
		var output syncrv1.JobOutput
		if err := proto.Unmarshal(j.Output, &output); err != nil {
			errs = append(errs, fmt.Errorf("decode output of job %s: %w", j.ID, err))
		} else {
			out.Output = &output
		}
	}
	return out, errors.Join(errs...)
}

// pickStats chooses the counters to report for j, and whether the latest
// stored sample is needed to know them: a terminal job reports its final
// stats unless it ended without any (interrupted, cancelled by the
// controller) after it had started, when the last sample is the best
// information; a running job reports the latest sample; a job that has not
// started reports none.
func pickStats(j jobs.Job) (final *jobs.Stats, needSample bool) {
	switch {
	case j.Status.Terminal() && j.Stats != (jobs.Stats{}):
		return &j.Stats, false
	case j.Status.Terminal():
		return nil, !j.StartedAt.IsZero()
	default:
		return nil, j.Status == domain.JobRunning
	}
}
