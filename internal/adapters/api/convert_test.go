package api

import (
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/contacts"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/notify"
)

var convT0 = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func TestWorkerToProto(t *testing.T) {
	t.Parallel()
	w := workers.Worker{
		ID: domain.NewWorkerID(), Name: "alpha", Mode: domain.WorkerPassive,
		URL: "https://w.example", CertFingerprint: testFP, SecretEnc: []byte("ciphertext"),
		CreatedAt: convT0, Link: workers.Link{Epoch: "e1", AckSeq: 9, PastEpochs: []string{"e0"}, LastSeen: convT0.Add(time.Minute)},
		Info: workers.Info{
			Version: "v2.1", OS: "linux", Arch: "arm64", Hostname: "box",
			Tools:    []workers.Tool{{Name: "rclone", Version: "v1.75"}, {Name: "restic", Version: "0.19"}},
			JobKinds: []string{"rclone_transfer"}, MaxConcurrentJobs: 3, LocalRoots: []string{"/data"}, ReportedAt: convT0,
		},
	}
	want := &syncrv1.Worker{
		Id: w.ID.String(), Name: "alpha", Mode: syncrv1.WorkerMode_WORKER_MODE_PASSIVE, Status: syncrv1.WorkerStatus_WORKER_STATUS_ONLINE,
		LastSeen: ts(convT0.Add(time.Minute)), CreatedAt: ts(convT0), Url: "https://w.example", CertFingerprint: testFP,
		Info: &syncrv1.WorkerInfo{
			WorkerVersion: "v2.1", Os: "linux", Arch: "arm64", Hostname: "box",
			Tools:    []*syncrv1.ToolVersion{{Name: "rclone", Version: "v1.75"}, {Name: "restic", Version: "0.19"}},
			JobKinds: []string{"rclone_transfer"}, MaxConcurrentJobs: 3, LocalRoots: []string{"/data"}, ReportedAt: ts(convT0),
		},
	}
	if got := workerToProto(w, domain.WorkerOnline); !proto.Equal(got, want) {
		t.Errorf("workerToProto =\n%v\nwant\n%v", got, want)
	}
}

func TestWorkerToProtoMinimal(t *testing.T) {
	t.Parallel()
	// An enrolled worker that never connected: OS and Arch from enrollment
	// do not make an Info.
	w := workers.Worker{ID: domain.NewWorkerID(), Name: "n", Mode: domain.WorkerActive, Info: workers.Info{OS: "linux", Arch: "amd64"}}
	got := workerToProto(w, domain.WorkerPending)
	if got.GetInfo() != nil || got.GetLastSeen() != nil || got.GetCreatedAt() != nil || got.GetUrl() != "" ||
		got.GetStatus() != syncrv1.WorkerStatus_WORKER_STATUS_PENDING || got.GetMode() != syncrv1.WorkerMode_WORKER_MODE_ACTIVE {
		t.Errorf("workerToProto = %v", got)
	}
}

func TestWorkerEnums(t *testing.T) {
	t.Parallel()
	for in, want := range map[domain.WorkerStatus]syncrv1.WorkerStatus{
		domain.WorkerPending: syncrv1.WorkerStatus_WORKER_STATUS_PENDING,
		domain.WorkerOnline:  syncrv1.WorkerStatus_WORKER_STATUS_ONLINE,
		domain.WorkerOffline: syncrv1.WorkerStatus_WORKER_STATUS_OFFLINE,
		"bogus":              syncrv1.WorkerStatus_WORKER_STATUS_UNSPECIFIED,
	} {
		if got := workerStatusToProto(in); got != want {
			t.Errorf("status %q -> %v, want %v", in, got, want)
		}
	}
	for in, want := range map[domain.WorkerMode]syncrv1.WorkerMode{
		domain.WorkerActive:  syncrv1.WorkerMode_WORKER_MODE_ACTIVE,
		domain.WorkerPassive: syncrv1.WorkerMode_WORKER_MODE_PASSIVE,
		"":                   syncrv1.WorkerMode_WORKER_MODE_UNSPECIFIED,
	} {
		if got := workerModeToProto(in); got != want {
			t.Errorf("mode %q -> %v, want %v", in, got, want)
		}
	}
}

