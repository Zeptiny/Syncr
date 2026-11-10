package api

import (
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/contacts"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
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
		Features:          in.Features,
		ProbeOps:          in.ProbeOps,
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

// ---------------------------------------------------------------------------
// Schedules
// ---------------------------------------------------------------------------

func scheduleTargetToProto(t schedules.Target) *syncrv1.ScheduleTarget {
	if !t.WorkerID.IsZero() {
		return &syncrv1.ScheduleTarget{Target: &syncrv1.ScheduleTarget_WorkerId{WorkerId: t.WorkerID.String()}}
	}
	return &syncrv1.ScheduleTarget{Target: &syncrv1.ScheduleTarget_WorkerLabel{WorkerLabel: t.Label}}
}

func missedActionToProto(a schedules.MissedAction) syncrv1.MissedRunAction {
	switch a {
	case schedules.MissedRunLate:
		return syncrv1.MissedRunAction_MISSED_RUN_ACTION_RUN_LATE
	case schedules.MissedSkip:
		return syncrv1.MissedRunAction_MISSED_RUN_ACTION_SKIP
	default:
		return syncrv1.MissedRunAction_MISSED_RUN_ACTION_UNSPECIFIED
	}
}

// missedActionFromProto maps UNSPECIFIED to run_late (the anacron-style
// default: a desktop that was off runs when it is back).
func missedActionFromProto(a syncrv1.MissedRunAction) (schedules.MissedAction, bool) {
	switch a {
	case syncrv1.MissedRunAction_MISSED_RUN_ACTION_UNSPECIFIED, syncrv1.MissedRunAction_MISSED_RUN_ACTION_RUN_LATE:
		return schedules.MissedRunLate, true
	case syncrv1.MissedRunAction_MISSED_RUN_ACTION_SKIP:
		return schedules.MissedSkip, true
	default:
		return "", false
	}
}

func overlapToProto(o schedules.Overlap) syncrv1.OverlapPolicy {
	switch o {
	case schedules.OverlapSkip:
		return syncrv1.OverlapPolicy_OVERLAP_POLICY_SKIP
	case schedules.OverlapQueue:
		return syncrv1.OverlapPolicy_OVERLAP_POLICY_QUEUE
	case schedules.OverlapAllow:
		return syncrv1.OverlapPolicy_OVERLAP_POLICY_ALLOW
	default:
		return syncrv1.OverlapPolicy_OVERLAP_POLICY_UNSPECIFIED
	}
}

// overlapFromProto maps UNSPECIFIED to "" (the service's default, skip).
func overlapFromProto(o syncrv1.OverlapPolicy) (schedules.Overlap, bool) {
	switch o {
	case syncrv1.OverlapPolicy_OVERLAP_POLICY_UNSPECIFIED:
		return "", true
	case syncrv1.OverlapPolicy_OVERLAP_POLICY_SKIP:
		return schedules.OverlapSkip, true
	case syncrv1.OverlapPolicy_OVERLAP_POLICY_QUEUE:
		return schedules.OverlapQueue, true
	case syncrv1.OverlapPolicy_OVERLAP_POLICY_ALLOW:
		return schedules.OverlapAllow, true
	default:
		return "", false
	}
}

// notificationEventToProto and notificationEventFromProto convert
// notification events: the events of a schedule's contact links, of a
// contact's manual jobs and of a delivery.
func notificationEventToProto(e domain.NotificationEvent) syncrv1.NotificationEvent {
	switch e {
	case domain.EventSucceeded:
		return syncrv1.NotificationEvent_NOTIFICATION_EVENT_SUCCEEDED
	case domain.EventFailed:
		return syncrv1.NotificationEvent_NOTIFICATION_EVENT_FAILED
	case domain.EventInterrupted:
		return syncrv1.NotificationEvent_NOTIFICATION_EVENT_INTERRUPTED
	case domain.EventMissed:
		return syncrv1.NotificationEvent_NOTIFICATION_EVENT_MISSED
	case domain.EventTest:
		return syncrv1.NotificationEvent_NOTIFICATION_EVENT_TEST
	default:
		return syncrv1.NotificationEvent_NOTIFICATION_EVENT_UNSPECIFIED
	}
}

// notificationEventFromProto maps an event; UNSPECIFIED and unknown values
// are not events.
func notificationEventFromProto(e syncrv1.NotificationEvent) (domain.NotificationEvent, bool) {
	switch e {
	case syncrv1.NotificationEvent_NOTIFICATION_EVENT_SUCCEEDED:
		return domain.EventSucceeded, true
	case syncrv1.NotificationEvent_NOTIFICATION_EVENT_FAILED:
		return domain.EventFailed, true
	case syncrv1.NotificationEvent_NOTIFICATION_EVENT_INTERRUPTED:
		return domain.EventInterrupted, true
	case syncrv1.NotificationEvent_NOTIFICATION_EVENT_MISSED:
		return domain.EventMissed, true
	case syncrv1.NotificationEvent_NOTIFICATION_EVENT_TEST:
		return domain.EventTest, true
	default:
		return "", false
	}
}

func runStatusToProto(s schedules.RunStatus) syncrv1.ScheduleRunStatus {
	switch s {
	case schedules.RunPending:
		return syncrv1.ScheduleRunStatus_SCHEDULE_RUN_STATUS_PENDING
	case schedules.RunLaunching:
		return syncrv1.ScheduleRunStatus_SCHEDULE_RUN_STATUS_LAUNCHING
	case schedules.RunLaunched:
		return syncrv1.ScheduleRunStatus_SCHEDULE_RUN_STATUS_LAUNCHED
	case schedules.RunMissed:
		return syncrv1.ScheduleRunStatus_SCHEDULE_RUN_STATUS_MISSED
	case schedules.RunFailed:
		return syncrv1.ScheduleRunStatus_SCHEDULE_RUN_STATUS_FAILED
	case schedules.RunCancelled:
		return syncrv1.ScheduleRunStatus_SCHEDULE_RUN_STATUS_CANCELLED
	default:
		return syncrv1.ScheduleRunStatus_SCHEDULE_RUN_STATUS_UNSPECIFIED
	}
}

func runTriggerToProto(t string) syncrv1.ScheduleRunTrigger {
	switch t {
	case schedules.TriggerCron:
		return syncrv1.ScheduleRunTrigger_SCHEDULE_RUN_TRIGGER_CRON
	case schedules.TriggerManual:
		return syncrv1.ScheduleRunTrigger_SCHEDULE_RUN_TRIGGER_MANUAL
	default:
		return syncrv1.ScheduleRunTrigger_SCHEDULE_RUN_TRIGGER_UNSPECIFIED
	}
}

// scheduleRunToProto converts a run (it holds no secrets).
func scheduleRunToProto(r schedules.Run) *syncrv1.ScheduleRun {
	out := &syncrv1.ScheduleRun{
		Id:           r.ID.String(),
		ScheduleId:   r.ScheduleID.String(),
		Trigger:      runTriggerToProto(r.Trigger),
		ScheduledFor: ts(r.ScheduledFor),
		Deadline:     ts(r.Deadline),
		Coalesced:    int32(min(max(r.Coalesced, 0), 1<<31-1)),
		Status:       runStatusToProto(r.Status),
		Reason:       r.Reason,
		Message:      r.Message,
		JobStatus:    jobStatusToProto(r.JobStatus),
		CreatedAt:    ts(r.CreatedAt),
		UpdatedAt:    ts(r.UpdatedAt),
	}
	if !r.WorkerID.IsZero() {
		out.WorkerId = r.WorkerID.String()
	}
	if !r.JobID.IsZero() {
		out.JobId = r.JobID.String()
	}
	return out
}

// scheduleRefs are the display references a converted schedule needs.
type scheduleRefs struct {
	remotes  map[domain.RemoteID]remotes.Ref
	contacts map[domain.ContactID]contacts.Ref
	repos    map[domain.ResticRepoID]restic.Ref
	// last is each schedule's newest run.
	last map[domain.ScheduleID]schedules.Run
}

// scheduleToProto converts sc (it holds no secrets: the definition refers
// to remotes and repositories by ID). refs names its remotes, repository
// and contacts and holds its last run. The returned error reports a stored definition that cannot be
// decoded; the schedule is still returned, without it (as jobToProto).
func scheduleToProto(sc schedules.Schedule, refs scheduleRefs) (*syncrv1.Schedule, error) {
	out := &syncrv1.Schedule{
		Id:           sc.ID.String(),
		OwnerId:      sc.OwnerID.String(),
		Name:         sc.Name,
		Description:  sc.Description,
		Cron:         sc.Cron,
		Timezone:     sc.Timezone,
		Target:       scheduleTargetToProto(sc.Target),
		MissedRun:    &syncrv1.MissedRunPolicy{Action: missedActionToProto(sc.Missed.Action)},
		Overlap:      overlapToProto(sc.Overlap),
		Enabled:      sc.Enabled,
		PausedReason: sc.PausedReason,
		NextRunAt:    ts(sc.NextRunAt),
		LastFiredAt:  ts(sc.LastFiredAt),
		CreatedAt:    ts(sc.CreatedAt),
		UpdatedAt:    ts(sc.UpdatedAt),
		Version:      sc.Version,
	}
	if sc.Missed.MaxDelay > 0 {
		out.MissedRun.MaxDelay = durationpb.New(sc.Missed.MaxDelay)
	}
	for _, id := range sc.RemoteIDs {
		r := refs.remotes[id]
		out.Remotes = append(out.Remotes, &syncrv1.JobRemoteRef{Id: id.String(), Name: r.Name, BackendType: r.Type, Deleted: r.Deleted})
	}
	for _, l := range sc.Contacts {
		c := refs.contacts[l.ContactID]
		link := &syncrv1.ScheduleContact{ContactId: l.ContactID.String(), ContactName: c.Name, ChannelType: c.Type}
		for _, e := range l.Events {
			link.Events = append(link.Events, notificationEventToProto(e))
		}
		out.Contacts = append(out.Contacts, link)
	}
	if r, ok := refs.last[sc.ID]; ok {
		out.LastRun = scheduleRunToProto(r)
	}
	if !sc.RepositoryID.IsZero() {
		out.Repository = repositoryRefToProto(sc.RepositoryID, refs.repos[sc.RepositoryID])
	}
	def, err := jobkind.Unmarshal(sc.Definition)
	if err != nil {
		return out, fmt.Errorf("decode definition of schedule %s: %w", sc.ID, err)
	}
	out.Definition = def
	return out, nil
}

// ---------------------------------------------------------------------------
// Restic repositories
// ---------------------------------------------------------------------------

// repositoryRefToProto names a job's or a schedule's repository: its ID
// and, when ref is its reference, its name and whether it was deleted.
func repositoryRefToProto(id domain.ResticRepoID, ref restic.Ref) *syncrv1.JobRepositoryRef {
	return &syncrv1.JobRepositoryRef{Id: id.String(), Name: ref.Name, Deleted: ref.Deleted}
}

// repositoryToProto converts r with its statistics; remote names its
// remote. The encrypted password is never part of the message.
func repositoryToProto(r restic.Repo, remote remotes.Ref) *syncrv1.Repository {
	out := &syncrv1.Repository{
		Id:            r.ID.String(),
		Name:          r.Name,
		Description:   r.Description,
		RemoteId:      r.RemoteID.String(),
		Path:          r.Path,
		Remote:        &syncrv1.JobRemoteRef{Id: r.RemoteID.String(), Name: remote.Name, BackendType: remote.Type, Deleted: remote.Deleted},
		ResticId:      r.ResticID,
		FormatVersion: int32(min(max(r.FormatVersion, 0), 1<<31-1)),
		Stats:         repositoryStatsToProto(r.Stats),
		CreatedAt:     ts(r.CreatedAt),
		UpdatedAt:     ts(r.UpdatedAt),
		Version:       r.Version,
		Deleting:      r.Deleting,
		DeletionError: r.DeletionError,
	}
	if !r.DeletionJobID.IsZero() {
		out.DeletionJobId = r.DeletionJobID.String()
		out.DeletionRequestedAt = ts(r.DeletionRequestedAt)
	}
	return out
}

func repositoryStatsToProto(s restic.RepoStats) *syncrv1.RepositoryStats {
	clamp := func(n int) int32 { return int32(min(max(n, 0), 1<<31-1)) }
	return &syncrv1.RepositoryStats{
		SnapshotCount:        clamp(s.SnapshotCount),
		LatestSnapshotAt:     ts(s.LatestSnapshotAt),
		LatestSnapshotBytes:  s.LatestSnapshotBytes,
		SnapshotsRefreshedAt: ts(s.SnapshotsRefreshedAt),
		SnapshotsStale:       s.SnapshotsStale(),
		SnapshotsTruncated:   s.SnapshotsTruncated,
		LastBackup:           repositoryJobRefToProto(s.LastBackup),
		LastCheck:            repositoryJobRefToProto(s.LastCheck),
		ScheduleCount:        clamp(s.ScheduleCount),
	}
}

// repositoryJobRefToProto converts a job reference; nil if there is none.
func repositoryJobRefToProto(r restic.JobRef) *syncrv1.RepositoryJobRef {
	if r.ID.IsZero() {
		return nil
	}
	return &syncrv1.RepositoryJobRef{JobId: r.ID.String(), Status: jobStatusToProto(r.Status), CreatedAt: ts(r.CreatedAt), FinishedAt: ts(r.FinishedAt)}
}

// repositoryModeFromProto maps a create mode; UNSPECIFIED and unknown
// values are "" (the service reports the "mode" field).
func repositoryModeFromProto(m syncrv1.RepositoryCreateMode) restic.Mode {
	switch m {
	case syncrv1.RepositoryCreateMode_REPOSITORY_CREATE_MODE_INIT:
		return restic.ModeInit
	case syncrv1.RepositoryCreateMode_REPOSITORY_CREATE_MODE_CONNECT:
		return restic.ModeConnect
	default:
		return ""
	}
}

// snapshotToProto converts a cached snapshot.
func snapshotToProto(s restic.Snapshot) *syncrv1.ResticSnapshot {
	out := &syncrv1.ResticSnapshot{
		Id: s.ID, Time: ts(s.Time), Host: s.Hostname, Paths: s.Paths, Tags: s.Tags,
		Parent: s.Parent, Username: s.Username, ProgramVersion: s.ProgramVersion,
	}
	if sum := s.Summary; sum != nil {
		out.Summary = &syncrv1.ResticSnapshotSummary{
			BackupStart: ts(sum.BackupStart), BackupEnd: ts(sum.BackupEnd),
			FilesNew: sum.FilesNew, FilesChanged: sum.FilesChanged, FilesUnmodified: sum.FilesUnmodified,
			DirsNew: sum.DirsNew, DirsChanged: sum.DirsChanged, DirsUnmodified: sum.DirsUnmodified,
			DataBlobs: sum.DataBlobs, TreeBlobs: sum.TreeBlobs,
			DataAddedBytes: sum.DataAdded, DataAddedPackedBytes: sum.DataAddedPacked,
			TotalFilesProcessed: sum.TotalFilesProcessed, TotalBytesProcessed: sum.TotalBytesProcessed,
		}
	}
	return out
}

func entryTypeToProto(t restic.EntryType) syncrv1.ProbeEntryType {
	switch t {
	case restic.EntryFile:
		return syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_FILE
	case restic.EntryDir:
		return syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR
	case restic.EntrySymlink:
		return syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_SYMLINK
	case restic.EntryOther:
		return syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_OTHER
	default:
		return syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_UNSPECIFIED
	}
}

// entryToProto converts an entry of a listed snapshot directory.
func entryToProto(e restic.Entry) *syncrv1.ProbeEntry {
	return &syncrv1.ProbeEntry{Name: e.Name, Path: e.Path, Type: entryTypeToProto(e.Type), Size: e.Size, ModTime: ts(e.ModTime), Mode: e.Mode}
}

// probeWorkerRef names the worker that ran a probe or runs a job; nil for
// the zero ID.
func probeWorkerRef(id domain.WorkerID, name string) *syncrv1.ProbeWorkerRef {
	if id.IsZero() {
		return nil
	}
	return &syncrv1.ProbeWorkerRef{Id: id.String(), Name: name}
}

// jobDetailsToProto converts a kind's display details.
func jobDetailsToProto(ds []jobkind.Detail) []*syncrv1.JobDetail {
	if len(ds) == 0 {
		return nil
	}
	out := make([]*syncrv1.JobDetail, len(ds))
	for i, d := range ds {
		out[i] = &syncrv1.JobDetail{Label: d.Label, Value: d.Value}
	}
	return out
}
