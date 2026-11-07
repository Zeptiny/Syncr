package api

import (
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
)

func TestRunJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	resp, err := e.jobsFor(testUser).RunJob(t.Context(), connect.NewRequest(&syncrv1.RunJobRequest{WorkerId: e.worker.String(), Definition: testDefinition()}))
	if err != nil {
		t.Fatal(err)
	}
	j := resp.Msg.GetJob()
	if j.GetId() == "" || j.GetWorkerId() != e.worker.String() || j.GetKind() != "rclone_transfer" ||
		j.GetStatus() != syncrv1.JobStatus_JOB_STATUS_QUEUED || j.GetCancelRequested() ||
		!j.GetCreatedAt().AsTime().Equal(e.clock.Now()) || j.GetDispatchedAt() != nil || j.GetStartedAt() != nil ||
		j.GetFinishedAt() != nil || j.GetStats() != nil || j.GetError() != nil || j.GetOutput() != nil || j.ExitCode != nil {
		t.Errorf("job = %v", j)
	}
	// The caller owns the job, and the definition comes back as sent.
	if j.GetOwnerId() != testUser.UserID.String() || !proto.Equal(j.GetDefinition(), testDefinition()) {
		t.Errorf("owner %q, definition %v", j.GetOwnerId(), j.GetDefinition())
	}
	stored, err := e.jsvc.Get(t.Context(), e.jobID(j))
	if err != nil {
		t.Fatal(err)
	}
	if stored.OwnerID != testUser.UserID || stored.Kind != "rclone_transfer" {
		t.Errorf("stored job = %+v", stored)
	}
}

func TestRunJobWithoutKinds(t *testing.T) {
	t.Parallel()
	req := &syncrv1.RunJobRequest{Definition: testDefinition()}
	e := newEnv(t, func(d *Deps) { d.Kinds = nil })
	req.WorkerId = e.worker.String()
	_, err := e.jobsClient().RunJob(t.Context(), connect.NewRequest(req))
	wantCode(t, err, connect.CodeUnimplemented)

	empty, err := jobkind.NewRegistry(jobkind.Builtin()...)
	if err != nil {
		t.Fatal(err)
	}
	e = newEnv(t, func(d *Deps) { d.Kinds = empty })
	req.WorkerId = e.worker.String()
	_, err = e.jobsClient().RunJob(t.Context(), connect.NewRequest(req))
	wantCode(t, err, connect.CodeInvalidArgument)
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("err = %v", err)
	}
}

func TestRunJobErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.jobsClient()
	missing := domain.NewWorkerID().String()

	noSource := testDefinition()
	noSource.GetRcloneTransfer().Source.Path = ""
	tests := []struct {
		name  string
		req   *syncrv1.RunJobRequest
		want  connect.Code
		msg   string
		field string // the ValidationError field, if any
	}{
		{"no worker", &syncrv1.RunJobRequest{Definition: testDefinition()}, connect.CodeInvalidArgument, "worker_id", ""},
		{"bad worker", &syncrv1.RunJobRequest{WorkerId: "nope", Definition: testDefinition()}, connect.CodeInvalidArgument, "worker_id", ""},
		{"nil definition", &syncrv1.RunJobRequest{WorkerId: e.worker.String()}, connect.CodeInvalidArgument, "definition is required", "definition"},
		{"no operation", &syncrv1.RunJobRequest{WorkerId: e.worker.String(), Definition: &syncrv1.JobDefinition{}}, connect.CodeInvalidArgument, "operation", "definition.operation"},
		{"rejected by the kind", &syncrv1.RunJobRequest{WorkerId: e.worker.String(), Definition: noSource}, connect.CodeInvalidArgument, "source.path", "definition.rclone_transfer.source.path"},
		{"unknown worker", &syncrv1.RunJobRequest{WorkerId: missing, Definition: testDefinition()}, connect.CodeNotFound, "worker not found", ""},
	}
	for _, tt := range tests {
		_, err := c.RunJob(t.Context(), connect.NewRequest(tt.req))
		if err == nil || codeOf(t, err) != tt.want {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
			continue
		}
		if !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("%s: message %q lacks %q", tt.name, err, tt.msg)
		}
		if tt.field != "" {
			if v := violations(t, err); len(v) != 1 || v[0].GetField() != tt.field {
				t.Errorf("%s: violations = %v, want one on %s", tt.name, v, tt.field)
			}
		}
	}
}

func TestRequestSizeLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	def := testDefinition()
	def.GetRcloneTransfer().Source.Path = strings.Repeat("x", 2<<20)
	_, err := e.jobsClient().RunJob(t.Context(), connect.NewRequest(&syncrv1.RunJobRequest{WorkerId: e.worker.String(), Definition: def}))
	wantCode(t, err, connect.CodeResourceExhausted)
}

func TestGetJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.jobsClient()
	queued := e.runJob()

	resp, err := c.GetJob(t.Context(), connect.NewRequest(&syncrv1.GetJobRequest{JobId: queued.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(resp.Msg.GetJob(), queued) {
		t.Errorf("GetJob = %v, want %v", resp.Msg.GetJob(), queued)
	}

	for id, want := range map[string]connect.Code{
		"":                                     connect.CodeInvalidArgument,
		"junk":                                 connect.CodeInvalidArgument,
		"00000000-0000-0000-0000-000000000000": connect.CodeInvalidArgument,
		domain.NewJobID().String():             connect.CodeNotFound,
	} {
		_, err := c.GetJob(t.Context(), connect.NewRequest(&syncrv1.GetJobRequest{JobId: id}))
		if codeOf(t, err) != want || err == nil {
			t.Errorf("GetJob(%q) = %v, want %v", id, err, want)
		}
	}
}

// startRunning moves a job to running through the service methods the
// dispatcher uses.
func (e *testEnv) startRunning(j *syncrv1.Job) {
	e.t.Helper()
	if _, _, err := e.jsvc.MarkRunning(e.t.Context(), e.worker, e.jobID(j), testEpoch, e.clock.Now()); err != nil {
		e.t.Fatal(err)
	}
}

func TestJobStatsAndOutput(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.jobsClient()
	ctx := t.Context()
	get := func(j *syncrv1.Job) *syncrv1.Job {
		t.Helper()
		resp, err := c.GetJob(ctx, connect.NewRequest(&syncrv1.GetJobRequest{JobId: j.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetJob()
	}

	j := e.runJob()
	e.startRunning(j)
	running := get(j)
	if running.GetStatus() != syncrv1.JobStatus_JOB_STATUS_RUNNING || running.GetStats() != nil || running.GetStartedAt() == nil || running.GetDispatchedAt() == nil {
		t.Fatalf("running job without samples = %v", running)
	}

	// While running: the latest stored sample.
	if _, err := e.jsvc.RecordProgress(ctx, e.worker, e.jobID(j), testEpoch, jobs.Sample{Time: e.clock.Now(), Stats: jobs.Stats{Bytes: 10, TotalBytes: 100, Elapsed: 3 * time.Second}}); err != nil {
		t.Fatal(err)
	}
	if st := get(j).GetStats(); st.GetBytes() != 10 || st.GetTotalBytes() != 100 || st.GetElapsed().AsDuration() != 3*time.Second {
		t.Errorf("running stats = %v", st)
	}

	// Terminal: the final stats, exit code and output.
	out, err := proto.Marshal(&syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticBackup{ResticBackup: &syncrv1.ResticBackupOutput{SnapshotId: "abc123", FilesNew: 4}}})
	if err != nil {
		t.Fatal(err)
	}
	final := jobs.Stats{Bytes: 100, TotalBytes: 100, Files: 7, TotalFiles: 7, Checks: 1, Deletes: 2, Renames: 3, Errors: 4, Elapsed: 9 * time.Second}
	if _, _, err := e.jsvc.Finish(ctx, e.worker, e.jobID(j), testEpoch, jobs.Outcome{
		Status: domain.JobSucceeded, Stats: final, ExitCode: 0, Warnings: []string{"w1"}, Output: out,
	}); err != nil {
		t.Fatal(err)
	}
	done := get(j)
	st := done.GetStats()
	if done.GetStatus() != syncrv1.JobStatus_JOB_STATUS_SUCCEEDED || st.GetBytes() != 100 || st.GetFiles() != 7 || st.GetChecks() != 1 ||
		st.GetDeletes() != 2 || st.GetRenames() != 3 || st.GetErrors() != 4 || st.GetElapsed().AsDuration() != 9*time.Second ||
		done.ExitCode == nil || done.GetExitCode() != 0 || done.GetFinishedAt() == nil ||
		len(done.GetWarnings()) != 1 || done.GetOutput().GetResticBackup().GetSnapshotId() != "abc123" || done.GetError() != nil {
		t.Errorf("finished job = %v", done)
	}

	// A failed job: error code and message, exit code of the tool.
	f := e.runJob()
	e.startRunning(f)
	if _, _, err := e.jsvc.Finish(ctx, e.worker, e.jobID(f), testEpoch, jobs.Outcome{
		Status: domain.JobFailed, ExitCode: 3, Error: jobs.JobError{Code: "repository_locked", Message: "locked"},
	}); err != nil {
		t.Fatal(err)
	}
	failed := get(f)
	if failed.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_LOCKED || failed.GetError().GetMessage() != "locked" || failed.GetExitCode() != 3 {
		t.Errorf("failed job = %v", failed)
	}
}

func TestInterruptedJobKeepsLastSample(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	j := e.runJob()
	e.startRunning(j)
	if _, err := e.jsvc.RecordProgress(ctx, e.worker, e.jobID(j), testEpoch, jobs.Sample{Time: e.clock.Now(), Stats: jobs.Stats{Bytes: 77}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.jsvc.Interrupt(ctx, e.worker, e.jobID(j), testEpoch, jobs.JobError{Code: jobs.CodeWorkerLost, Message: "gone"}); err != nil {
		t.Fatal(err)
	}
	resp, err := e.jobsClient().GetJob(ctx, connect.NewRequest(&syncrv1.GetJobRequest{JobId: j.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Msg.GetJob()
	if got.GetStatus() != syncrv1.JobStatus_JOB_STATUS_INTERRUPTED || got.GetStats().GetBytes() != 77 ||
		got.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_WORKER_LOST || got.ExitCode != nil {
		t.Errorf("interrupted job = %v", got)
	}
}

func TestCancelJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.jobsClient()
	ctx := t.Context()

	// Queued: cancelled at once.
	q := e.runJob()
	resp, err := c.CancelJob(ctx, connect.NewRequest(&syncrv1.CancelJobRequest{JobId: q.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Msg.GetJob()
	if got.GetStatus() != syncrv1.JobStatus_JOB_STATUS_CANCELLED || got.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_CANCELLED || got.GetFinishedAt() == nil {
		t.Errorf("cancelled queued job = %v", got)
	}
	// Canceling again changes nothing.
	again, err := c.CancelJob(ctx, connect.NewRequest(&syncrv1.CancelJobRequest{JobId: q.GetId()}))
	if err != nil || !proto.Equal(again.Msg.GetJob(), got) {
		t.Errorf("second cancel = %v, %v", again, err)
	}

	// Running: a cancel is requested, the worker confirms later.
	r := e.runJob()
	e.startRunning(r)
	resp, err = c.CancelJob(ctx, connect.NewRequest(&syncrv1.CancelJobRequest{JobId: r.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Msg.GetJob(); got.GetStatus() != syncrv1.JobStatus_JOB_STATUS_RUNNING || !got.GetCancelRequested() {
		t.Errorf("cancel requested job = %v", got)
	}

	for id, want := range map[string]connect.Code{
		"x":                        connect.CodeInvalidArgument,
		"":                         connect.CodeInvalidArgument,
		domain.NewJobID().String(): connect.CodeNotFound,
	} {
		_, err := c.CancelJob(ctx, connect.NewRequest(&syncrv1.CancelJobRequest{JobId: id}))
		if err == nil || codeOf(t, err) != want {
			t.Errorf("CancelJob(%q) = %v, want %v", id, err, want)
		}
	}
}

func TestListJobsPagination(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.jobsClient()
	ctx := t.Context()

	const total = 23
	var created []string // oldest first
	for range total {
		created = append(created, e.runJob().GetId())
	}

	var got []string
	token := ""
	pages := 0
	for {
		resp, err := c.ListJobs(ctx, connect.NewRequest(&syncrv1.ListJobsRequest{PageSize: 5, PageToken: token}))
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if n := len(resp.Msg.GetJobs()); n > 5 || n == 0 {
			t.Fatalf("page %d has %d jobs", pages, n)
		}
		for _, j := range resp.Msg.GetJobs() {
			got = append(got, j.GetId())
		}
		token = resp.Msg.GetNextPageToken()
		if token == "" {
			break
		}
		if pages > total {
			t.Fatal("pagination does not end")
		}
	}
	if pages != 5 || len(got) != total {
		t.Fatalf("pages = %d, jobs = %d", pages, len(got))
	}
	// Newest first, each exactly once.
	for i, id := range got {
		if want := created[total-1-i]; id != want {
			t.Fatalf("job %d = %s, want %s", i, id, want)
		}
	}

	// Default and oversized page sizes work and are clamped, not rejected.
	resp, err := c.ListJobs(ctx, connect.NewRequest(&syncrv1.ListJobsRequest{}))
	if err != nil || len(resp.Msg.GetJobs()) != total || resp.Msg.GetNextPageToken() != "" {
		t.Errorf("default page = %d jobs, token %q, %v", len(resp.Msg.GetJobs()), resp.Msg.GetNextPageToken(), err)
	}
	resp, err = c.ListJobs(ctx, connect.NewRequest(&syncrv1.ListJobsRequest{PageSize: 1 << 30}))
	if err != nil || len(resp.Msg.GetJobs()) != total {
		t.Errorf("huge page size: %d jobs, %v", len(resp.Msg.GetJobs()), err)
	}
	_, err = c.ListJobs(ctx, connect.NewRequest(&syncrv1.ListJobsRequest{PageSize: -1}))
	wantCode(t, err, connect.CodeInvalidArgument)
}

func TestListJobsBadPageTokens(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.runJob()
	for _, token := range []string{
		"garbage!",
		"AAAA",
		encodePageToken(domain.NewJobID())[:10],
		strings.Repeat("A", 500),
		"anot:" + domain.NewJobID().String(),
		"====",
	} {
		_, err := e.jobsClient().ListJobs(t.Context(), connect.NewRequest(&syncrv1.ListJobsRequest{PageToken: token}))
		if err == nil || codeOf(t, err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "page_token") {
			t.Errorf("token %q: err = %v, want InvalidArgument", token, err)
		}
	}
}

func TestListJobsFilters(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.jobsClient()
	ctx := t.Context()
	other := e.addWorker()

	a, b := e.runJob(), e.runJob()
	e.startRunning(b)
	resp, err := c.RunJob(ctx, connect.NewRequest(&syncrv1.RunJobRequest{WorkerId: other.String(), Definition: testDefinition()}))
	if err != nil {
		t.Fatal(err)
	}
	o := resp.Msg.GetJob()

	list := func(req *syncrv1.ListJobsRequest) []string {
		t.Helper()
		resp, err := c.ListJobs(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, j := range resp.Msg.GetJobs() {
			ids = append(ids, j.GetId())
		}
		return ids
	}
	same := func(got []string, want ...string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("got %v, want %v", got, want)
		}
	}
	same(list(&syncrv1.ListJobsRequest{WorkerId: e.worker.String()}), b.GetId(), a.GetId())
	same(list(&syncrv1.ListJobsRequest{WorkerId: other.String()}), o.GetId())
	same(list(&syncrv1.ListJobsRequest{Statuses: []syncrv1.JobStatus{syncrv1.JobStatus_JOB_STATUS_RUNNING}}), b.GetId())
	same(list(&syncrv1.ListJobsRequest{Statuses: []syncrv1.JobStatus{syncrv1.JobStatus_JOB_STATUS_QUEUED, syncrv1.JobStatus_JOB_STATUS_RUNNING}, WorkerId: e.worker.String()}), b.GetId(), a.GetId())
	same(list(&syncrv1.ListJobsRequest{Statuses: []syncrv1.JobStatus{syncrv1.JobStatus_JOB_STATUS_FAILED}}))

	for name, req := range map[string]*syncrv1.ListJobsRequest{
		"unspecified status": {Statuses: []syncrv1.JobStatus{syncrv1.JobStatus_JOB_STATUS_UNSPECIFIED}},
		"unknown status":     {Statuses: []syncrv1.JobStatus{99}},
		"bad worker id":      {WorkerId: "nope"},
	} {
		_, err := c.ListJobs(ctx, connect.NewRequest(req))
		if err == nil || codeOf(t, err) != connect.CodeInvalidArgument {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