func TestJobStatusRoundTrip(t *testing.T) {
	t.Parallel()
	for _, st := range domain.JobStatuses() {
		p := jobStatusToProto(st)
		if p == syncrv1.JobStatus_JOB_STATUS_UNSPECIFIED {
			t.Errorf("%s has no wire status", st)
		}
		back, err := jobStatusFromProto(p)
		if err != nil || back != st {
			t.Errorf("%s -> %v -> %q, %v", st, p, back, err)
		}
	}
	// Every wire status is covered by some domain status.
	if len(domain.JobStatuses()) != len(syncrv1.JobStatus_name)-1 {
		t.Errorf("%d domain statuses, %d wire statuses", len(domain.JobStatuses()), len(syncrv1.JobStatus_name)-1)
	}
	for _, bad := range []syncrv1.JobStatus{syncrv1.JobStatus_JOB_STATUS_UNSPECIFIED, 99, -1} {
		if _, err := jobStatusFromProto(bad); err == nil {
			t.Errorf("jobStatusFromProto(%v) succeeded", bad)
		}
	}
	if got := jobStatusToProto("bogus"); got != syncrv1.JobStatus_JOB_STATUS_UNSPECIFIED {
		t.Errorf("bogus -> %v", got)
	}
}

func TestJobToProto(t *testing.T) {
	t.Parallel()
	out, err := proto.Marshal(&syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticForget{ResticForget: &syncrv1.ResticForgetOutput{}}})
	if err != nil {
		t.Fatal(err)
	}
	def, err := jobkind.Marshal(testDefinition())
	if err != nil {
		t.Fatal(err)
	}
	j := jobs.Job{
		ID: domain.NewJobID(), WorkerID: domain.NewWorkerID(), OwnerID: domain.NewUserID(), Kind: "restic_forget", Status: domain.JobFailed,
		Definition: def, Epoch: "e1", CancelRequestedAt: convT0,
		CreatedAt: convT0, QueuedAt: convT0.Add(time.Second), DispatchedAt: convT0.Add(2 * time.Second),
		StartedAt: convT0.Add(3 * time.Second), FinishedAt: convT0.Add(4 * time.Second),
		Requeues: 2, Error: jobs.JobError{Code: "wrong_password", Message: "nope"}, Warnings: []string{"a", "b"},
		ExitCode: 12, Output: out, Version: 7,
	}
	named, unnamed := domain.NewRemoteID(), domain.NewRemoteID()
	j.RemoteIDs = []domain.RemoteID{named, unnamed}
	refs := map[domain.RemoteID]remotes.Ref{named: {ID: named, Name: "Photos", Type: "local", Deleted: true}}
	stats := &jobs.Stats{Bytes: 1, TotalBytes: 2, Files: 3, TotalFiles: 4, Checks: 5, Deletes: 6, Renames: 7, Errors: 8, Elapsed: time.Minute}
	got, err := jobToProto(j, stats, refs)
	if err != nil {
		t.Fatal(err)
	}
	exit := int32(12)
	want := &syncrv1.Job{
		Id: j.ID.String(), WorkerId: j.WorkerID.String(), OwnerId: j.OwnerID.String(), Definition: testDefinition(),
		Kind: "restic_forget", Status: syncrv1.JobStatus_JOB_STATUS_FAILED, CancelRequested: true,
		CreatedAt: ts(convT0), DispatchedAt: ts(convT0.Add(2 * time.Second)), StartedAt: ts(convT0.Add(3 * time.Second)), FinishedAt: ts(convT0.Add(4 * time.Second)),
		Requeues: 2,
		Stats:    &syncrv1.JobStats{Bytes: 1, TotalBytes: 2, Files: 3, TotalFiles: 4, Checks: 5, Deletes: 6, Renames: 7, Errors: 8, Elapsed: statsToProto(jobs.Stats{Elapsed: time.Minute}).GetElapsed()},
		Error:    &syncrv1.JobError{Code: syncrv1.JobErrorCode_JOB_ERROR_CODE_WRONG_PASSWORD, Message: "nope"},
		Warnings: []string{"a", "b"}, ExitCode: &exit,
		Output: &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticForget{ResticForget: &syncrv1.ResticForgetOutput{}}},
		// Every remote is referenced; unknown ones without a name.
		Remotes: []*syncrv1.JobRemoteRef{{Id: named.String(), Name: "Photos", BackendType: "local", Deleted: true}, {Id: unnamed.String()}},
	}
	if !proto.Equal(got, want) {
		t.Errorf("jobToProto =\n%v\nwant\n%v", got, want)
	}
}

