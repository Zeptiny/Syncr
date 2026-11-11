package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/workers"
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
	stored, err := e.jsvc.Get(t.Context(), testUser, e.jobID(j))
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

	empty, err := jobkind.NewRegistry()
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

// countingJobs counts the LatestSamples lookups of the handler.
type countingJobs struct {
	JobService
	mu    sync.Mutex
	calls [][]domain.JobID
}

func (c *countingJobs) LatestSamples(ctx context.Context, ids []domain.JobID) (map[domain.JobID]jobs.Sample, error) {
	c.mu.Lock()
	c.calls = append(c.calls, slices.Clone(ids))
	c.mu.Unlock()
	return c.JobService.LatestSamples(ctx, ids)
}

func TestListJobsReadsLatestSamplesInOneLookup(t *testing.T) {
	t.Parallel()
	counter := &countingJobs{}
	e := newEnv(t, func(d *Deps) { counter.JobService = d.Jobs; d.Jobs = counter })
	ctx := t.Context()
	progress := func(j *syncrv1.Job, bytes int64) {
		t.Helper()
		if _, err := e.jsvc.RecordProgress(ctx, e.worker, e.jobID(j), testEpoch, jobs.Sample{
			Time: e.clock.Now(), Stats: jobs.Stats{Bytes: bytes},
		}); err != nil {
			t.Fatal(err)
		}
	}
	queued := e.runJob()
	running := e.runJob()
	e.startRunning(running)
	progress(running, 10)
	idle := e.runJob() // running, no samples yet
	e.startRunning(idle)
	interrupted := e.runJob()
	e.startRunning(interrupted)
	progress(interrupted, 20)
	if _, _, err := e.jsvc.Interrupt(ctx, e.worker, e.jobID(interrupted), testEpoch, jobs.JobError{Code: jobs.CodeWorkerLost}); err != nil {
		t.Fatal(err)
	}
	done := e.runJob()
	e.startRunning(done)
	progress(done, 30)
	if _, _, err := e.jsvc.Finish(ctx, e.worker, e.jobID(done), testEpoch, jobs.Outcome{Status: domain.JobSucceeded, Stats: jobs.Stats{Bytes: 33}}); err != nil {
		t.Fatal(err)
	}

	counter.mu.Lock()
	counter.calls = nil
	counter.mu.Unlock()
	resp, err := e.jobsClient().ListJobs(ctx, connect.NewRequest(&syncrv1.ListJobsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*syncrv1.JobStats{}
	for _, j := range resp.Msg.GetJobs() {
		got[j.GetId()] = j.GetStats()
	}
	for _, tt := range []struct {
		job   *syncrv1.Job
		stats bool
		bytes int64
	}{{queued, false, 0}, {running, true, 10}, {idle, false, 0}, {interrupted, true, 20}, {done, true, 33}} {
		st, ok := got[tt.job.GetId()]
		if !ok || (st != nil) != tt.stats || st.GetBytes() != tt.bytes {
			t.Errorf("job %s: stats = %v (listed %v), want bytes %d", tt.job.GetId(), st, ok, tt.bytes)
		}
	}
	counter.mu.Lock()
	defer counter.mu.Unlock()
	if len(counter.calls) != 1 || len(counter.calls[0]) != 3 {
		t.Errorf("LatestSamples calls = %v, want one for the running, idle and interrupted jobs", counter.calls)
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

// fakeJobRemotes stands in for the remotes service of the job tests: the
// jobs service's RemoteChecker (live remotes of the owner are usable) and
// the Names of Deps.Remotes. The other RemoteService methods are not
// served (the embedded nil interface panics, which the handler recovers).
type fakeJobRemotes struct {
	RemoteService

	mu    sync.Mutex
	refs  map[domain.RemoteID]remotes.Ref
	owner map[domain.RemoteID]domain.UserID
	// namesErr, if set, fails Names.
	namesErr error
}

func newFakeJobRemotes() *fakeJobRemotes {
	return &fakeJobRemotes{refs: map[domain.RemoteID]remotes.Ref{}, owner: map[domain.RemoteID]domain.UserID{}}
}

// add creates a live remote of owner and returns its ID.
func (f *fakeJobRemotes) add(owner domain.Actor, name, typ string) domain.RemoteID {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := domain.NewRemoteID()
	f.refs[id] = remotes.Ref{ID: id, Name: name, Type: typ}
	f.owner[id] = owner.UserID
	return id
}

// remove deletes a remote (soft: it keeps its name).
func (f *fakeJobRemotes) remove(id domain.RemoteID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.refs[id]
	r.Deleted = true
	f.refs[id] = r
}

func (f *fakeJobRemotes) Usable(_ context.Context, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[domain.RemoteID]string{}
	for _, id := range ids {
		r, ok := f.refs[id]
		if !ok || r.Deleted || f.owner[id] != owner {
			return nil, fmt.Errorf("remote %s: %w", id, domain.ErrNotFound)
		}
		out[id] = r.Name
	}
	return out, nil
}

func (f *fakeJobRemotes) Names(_ context.Context, ids []domain.RemoteID) (map[domain.RemoteID]remotes.Ref, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.namesErr != nil {
		return nil, f.namesErr
	}
	out := map[domain.RemoteID]remotes.Ref{}
	for _, id := range ids {
		if r, ok := f.refs[id]; ok {
			out[id] = r
		}
	}
	return out, nil
}

// storeRemote makes id exist in the store too (the jobs' foreign keys).
func (e *testEnv) storeRemote(owner domain.Actor, id domain.RemoteID) {
	e.t.Helper()
	r := remotes.Remote{ID: id, OwnerID: owner.UserID, Name: "r-" + id.String()[30:], Type: "s3", Version: 1}
	if err := e.store.CreateRemote(e.t.Context(), r); err != nil {
		e.t.Fatal(err)
	}
}

// addRemote adds a remote of owner to the fake service and the store.
func (e *testEnv) addRemote(owner domain.Actor, name, typ string) domain.RemoteID {
	e.t.Helper()
	id := e.remotes.add(owner, name, typ)
	e.storeRemote(owner, id)
	return id
}

// remoteDefinition syncs path on remote src to the worker's /restore.
func remoteDefinition(src domain.RemoteID, path string) *syncrv1.JobDefinition {
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransferDefinition{
		Mode:        syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_SYNC,
		Source:      &syncrv1.JobLocation{RemoteId: src.String(), Path: path},
		Destination: &syncrv1.JobLocation{Path: "/restore"},
	}}}
}

func (e *testEnv) runAs(a domain.Actor, def *syncrv1.JobDefinition) (*syncrv1.Job, error) {
	e.t.Helper()
	resp, err := e.jobsFor(a).RunJob(e.t.Context(), connect.NewRequest(&syncrv1.RunJobRequest{WorkerId: e.worker.String(), Definition: def}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetJob(), nil
}

func TestRunJobWithRemotes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	photos := e.addRemote(testUser, "Photos", "sftp")
	admins := e.addRemote(testAdmin, "Admin's", "s3")

	j, err := e.runAs(testUser, remoteDefinition(photos, "2026"))
	if err != nil {
		t.Fatal(err)
	}
	want := []*syncrv1.JobRemoteRef{{Id: photos.String(), Name: "Photos", BackendType: "sftp"}}
	if len(j.GetRemotes()) != 1 || !proto.Equal(j.GetRemotes()[0], want[0]) {
		t.Errorf("remotes = %v, want %v", j.GetRemotes(), want)
	}
	stored, err := e.jsvc.Get(t.Context(), testUser, e.jobID(j))
	if err != nil || !slices.Equal(stored.RemoteIDs, []domain.RemoteID{photos}) || !strings.Contains(stored.SearchText, "photos") || !strings.Contains(stored.SearchText, "2026") {
		t.Errorf("stored job = remotes %v, text %q (%v)", stored.RemoteIDs, stored.SearchText, err)
	}

	// A deleted remote keeps its name in the job's references.
	e.remotes.remove(photos)
	got, err := e.jobsFor(testUser).GetJob(t.Context(), connect.NewRequest(&syncrv1.GetJobRequest{JobId: j.GetId()}))
	if err != nil || len(got.Msg.GetJob().GetRemotes()) != 1 || !got.Msg.GetJob().GetRemotes()[0].GetDeleted() || got.Msg.GetJob().GetRemotes()[0].GetName() != "Photos" {
		t.Errorf("GetJob after the remote was deleted = %v (%v)", got, err)
	}

	// Remotes must be the caller's live remotes, admins included.
	for name, tt := range map[string]struct {
		actor domain.Actor
		def   *syncrv1.JobDefinition
	}{
		"deleted remote":        {testUser, remoteDefinition(photos, "")},
		"another user's remote": {testUser, remoteDefinition(admins, "")},
		"an admin, a user's":    {testAdmin, remoteDefinition(photos, "")},
		"unknown remote":        {testUser, remoteDefinition(domain.NewRemoteID(), "")},
	} {
		_, err := e.runAs(tt.actor, tt.def)
		wantCode(t, err, connect.CodeNotFound)
		if !strings.Contains(err.Error(), "remote not found") {
			t.Errorf("%s: err = %v", name, err)
		}
	}

	// A failing lookup of the names fails the call, without details.
	e.remotes.namesErr = errors.New("db exploded: password=hunter2")
	_, err = e.jobsFor(testUser).GetJob(t.Context(), connect.NewRequest(&syncrv1.GetJobRequest{JobId: j.GetId()}))
	wantCode(t, err, connect.CodeInternal)
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("internal error leaks: %v", err)
	}
}

// TestJobOwnerScoping: user B gets NotFound for user A's job on every
// procedure (get, cancel, watch, samples) and does not see it in lists;
// the admin views and cancels it, and lists it with all_users only.
func TestJobOwnerScoping(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	aJob, err := e.runAs(testUser, testDefinition())
	if err != nil {
		t.Fatal(err)
	}
	adminJob := e.runJob()
	e.startRunning(aJob)
	if _, err := e.jsvc.RecordProgress(ctx, e.worker, e.jobID(aJob), testEpoch, jobs.Sample{Time: e.clock.Now(), Stats: jobs.Stats{Bytes: 5}}); err != nil {
		t.Fatal(err)
	}
	id := aJob.GetId()

	b := e.jobsFor(testOther)
	_, err = b.GetJob(ctx, connect.NewRequest(&syncrv1.GetJobRequest{JobId: id}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = b.CancelJob(ctx, connect.NewRequest(&syncrv1.CancelJobRequest{JobId: id}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = b.ListJobSamples(ctx, connect.NewRequest(&syncrv1.ListJobSamplesRequest{JobId: id}))
	wantCode(t, err, connect.CodeNotFound)
	stream, err := b.WatchJob(ctx, connect.NewRequest(&syncrv1.WatchJobRequest{JobId: id}))
	if err != nil {
		t.Fatal(err)
	}
	if stream.Receive() {
		t.Errorf("user B received %v", stream.Msg())
	}
	wantCode(t, stream.Err(), connect.CodeNotFound)
	_ = stream.Close()
	if stored, _ := e.jsvc.Get(ctx, testUser, e.jobID(aJob)); stored.CancelRequested() {
		t.Error("user B's cancel reached the job")
	}

	list := func(a domain.Actor, all bool) []string {
		t.Helper()
		resp, err := e.jobsFor(a).ListJobs(ctx, connect.NewRequest(&syncrv1.ListJobsRequest{AllUsers: all}))
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, j := range resp.Msg.GetJobs() {
			ids = append(ids, j.GetId())
		}
		return ids
	}
	same := func(what string, got []string, want ...string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", what, got, want)
		}
	}
	same("user B", list(testOther, false))
	same("user B, all users", list(testOther, true))
	same("user A", list(testUser, false), id)
	same("admin", list(testAdmin, false), adminJob.GetId())
	same("admin, all users", list(testAdmin, true), adminJob.GetId(), id)

	// The admin sees and cancels user A's job.
	a := e.jobsFor(testAdmin)
	got, err := a.GetJob(ctx, connect.NewRequest(&syncrv1.GetJobRequest{JobId: id}))
	if err != nil || got.Msg.GetJob().GetOwnerId() != testUser.UserID.String() {
		t.Errorf("admin GetJob = %v (%v)", got, err)
	}
	if smp, err := a.ListJobSamples(ctx, connect.NewRequest(&syncrv1.ListJobSamplesRequest{JobId: id})); err != nil || len(smp.Msg.GetSamples()) != 1 {
		t.Errorf("admin ListJobSamples = %v (%v)", smp, err)
	}
	cancelled, err := a.CancelJob(ctx, connect.NewRequest(&syncrv1.CancelJobRequest{JobId: id}))
	if err != nil || !cancelled.Msg.GetJob().GetCancelRequested() {
		t.Errorf("admin CancelJob = %v (%v)", cancelled, err)
	}
}

func TestListJobSamples(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	j := e.runJob()
	c := e.jobsClient()
	resp, err := c.ListJobSamples(ctx, connect.NewRequest(&syncrv1.ListJobSamplesRequest{JobId: j.GetId()}))
	if err != nil || len(resp.Msg.GetSamples()) != 0 {
		t.Fatalf("samples of a queued job = %v (%v)", resp, err)
	}
	e.startRunning(j)
	t0 := e.clock.Now()
	for i := range 4 { // one stored per 5 s
		smp := jobs.Sample{Time: t0.Add(time.Duration(i) * 3 * time.Second), Stats: jobs.Stats{Bytes: int64(i) * 100, Elapsed: time.Duration(i) * time.Second}}
		if _, err := e.jsvc.RecordProgress(ctx, e.worker, e.jobID(j), testEpoch, smp); err != nil {
			t.Fatal(err)
		}
	}
	resp, err = c.ListJobSamples(ctx, connect.NewRequest(&syncrv1.ListJobSamplesRequest{JobId: j.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range resp.Msg.GetSamples() {
		got = append(got, fmt.Sprintf("%s:%d", s.GetTime().AsTime().Sub(t0), s.GetStats().GetBytes()))
	}
	if want := []string{"0s:0", "6s:200"}; !slices.Equal(got, want) {
		t.Errorf("samples = %v, want %v", got, want)
	}
	for id, want := range map[string]connect.Code{"": connect.CodeInvalidArgument, "x": connect.CodeInvalidArgument, domain.NewJobID().String(): connect.CodeNotFound} {
		_, err := c.ListJobSamples(ctx, connect.NewRequest(&syncrv1.ListJobSamplesRequest{JobId: id}))
		wantCode(t, err, want)
	}
}

func TestListJobsSearchFilters(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	photos, archive := e.addRemote(testUser, "Holiday Photos", "sftp"), e.addRemote(testUser, "Archive_2026", "b2")
	run := func(def *syncrv1.JobDefinition) *syncrv1.Job {
		t.Helper()
		e.clock.Advance(time.Minute)
		j, err := e.runAs(testUser, def)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	j1 := run(remoteDefinition(photos, "albums/greece"))
	j2 := run(remoteDefinition(archive, "tax"))
	j3 := run(testDefinition())
	c := e.jobsFor(testUser)
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
	ts := func(j *syncrv1.Job) *timestamppb.Timestamp { return j.GetCreatedAt() }
	tests := []struct {
		name string
		req  *syncrv1.ListJobsRequest
		want []string
	}{
		{"remote", &syncrv1.ListJobsRequest{RemoteId: photos.String()}, []string{j1.GetId()}},
		{"kinds", &syncrv1.ListJobsRequest{Kinds: []string{"rclone_transfer"}}, []string{j3.GetId(), j2.GetId(), j1.GetId()}},
		{"other kind", &syncrv1.ListJobsRequest{Kinds: []string{"restic_backup"}}, nil},
		{"query on a remote name", &syncrv1.ListJobsRequest{Query: "HOLIDAY"}, []string{j1.GetId()}},
		{"query on a path", &syncrv1.ListJobsRequest{Query: "greece"}, []string{j1.GetId()}},
		{"query on the mode", &syncrv1.ListJobsRequest{Query: "copy"}, []string{j3.GetId()}},
		{"query with an underscore", &syncrv1.ListJobsRequest{Query: "archive_"}, []string{j2.GetId()}},
		{"query with a percent", &syncrv1.ListJobsRequest{Query: "%"}, nil},
		{"created after", &syncrv1.ListJobsRequest{CreatedAfter: ts(j2)}, []string{j3.GetId(), j2.GetId()}},
		{"created before", &syncrv1.ListJobsRequest{CreatedBefore: ts(j2)}, []string{j1.GetId()}},
		{"combined", &syncrv1.ListJobsRequest{RemoteId: archive.String(), Query: "tax", CreatedAfter: ts(j1), WorkerId: e.worker.String()}, []string{j2.GetId()}},
	}
	for _, tt := range tests {
		if got := list(tt.req); !slices.Equal(got, tt.want) {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}
	for name, req := range map[string]*syncrv1.ListJobsRequest{
		"bad remote id":     {RemoteId: "photos"},
		"bad kind":          {Kinds: []string{"Rclone Transfer"}},
		"long query":        {Query: strings.Repeat("q", 300)},
		"bad created_after": {CreatedAfter: &timestamppb.Timestamp{Seconds: 1 << 62}},
	} {
		_, err := c.ListJobs(ctx, connect.NewRequest(req))
		wantCode(t, err, connect.CodeInvalidArgument)
		if name == "bad remote id" && len(violations(t, err)) != 1 {
			t.Errorf("%s: no field violation", name)
		}
	}
}

// setWorkerOS makes the env's worker report an operating system.
func (e *testEnv) setWorkerOS(os string) {
	e.t.Helper()
	if err := e.store.SetWorkerInfo(e.t.Context(), e.worker, "", workers.Info{Version: "v2", OS: os, ReportedAt: e.clock.Now()}); err != nil {
		e.t.Fatal(err)
	}
}

// TestRunResticJobs: a restic job refers to one of the caller's
// repositories, is titled by its kind, holds the repository's concurrency
// key (so a second one waits for the first) and shows its kind's details
// once it has an output.
func TestRunResticJobs(t *testing.T) {
	t.Parallel()
	e := newResticEnv(t)
	ctx := t.Context()
	c := e.jobsFor(testUser)
	repo := e.createRepo("Photos vault")
	get := func(id string) *syncrv1.Job {
		t.Helper()
		resp, err := c.GetJob(ctx, connect.NewRequest(&syncrv1.GetJobRequest{JobId: id}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetJob()
	}

	first, err := e.runAs(testUser, apiBackupDef(repo.GetId()))
	if err != nil {
		t.Fatal(err)
	}
	if first.GetKind() != "restic_backup" || first.GetTitle() != "Back up /srv/photos → Photos vault" ||
		first.GetRepository().GetId() != repo.GetId() || first.GetRepository().GetName() != "Photos vault" || first.GetRepository().GetDeleted() ||
		first.GetWaitingForJobId() != "" || len(first.GetDetails()) != 0 {
		t.Errorf("the first backup = %v", first)
	}
	// The repository's remote is one of the job's remotes.
	if rs := first.GetRemotes(); len(rs) != 1 || rs[0].GetId() != e.remote.String() || rs[0].GetName() != "B2 offsite" {
		t.Errorf("remotes of a backup = %v", rs)
	}
	stored, err := e.store.GetJob(ctx, e.jobID(first))
	if err != nil || stored.RepositoryID.String() != repo.GetId() || !slices.Equal(stored.ConcurrencyKeys, []string{"restic-repo:" + repo.GetId()}) {
		t.Errorf("the stored job: repository %s, keys %v, %v", stored.RepositoryID, stored.ConcurrencyKeys, err)
	}

	// A second job on the repository waits for the first, whatever its
	// kind.
	e.clock.Advance(time.Second)
	check := &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticCheck{ResticCheck: &syncrv1.ResticCheckDefinition{RepositoryId: repo.GetId(), ReadDataPercent: 10}}}
	second, err := e.runAs(testUser, check)
	if err != nil {
		t.Fatal(err)
	}
	if second.GetWaitingForJobId() != first.GetId() || second.GetTitle() != "Check Photos vault (read 10% of the data)" {
		t.Errorf("the second job = waiting for %q, title %q", second.GetWaitingForJobId(), second.GetTitle())
	}
	list, err := c.ListJobs(ctx, connect.NewRequest(&syncrv1.ListJobsRequest{RepositoryId: repo.GetId()}))
	if err != nil || len(list.Msg.GetJobs()) != 2 || list.Msg.GetJobs()[0].GetWaitingForJobId() != first.GetId() || list.Msg.GetJobs()[1].GetWaitingForJobId() != "" {
		t.Errorf("ListJobs(repository) = %v, %v", list, err)
	}
	// A transfer is no job of the repository.
	plain, err := e.runAs(testUser, testDefinition())
	if err != nil || plain.GetRepository() != nil || plain.GetTitle() != "Copy /data/src → /data/dst" || plain.GetWaitingForJobId() != "" {
		t.Errorf("a transfer = %v, %v", plain, err)
	}
	if list, err := c.ListJobs(ctx, connect.NewRequest(&syncrv1.ListJobsRequest{RepositoryId: repo.GetId()})); err != nil || len(list.Msg.GetJobs()) != 2 {
		t.Errorf("ListJobs(repository) with a transfer around = %v, %v", list, err)
	}
	if list, err := c.ListJobs(ctx, connect.NewRequest(&syncrv1.ListJobsRequest{RepositoryId: domain.NewResticRepoID().String()})); err != nil || len(list.Msg.GetJobs()) != 0 {
		t.Errorf("ListJobs(another repository) = %v, %v", list, err)
	}
	_, err = c.ListJobs(ctx, connect.NewRequest(&syncrv1.ListJobsRequest{RepositoryId: "nope"}))
	if codeOf(t, err) != connect.CodeInvalidArgument || violation(err, "repository_id") == "" {
		t.Errorf("ListJobs(malformed repository) = %v", err)
	}

	// Finished: the kind's details; the next job no longer waits.
	e.startRunning(first)
	out, err := proto.Marshal(&syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticBackup{ResticBackup: &syncrv1.ResticBackupOutput{
		SnapshotId: "1a2b3c4d5e6f", FilesNew: 12, FilesUnmodified: 1204, DataAddedBytes: 2048, TotalFilesProcessed: 1216, TotalBytesProcessed: 5 << 30,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.jsvc.Finish(ctx, e.worker, e.jobID(first), testEpoch, jobs.Outcome{Status: domain.JobSucceeded, Output: out}); err != nil {
		t.Fatal(err)
	}
	done := get(first.GetId())
	want := []*syncrv1.JobDetail{
		{Label: "Snapshot", Value: "1a2b3c4d"},
		{Label: "Files", Value: "12 new, 0 changed, 1,204 unmodified"},
		{Label: "Folders", Value: "0 new, 0 changed, 0 unmodified"},
		{Label: "Data added", Value: "2.0 KiB"},
		{Label: "Processed", Value: "5.0 GiB in 1,216 files"},
	}
	if len(done.GetDetails()) != len(want) {
		t.Fatalf("details = %v", done.GetDetails())
	}
	for i, w := range want {
		if !proto.Equal(done.GetDetails()[i], w) {
			t.Errorf("detail %d = %v, want %v", i, done.GetDetails()[i], w)
		}
	}
	if got := get(second.GetId()); got.GetWaitingForJobId() != "" {
		t.Errorf("the second job still waits for %s", got.GetWaitingForJobId())
	}

	// An admin sees the job with its repository's name; a deleted
	// repository stays named.
	resp, err := e.jobsFor(testAdmin).GetJob(ctx, connect.NewRequest(&syncrv1.GetJobRequest{JobId: first.GetId()}))
	if err != nil || resp.Msg.GetJob().GetRepository().GetName() != "Photos vault" {
		t.Errorf("an admin's view = %v, %v", resp, err)
	}
	if _, err := e.jsvc.Cancel(ctx, testUser, e.jobID(second)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.resticFor(testUser).DeleteRepository(ctx, connect.NewRequest(&syncrv1.DeleteRepositoryRequest{RepositoryId: repo.GetId()})); err != nil {
		t.Fatal(err)
	}
	if got := get(first.GetId()); !got.GetRepository().GetDeleted() || got.GetRepository().GetName() != "Photos vault" || got.GetTitle() != "Back up /srv/photos → Photos vault" {
		t.Errorf("a job of a deleted repository = %v", got.GetRepository())
	}
	_, err = e.runAs(testUser, apiBackupDef(repo.GetId()))
	if codeOf(t, err) != connect.CodeNotFound || !strings.Contains(err.Error(), "repository not found") {
		t.Errorf("RunJob on a deleted repository = %v", err)
	}
}

func TestRunResticJobRejections(t *testing.T) {
	t.Parallel()
	e := newResticEnv(t)
	repo := e.createRepo("Photos vault")

	// Only deleting the repository starts a data deletion.
	_, err := e.runAs(testUser, &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticDeleteData{
		ResticDeleteData: &syncrv1.ResticDeleteDataDefinition{RepositoryId: repo.GetId()},
	}})
	if codeOf(t, err) != connect.CodeInvalidArgument || violation(err, "definition.restic_delete_data") != "is started by deleting the repository" {
		t.Errorf("RunJob(restic_delete_data) = %v", err)
	}
	// Admins included.
	_, err = e.runAs(testAdmin, &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticDeleteData{
		ResticDeleteData: &syncrv1.ResticDeleteDataDefinition{RepositoryId: repo.GetId()},
	}})
	wantCode(t, err, connect.CodeInvalidArgument)

	// The kind's field errors.
	_, err = e.runAs(testUser, &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticBackup{ResticBackup: &syncrv1.ResticBackupDefinition{RepositoryId: repo.GetId()}}})
	if violation(err, "definition.restic_backup.paths") == "" {
		t.Errorf("a backup without paths = %v", err)
	}
	// Unknown and foreign repositories look the same.
	_, err = e.runAs(testUser, apiBackupDef(domain.NewResticRepoID().String()))
	wantCode(t, err, connect.CodeNotFound)
	_, err = e.runAs(testOther, apiBackupDef(repo.GetId()))
	wantCode(t, err, connect.CodeNotFound)
	if n, _ := e.jsvc.Unfinished(t.Context(), e.worker); len(n) != 0 {
		t.Errorf("a rejected RunJob queued %d jobs", len(n))
	}

	// A restore never replaces files unless asked to; it is a job like
	// the others.
	restore, err := e.runAs(testUser, &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticRestore{ResticRestore: &syncrv1.ResticRestoreDefinition{
		RepositoryId: repo.GetId(), SnapshotId: "1a2b3c4d", Target: "/srv/restore", Subfolder: "/srv/photos/2025",
	}}})
	if err != nil || restore.GetTitle() != "Restore /srv/photos/2025 from Photos vault @ 1a2b3c4d → /srv/restore" ||
		restore.GetDefinition().GetResticRestore().GetOverwrite() != syncrv1.ResticRestoreOverwrite_RESTIC_RESTORE_OVERWRITE_UNSPECIFIED {
		t.Errorf("RunJob(restore) = %v, %v", restore, err)
	}

	// Windows VSS only on a worker known to run Windows.
	vss := apiBackupDef(repo.GetId())
	vss.GetResticBackup().UseFsSnapshot = true
	if _, err := e.runAs(testUser, vss); err != nil {
		t.Errorf("VSS on a worker that has not reported its system = %v, want it accepted", err)
	}
	e.setWorkerOS("linux")
	_, err = e.runAs(testUser, vss)
	if codeOf(t, err) != connect.CodeInvalidArgument || violation(err, "definition.restic_backup.use_fs_snapshot") != "is only available on Windows workers; this worker runs linux" {
		t.Errorf("VSS on a Linux worker = %v", err)
	}
	e.setWorkerOS("windows")
	if _, err := e.runAs(testUser, vss); err != nil {
		t.Errorf("VSS on a Windows worker = %v", err)
	}
}

// TestBlockersAreLookedUpForQueuedKeyedJobsOnly: listing transfers never
// asks for blockers.
func TestBlockersAreLookedUpForQueuedKeyedJobsOnly(t *testing.T) {
	t.Parallel()
	h := &jobHandler{jobs: panicJobs{}}
	got, err := h.blockers(t.Context(), []jobs.Job{
		{ID: domain.NewJobID(), Status: domain.JobQueued},
		{ID: domain.NewJobID(), Status: domain.JobRunning, ConcurrencyKeys: []string{"k"}},
		{ID: domain.NewJobID(), Status: domain.JobSucceeded, ConcurrencyKeys: []string{"k"}},
	})
	if err != nil || len(got) != 0 {
		t.Errorf("blockers = %v, %v", got, err)
	}
}

// panicJobs is a JobService none of whose methods may be called.
type panicJobs struct{ JobService }

// TestListJobSamplesThinned: max_points thins the samples (first and last
// kept), 0 is the default, a huge value is capped, a negative one is
// refused; total counts every stored sample.
func TestListJobSamplesThinned(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	j := e.runJob()
	c := e.jobsClient()
	e.startRunning(j)
	t0 := e.clock.Now()
	const stored = 7
	for i := range stored { // 5 s apart, so each one is stored
		smp := jobs.Sample{Time: t0.Add(time.Duration(i) * 5 * time.Second), Stats: jobs.Stats{Bytes: int64(i)}}
		if _, err := e.jsvc.RecordProgress(ctx, e.worker, e.jobID(j), testEpoch, smp); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		maxPoints int32
		want      []int64
	}{
		{0, []int64{0, 1, 2, 3, 4, 5, 6}},
		{2, []int64{0, 6}},
		{3, []int64{0, 3, 6}},
		{7, []int64{0, 1, 2, 3, 4, 5, 6}},
		{1 << 30, []int64{0, 1, 2, 3, 4, 5, 6}},
	} {
		resp, err := c.ListJobSamples(ctx, connect.NewRequest(&syncrv1.ListJobSamplesRequest{JobId: j.GetId(), MaxPoints: tt.maxPoints}))
		if err != nil {
			t.Fatal(err)
		}
		var got []int64
		for _, s := range resp.Msg.GetSamples() {
			got = append(got, s.GetStats().GetBytes())
		}
		if !slices.Equal(got, tt.want) || resp.Msg.GetTotal() != stored {
			t.Errorf("max_points %d: samples %v, total %d; want %v, %d", tt.maxPoints, got, resp.Msg.GetTotal(), tt.want, stored)
		}
	}
	_, err := c.ListJobSamples(ctx, connect.NewRequest(&syncrv1.ListJobSamplesRequest{JobId: j.GetId(), MaxPoints: -1}))
	wantCode(t, err, connect.CodeInvalidArgument)
}

// fakeLimits is a TimeLimits with a default limit, as the dispatcher
// computes it.
type fakeLimits struct{ def time.Duration }

func (f fakeLimits) TimeLimit(def *syncrv1.JobDefinition) time.Duration {
	return jobs.EffectiveTimeLimit(def.GetMaxRuntime().AsDuration(), f.def, false)
}

// TestJobTimeLimit: Job.time_limit is the effective limit (the
// definition's, else the default). The title never names a limit: the job
// page shows time_limit by itself.
func TestJobTimeLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(d *Deps) { d.TimeLimits = fakeLimits{def: 2 * time.Hour} })
	ctx := t.Context()
	run := func(def *syncrv1.JobDefinition) *syncrv1.Job {
		t.Helper()
		resp, err := e.jobsClient().RunJob(ctx, connect.NewRequest(&syncrv1.RunJobRequest{WorkerId: e.worker.String(), Definition: def}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetJob()
	}
	own := testDefinition()
	own.MaxRuntime = durationpb.New(6 * time.Hour)
	j := run(own)
	if j.GetTimeLimit().AsDuration() != 6*time.Hour || j.GetTitle() == "" || strings.Contains(j.GetTitle(), "Time limit") {
		t.Errorf("own limit: time_limit %v, title %q", j.GetTimeLimit(), j.GetTitle())
	}
	got, err := e.jobsClient().GetJob(ctx, connect.NewRequest(&syncrv1.GetJobRequest{JobId: j.GetId()}))
	if err != nil || got.Msg.GetJob().GetTimeLimit().AsDuration() != 6*time.Hour {
		t.Errorf("GetJob time_limit = %v (%v)", got, err)
	}
	if j := run(testDefinition()); j.GetTimeLimit().AsDuration() != 2*time.Hour || strings.Contains(j.GetTitle(), "Time limit") {
		t.Errorf("default limit: time_limit %v, title %q", j.GetTimeLimit(), j.GetTitle())
	}
	bad := testDefinition()
	bad.MaxRuntime = durationpb.New(30 * time.Second)
	_, err = e.jobsClient().RunJob(ctx, connect.NewRequest(&syncrv1.RunJobRequest{WorkerId: e.worker.String(), Definition: bad}))
	if codeOf(t, err) != connect.CodeInvalidArgument || violation(err, "definition.max_runtime") == "" {
		t.Errorf("a 30s limit: %v", err)
	}

	// Without the dependency the field is unset.
	plain := newEnv(t)
	resp, err := plain.jobsClient().RunJob(ctx, connect.NewRequest(&syncrv1.RunJobRequest{WorkerId: plain.worker.String(), Definition: own}))
	if err != nil || resp.Msg.GetJob().GetTimeLimit() != nil {
		t.Errorf("without TimeLimits: %v (%v)", resp, err)
	}
}
