package api

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
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
	j := jobs.Job{
		ID: domain.NewJobID(), WorkerID: domain.NewWorkerID(), Kind: "restic_forget", Status: domain.JobFailed,
		SpecEnc: []byte(specSecret), Epoch: "e1", CancelRequestedAt: convT0,
		CreatedAt: convT0, QueuedAt: convT0.Add(time.Second), DispatchedAt: convT0.Add(2 * time.Second),
		StartedAt: convT0.Add(3 * time.Second), FinishedAt: convT0.Add(4 * time.Second),
		Requeues: 2, Error: jobs.JobError{Code: "wrong_password", Message: "nope"}, Warnings: []string{"a", "b"},
		ExitCode: 12, Output: out, Version: 7,
	}
	stats := &jobs.Stats{Bytes: 1, TotalBytes: 2, Files: 3, TotalFiles: 4, Checks: 5, Deletes: 6, Renames: 7, Errors: 8, Elapsed: time.Minute}
	got, err := jobToProto(j, stats)
	if err != nil {
		t.Fatal(err)
	}
	exit := int32(12)
	want := &syncrv1.Job{
		Id: j.ID.String(), WorkerId: j.WorkerID.String(), Kind: "restic_forget", Status: syncrv1.JobStatus_JOB_STATUS_FAILED, CancelRequested: true,
		CreatedAt: ts(convT0), DispatchedAt: ts(convT0.Add(2 * time.Second)), StartedAt: ts(convT0.Add(3 * time.Second)), FinishedAt: ts(convT0.Add(4 * time.Second)),
		Requeues: 2,
		Stats:    &syncrv1.JobStats{Bytes: 1, TotalBytes: 2, Files: 3, TotalFiles: 4, Checks: 5, Deletes: 6, Renames: 7, Errors: 8, Elapsed: statsToProto(jobs.Stats{Elapsed: time.Minute}).GetElapsed()},
		Error:    &syncrv1.JobError{Code: syncrv1.JobErrorCode_JOB_ERROR_CODE_WRONG_PASSWORD, Message: "nope"},
		Warnings: []string{"a", "b"}, ExitCode: &exit,
		Output: &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticForget{ResticForget: &syncrv1.ResticForgetOutput{}}},
	}
	if !proto.Equal(got, want) {
		t.Errorf("jobToProto =\n%v\nwant\n%v", got, want)
	}
}

func TestJobToProtoEdges(t *testing.T) {
	t.Parallel()
	base := jobs.Job{ID: domain.NewJobID(), WorkerID: domain.NewWorkerID(), Kind: "k", Status: domain.JobQueued, ExitCode: -1, CreatedAt: convT0}

	got, err := jobToProto(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetStats() != nil || got.GetError() != nil || got.GetOutput() != nil || got.ExitCode != nil || got.GetDispatchedAt() != nil ||
		got.GetStartedAt() != nil || got.GetFinishedAt() != nil || got.GetCancelRequested() {
		t.Errorf("queued job = %v", got)
	}

	// An unknown error code keeps its message and maps to UNSPECIFIED.
	base.Error = jobs.JobError{Code: "made_up", Message: "m"}
	if got, _ := jobToProto(base, nil); got.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSPECIFIED || got.GetError().GetMessage() != "m" {
		t.Errorf("error = %v", got.GetError())
	}
	// Controller-set codes map too.
	for _, code := range []string{jobs.CodeCancelled, jobs.CodeWorkerShutdown, jobs.CodeDispatchTimeout, jobs.CodeWorkerRestarted, jobs.CodeWorkerLost, jobs.CodeWorkerRemoved, jobs.CodeInternal} {
		base.Error = jobs.JobError{Code: code}
		if got, _ := jobToProto(base, nil); got.GetError().GetCode() == syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSPECIFIED {
			t.Errorf("code %q has no wire code", code)
		}
	}

	// Exit code 0 of a finished job is set (not confused with unset); the
	// exit code of an unfinished job is not reported.
	base.Status, base.ExitCode = domain.JobSucceeded, 0
	if got, _ := jobToProto(base, nil); got.ExitCode == nil || got.GetExitCode() != 0 {
		t.Errorf("exit code = %v", got.ExitCode)
	}
	base.Status = domain.JobRunning
	if got, _ := jobToProto(base, nil); got.ExitCode != nil {
		t.Errorf("exit code of a running job = %v", got.ExitCode)
	}

	// A corrupt output is reported but the job is still returned.
	base.Output = []byte{0xff, 0xff, 0xff}
	got, err = jobToProto(base, nil)
	if err == nil || got == nil || got.GetOutput() != nil || got.GetId() != base.ID.String() {
		t.Errorf("corrupt output: %v, %v", got, err)
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

func TestMarshalSpecIsDeterministic(t *testing.T) {
	t.Parallel()
	spec := testSpec()
	spec.Remotes[0].Options = map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5", "f": "6"}
	first, err := marshalSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		again, err := marshalSpec(spec)
		if err != nil || string(again) != string(first) {
			t.Fatalf("marshalSpec differs between calls: %v", err)
		}
	}
}