func TestJobToProtoEdges(t *testing.T) {
	t.Parallel()
	base := jobs.Job{ID: domain.NewJobID(), WorkerID: domain.NewWorkerID(), Kind: "k", Status: domain.JobQueued, ExitCode: -1, CreatedAt: convT0}

	got, err := jobToProto(base, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetStats() != nil || got.GetError() != nil || got.GetOutput() != nil || got.ExitCode != nil || got.GetDispatchedAt() != nil ||
		got.GetStartedAt() != nil || got.GetFinishedAt() != nil || got.GetCancelRequested() || got.GetOwnerId() != "" || got.GetDefinition() != nil {
		t.Errorf("queued job = %v", got)
	}

	// An unknown error code keeps its message and maps to UNSPECIFIED.
	base.Error = jobs.JobError{Code: "made_up", Message: "m"}
	if got, _ := jobToProto(base, nil, nil); got.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSPECIFIED || got.GetError().GetMessage() != "m" {
		t.Errorf("error = %v", got.GetError())
	}
	// Controller-set codes map too.
	for _, code := range []string{jobs.CodeCancelled, jobs.CodeWorkerShutdown, jobs.CodeDispatchTimeout, jobs.CodeWorkerRestarted, jobs.CodeWorkerLost,
		jobs.CodeWorkerRemoved, jobs.CodeInternal, jobs.CodeInvalidSpec, jobs.CodeRemoteUnavailable} {
		base.Error = jobs.JobError{Code: code}
		if got, _ := jobToProto(base, nil, nil); got.GetError().GetCode() == syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSPECIFIED {
			t.Errorf("code %q has no wire code", code)
		}
	}

	// Exit code 0 of a finished job is set (not confused with unset); the
	// exit code of an unfinished job is not reported.
	base.Status, base.ExitCode = domain.JobSucceeded, 0
	if got, _ := jobToProto(base, nil, nil); got.ExitCode == nil || got.GetExitCode() != 0 {
		t.Errorf("exit code = %v", got.ExitCode)
	}
	base.Status = domain.JobRunning
	if got, _ := jobToProto(base, nil, nil); got.ExitCode != nil {
		t.Errorf("exit code of a running job = %v", got.ExitCode)
	}

	// A corrupt output is reported but the job is still returned.
	base.Output = []byte{0xff, 0xff, 0xff}
	got, err = jobToProto(base, nil, nil)
	if err == nil || got == nil || got.GetOutput() != nil || got.GetId() != base.ID.String() {
		t.Errorf("corrupt output: %v, %v", got, err)
	}
	// So is a corrupt definition.
	base.Output, base.Definition = nil, []byte("{not json")
	got, err = jobToProto(base, nil, nil)
	if err == nil || got == nil || got.GetDefinition() != nil || got.GetId() != base.ID.String() {
		t.Errorf("corrupt definition: %v, %v", got, err)
	}
}

func TestJobMessageHasNoSpec(t *testing.T) {
	t.Parallel()
	// The wire Job and the messages carrying it must have no field that
	// could hold the spec. A new field named like one needs a conscious look.
	check := func(md protoreflect.MessageDescriptor) {
		for i := range md.Fields().Len() {
			fd := md.Fields().Get(i)
			if fd.Message() != nil && fd.Message().FullName() == (&syncrv1.JobSpec{}).ProtoReflect().Descriptor().FullName() {
				t.Errorf("%s.%s carries a JobSpec", md.FullName(), fd.Name())
			}
		}
	}
	for _, m := range []proto.Message{&syncrv1.Job{}, &syncrv1.RunJobResponse{}, &syncrv1.GetJobResponse{}, &syncrv1.ListJobsResponse{},
		&syncrv1.CancelJobResponse{}, &syncrv1.WatchJobResponse{}, &syncrv1.JobProgress{}} {
		check(m.ProtoReflect().Descriptor())
	}
}

func TestPickStats(t *testing.T) {
	t.Parallel()
	final := jobs.Stats{Bytes: 9}
	started := convT0
	tests := []struct {
		name       string
		job        jobs.Job
		wantFinal  *jobs.Stats
		wantSample bool
	}{
		{"queued", jobs.Job{Status: domain.JobQueued}, nil, false},
		{"dispatched", jobs.Job{Status: domain.JobDispatched}, nil, false},
		{"running", jobs.Job{Status: domain.JobRunning, StartedAt: started}, nil, true},
		{"succeeded", jobs.Job{Status: domain.JobSucceeded, Stats: final, StartedAt: started}, &final, false},
		{"interrupted after start", jobs.Job{Status: domain.JobInterrupted, StartedAt: started}, nil, true},
		{"failed before start", jobs.Job{Status: domain.JobFailed}, nil, false},
		{"cancelled while queued", jobs.Job{Status: domain.JobCancelled}, nil, false},
	}
	for _, tt := range tests {
		gotFinal, gotSample := pickStats(tt.job)
		if gotSample != tt.wantSample || (gotFinal == nil) != (tt.wantFinal == nil) || gotFinal != nil && *gotFinal != *tt.wantFinal {
			t.Errorf("%s: pickStats = (%v, %v), want (%v, %v)", tt.name, gotFinal, gotSample, tt.wantFinal, tt.wantSample)
		}
	}
}

func TestProgressToProto(t *testing.T) {
	t.Parallel()
	got := progressToProto(jobs.Sample{Time: convT0, Stats: jobs.Stats{Bytes: 3, Elapsed: 2 * time.Second}})
	if !got.GetTime().AsTime().Equal(convT0) || got.GetStats().GetBytes() != 3 || got.GetStats().GetElapsed().AsDuration() != 2*time.Second {
		t.Errorf("progressToProto = %v", got)
	}
}

// TestMilestone4Conversions covers the contracts step's additions: worker
// labels and a job's schedule ID (the schedule reference itself is filled
// in by the jobs handler).
func TestMilestone4Conversions(t *testing.T) {
	t.Parallel()
	w := workers.Worker{ID: domain.NewWorkerID(), Name: "nas-01", Labels: []string{"linux", "nas"}}
	if got := workerToProto(w, domain.WorkerOnline); !slices.Equal(got.GetLabels(), []string{"linux", "nas"}) {
		t.Errorf("labels = %v", got.GetLabels())
	}
	sched := domain.NewScheduleID()
	j, err := jobToProto(jobs.Job{ID: domain.NewJobID(), ScheduleID: sched, Status: domain.JobQueued}, nil, nil)
	if err != nil || j.GetScheduleId() != sched.String() || j.GetSchedule() != nil {
		t.Errorf("scheduled job = (%v, %v)", j, err)
	}
	if manual, _ := jobToProto(jobs.Job{ID: domain.NewJobID(), Status: domain.JobQueued}, nil, nil); manual.GetScheduleId() != "" {
		t.Errorf("manual job schedule_id = %q", manual.GetScheduleId())
	}
}

func TestScheduleConversions(t *testing.T) {
	t.Parallel()
	for _, a := range []schedules.MissedAction{schedules.MissedRunLate, schedules.MissedSkip} {
		if got, ok := missedActionFromProto(missedActionToProto(a)); !ok || got != a {
			t.Errorf("missed action %q round trip = %q", a, got)
		}
	}
	if got, ok := missedActionFromProto(syncrv1.MissedRunAction_MISSED_RUN_ACTION_UNSPECIFIED); !ok || got != schedules.MissedRunLate {
		t.Errorf("unspecified missed action = %q", got)
	}
	if _, ok := missedActionFromProto(99); ok {
		t.Error("unknown missed action accepted")
	}
	for _, o := range []schedules.Overlap{schedules.OverlapSkip, schedules.OverlapQueue, schedules.OverlapAllow} {
		if got, ok := overlapFromProto(overlapToProto(o)); !ok || got != o {
			t.Errorf("overlap %q round trip = %q", o, got)
		}
	}
	if got, ok := overlapFromProto(syncrv1.OverlapPolicy_OVERLAP_POLICY_UNSPECIFIED); !ok || got != "" {
		t.Errorf("unspecified overlap = %q", got)
	}
	if _, ok := overlapFromProto(99); ok {
		t.Error("unknown overlap accepted")
	}
	for _, e := range domain.NotificationEvents() {
		if got, ok := notificationEventFromProto(notificationEventToProto(e)); !ok || got != e {
			t.Errorf("event %q round trip = %q", e, got)
		}
	}
	if _, ok := notificationEventFromProto(syncrv1.NotificationEvent_NOTIFICATION_EVENT_UNSPECIFIED); ok {
		t.Error("unspecified event accepted")
	}
	statuses := map[schedules.RunStatus]syncrv1.ScheduleRunStatus{
		schedules.RunPending: syncrv1.ScheduleRunStatus_SCHEDULE_RUN_STATUS_PENDING, schedules.RunLaunching: syncrv1.ScheduleRunStatus_SCHEDULE_RUN_STATUS_LAUNCHING,
		schedules.RunLaunched: syncrv1.ScheduleRunStatus_SCHEDULE_RUN_STATUS_LAUNCHED, schedules.RunMissed: syncrv1.ScheduleRunStatus_SCHEDULE_RUN_STATUS_MISSED,
		schedules.RunFailed: syncrv1.ScheduleRunStatus_SCHEDULE_RUN_STATUS_FAILED, schedules.RunCancelled: syncrv1.ScheduleRunStatus_SCHEDULE_RUN_STATUS_CANCELLED,
		"bogus": syncrv1.ScheduleRunStatus_SCHEDULE_RUN_STATUS_UNSPECIFIED,
	}
	for s, want := range statuses {
		if got := runStatusToProto(s); got != want {
			t.Errorf("runStatusToProto(%q) = %v", s, got)
		}
	}

	r := schedules.Run{
		ID: domain.NewScheduleRunID(), ScheduleID: domain.NewScheduleID(), Trigger: schedules.TriggerCron,
		ScheduledFor: convT0, Deadline: convT0.Add(time.Hour), Coalesced: 3, Status: schedules.RunLaunched,
		WorkerID: domain.NewWorkerID(), JobID: domain.NewJobID(), JobStatus: domain.JobRunning, CreatedAt: convT0, UpdatedAt: convT0,
	}
	pr := scheduleRunToProto(r)
	if pr.GetId() != r.ID.String() || pr.GetTrigger() != syncrv1.ScheduleRunTrigger_SCHEDULE_RUN_TRIGGER_CRON || pr.GetCoalesced() != 3 ||
		pr.GetJobId() != r.JobID.String() || pr.GetWorkerId() != r.WorkerID.String() || pr.GetJobStatus() != syncrv1.JobStatus_JOB_STATUS_RUNNING ||
		!pr.GetDeadline().AsTime().Equal(r.Deadline) {
		t.Errorf("scheduleRunToProto = %v", pr)
	}
	if p := scheduleRunToProto(schedules.Run{Trigger: schedules.TriggerManual, Status: schedules.RunPending}); p.GetJobId() != "" || p.GetWorkerId() != "" ||
		p.GetTrigger() != syncrv1.ScheduleRunTrigger_SCHEDULE_RUN_TRIGGER_MANUAL || p.GetJobStatus() != syncrv1.JobStatus_JOB_STATUS_UNSPECIFIED {
		t.Errorf("pending run = %v", p)
	}

	remote, contact := domain.NewRemoteID(), domain.NewContactID()
	sc := schedules.Schedule{
		ID: r.ScheduleID, OwnerID: domain.NewUserID(), Name: "n", Cron: "0 * * * *", Timezone: "UTC",
		Definition: []byte(`{"rclone_transfer":{"mode":"RCLONE_TRANSFER_MODE_SYNC"}}`), RemoteIDs: []domain.RemoteID{remote},
		Target: schedules.Target{Label: "nas"}, Missed: schedules.MissedPolicy{Action: schedules.MissedSkip}, Overlap: schedules.OverlapAllow,
		Contacts: []schedules.ContactLink{{ContactID: contact, Events: domain.EventSet{domain.EventMissed}}}, Version: 4,
	}
	refs := scheduleRefs{
		remotes:  map[domain.RemoteID]remotes.Ref{remote: {ID: remote, Name: "Photos", Type: "s3", Deleted: true}},
		contacts: map[domain.ContactID]contacts.Ref{contact: {ID: contact, Name: "Ops", Type: "webhook"}},
		last:     map[domain.ScheduleID]schedules.Run{sc.ID: r},
	}
	ps, err := scheduleToProto(sc, refs)
	if err != nil || ps.GetTarget().GetWorkerLabel() != "nas" || ps.GetMissedRun().GetMaxDelay() != nil ||
		ps.GetOverlap() != syncrv1.OverlapPolicy_OVERLAP_POLICY_ALLOW || !ps.GetRemotes()[0].GetDeleted() ||
		ps.GetContacts()[0].GetContactName() != "Ops" || ps.GetLastRun().GetId() != r.ID.String() || ps.GetNextRunAt() != nil ||
		ps.GetDefinition().GetRcloneTransfer().GetMode() != syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_SYNC || ps.GetVersion() != 4 {
		t.Errorf("scheduleToProto = (%v, %v)", ps, err)
	}
	sc.Definition = []byte("not json")
	if ps, err := scheduleToProto(sc, scheduleRefs{}); err == nil || ps.GetDefinition() != nil || ps.GetName() != "n" || ps.GetLastRun() != nil {
		t.Errorf("scheduleToProto(corrupt definition) = (%v, %v)", ps, err)
	}
}

// The job kinds format sizes exactly as notifications do: the same job
// reads the same in the UI and in a notification.
func TestFormatBytesAgree(t *testing.T) {
	t.Parallel()
	for _, n := range []int64{0, 1, 2, 1023, 1024, 1025, 1536, 1 << 20, 1<<20 + 1<<19, 880803840, 5 << 30, 51646976819, 1 << 40, 1 << 50, 1 << 60, 1 << 62, 1<<63 - 1} {
		if a, b := jobkind.FormatBytes(n), notify.FormatBytes(n); a != b {
			t.Errorf("FormatBytes(%d): jobkind %q, notify %q", n, a, b)
		}
	}
}

func TestRepositoryToProto(t *testing.T) {
	t.Parallel()
	id, remote, job := domain.NewResticRepoID(), domain.NewRemoteID(), domain.NewJobID()
	backup, check := domain.NewJobID(), domain.NewJobID()
	r := restic.Repo{
		ID: id, OwnerID: domain.NewUserID(), Name: "Photos vault", Description: "family", RemoteID: remote, Path: "restic/photos",
		PasswordEnc: []byte("never shown"), ResticID: "abc", FormatVersion: 2,
		Deleting: true, DeletionJobID: job, DeletionRequestedAt: convT0.Add(time.Hour), DeletionError: "",
		CreatedAt: convT0, UpdatedAt: convT0.Add(time.Minute), Version: 4,
		Stats: restic.RepoStats{
			SnapshotCount: 3, LatestSnapshotAt: convT0, LatestSnapshotBytes: 12345, SnapshotsRefreshedAt: convT0.Add(time.Minute),
			SnapshotsTruncated: true, SnapshotsMark: restic.ChangeMark{Jobs: 1, LastFinishedAt: convT0}, ChangeMark: restic.ChangeMark{Jobs: 2, LastFinishedAt: convT0},
			LastBackup:    restic.JobRef{ID: backup, Status: domain.JobSucceeded, CreatedAt: convT0, FinishedAt: convT0.Add(2 * time.Minute)},
			LastCheck:     restic.JobRef{ID: check, Status: domain.JobRunning, CreatedAt: convT0},
			ScheduleCount: 2,
		},
	}
	got := repositoryToProto(r, remotes.Ref{ID: remote, Name: "B2 offsite", Type: "b2", Deleted: true})
	want := &syncrv1.Repository{
		Id: id.String(), Name: "Photos vault", Description: "family", RemoteId: remote.String(), Path: "restic/photos",
		Remote:   &syncrv1.JobRemoteRef{Id: remote.String(), Name: "B2 offsite", BackendType: "b2", Deleted: true},
		ResticId: "abc", FormatVersion: 2, CreatedAt: ts(convT0), UpdatedAt: ts(convT0.Add(time.Minute)), Version: 4,
		Deleting: true, DeletionJobId: job.String(), DeletionRequestedAt: ts(convT0.Add(time.Hour)),
		Stats: &syncrv1.RepositoryStats{
			SnapshotCount: 3, LatestSnapshotAt: ts(convT0), LatestSnapshotBytes: 12345, SnapshotsRefreshedAt: ts(convT0.Add(time.Minute)),
			SnapshotsStale: true, SnapshotsTruncated: true,
			LastBackup:    &syncrv1.RepositoryJobRef{JobId: backup.String(), Status: syncrv1.JobStatus_JOB_STATUS_SUCCEEDED, CreatedAt: ts(convT0), FinishedAt: ts(convT0.Add(2 * time.Minute))},
			LastCheck:     &syncrv1.RepositoryJobRef{JobId: check.String(), Status: syncrv1.JobStatus_JOB_STATUS_RUNNING, CreatedAt: ts(convT0)},
			ScheduleCount: 2,
		},
	}
	if !proto.Equal(got, want) {
		t.Errorf("repositoryToProto =\n%v\nwant\n%v", got, want)
	}
	// The message has no field that could carry the password.
	fields := got.ProtoReflect().Descriptor().Fields()
	for i := range fields.Len() {
		if name := string(fields.Get(i).Name()); strings.Contains(name, "password") || strings.Contains(name, "secret") {
			t.Errorf("Repository has the field %q", name)
		}
	}

	// After an attempt that did not delete the data, the outcome says how
	// it ended; while deleting, and without an error, there is none.
	for _, tt := range []struct {
		deleting, cancelled bool
		msg                 string
		want                syncrv1.RepositoryDeletionOutcome
	}{
		{false, false, "boom", syncrv1.RepositoryDeletionOutcome_REPOSITORY_DELETION_OUTCOME_FAILED},
		{false, true, "stopped", syncrv1.RepositoryDeletionOutcome_REPOSITORY_DELETION_OUTCOME_CANCELLED},
		{true, true, "stale", syncrv1.RepositoryDeletionOutcome_REPOSITORY_DELETION_OUTCOME_UNSPECIFIED},
		{false, true, "", syncrv1.RepositoryDeletionOutcome_REPOSITORY_DELETION_OUTCOME_UNSPECIFIED},
	} {
		ended := repositoryToProto(restic.Repo{ID: id, RemoteID: remote, Version: 1, Deleting: tt.deleting, DeletionJobID: job,
			DeletionError: tt.msg, DeletionCancelled: tt.cancelled}, remotes.Ref{})
		if ended.GetDeletionOutcome() != tt.want || ended.GetDeletionError() != tt.msg {
			t.Errorf("deleting=%v cancelled=%v error=%q: outcome %v, want %v", tt.deleting, tt.cancelled, tt.msg, ended.GetDeletionOutcome(), tt.want)
		}
	}

	// A repository that was never deleting or listed.
	plain := repositoryToProto(restic.Repo{ID: id, RemoteID: remote, Version: 1}, remotes.Ref{})
	if plain.GetDeletionJobId() != "" || plain.GetDeletionRequestedAt() != nil || plain.GetStats().GetLastBackup() != nil ||
		plain.GetStats().GetLastCheck() != nil || !plain.GetStats().GetSnapshotsStale() || plain.GetRemote().GetId() != remote.String() {
		t.Errorf("a plain repository = %v", plain)
	}
	if got := repositoryModeFromProto(syncrv1.RepositoryCreateMode_REPOSITORY_CREATE_MODE_INIT); got != restic.ModeInit {
		t.Errorf("mode INIT = %q", got)
	}
	if got := repositoryModeFromProto(syncrv1.RepositoryCreateMode_REPOSITORY_CREATE_MODE_CONNECT); got != restic.ModeConnect {
		t.Errorf("mode CONNECT = %q", got)
	}
	if got := repositoryModeFromProto(42); got != "" {
		t.Errorf("an unknown mode = %q", got)
	}
	if probeWorkerRef(domain.WorkerID{}, "x") != nil {
		t.Error("the zero worker has a reference")
	}
}
