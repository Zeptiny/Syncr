package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
)

// Why a WatchJob stream was ended by the server.
var (
	errWatchLimit   = errors.New("watch duration limit reached")
	errShuttingDown = errors.New("controller is shutting down")
)

// jobHandler implements syncr.v1.JobService. Every call acts as the
// caller's actor; the jobs service scopes the jobs by it.
type jobHandler struct {
	syncrv1connect.UnimplementedJobServiceHandler

	jobs JobService
	// kinds checks the definitions of RunJob; nil makes RunJob
	// Unimplemented.
	kinds *jobkind.Registry
	// remotes names the jobs' remotes (Job.remotes); nil leaves the
	// references without names.
	remotes RemoteNames
	// schedules names the jobs' schedules (Job.schedule); nil leaves it
	// unset (schedule_id is still set).
	schedules ScheduleNames
	// repos names the jobs' restic repositories (Job.repository and the
	// title); nil leaves the reference without a name.
	repos RepositoryNames
	// workers tells RunJob the worker's operating system (the VSS check);
	// nil skips the check.
	workers WorkerNames
	errs    errorMapper
	log     *slog.Logger
	// base ends all watch streams when done (server shutdown).
	base context.Context
	// watchMax bounds a WatchJob stream; keepalive is the idle time after
	// which the stream resends the job.
	watchMax  time.Duration
	keepalive time.Duration
}

var _ syncrv1connect.JobServiceHandler = (*jobHandler)(nil)

func (h *jobHandler) fail(ctx context.Context, procedure string, err error) error {
	return h.errs.toConnect(ctx, procedure, "job", err)
}

// job converts j (see protoJobs).
func (h *jobHandler) job(ctx context.Context, j jobs.Job) (*syncrv1.Job, error) {
	out, err := h.protoJobs(ctx, []jobs.Job{j})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

// protoJobs converts js with their remote, repository and schedule
// references (one Names lookup each for all), the jobs the queued ones wait
// for (one Blockers lookup) and, where the job's status calls for them (see
// pickStats), the live counters (one LatestSamples lookup for all). The
// job kinds add the title and the display details.
func (h *jobHandler) protoJobs(ctx context.Context, js []jobs.Job) ([]*syncrv1.Job, error) {
	refs, err := h.remoteRefs(ctx, js)
	if err != nil {
		return nil, err
	}
	scheds, err := h.scheduleRefs(ctx, js)
	if err != nil {
		return nil, err
	}
	repos, err := h.repositoryRefs(ctx, js)
	if err != nil {
		return nil, err
	}
	waiting, err := h.blockers(ctx, js)
	if err != nil {
		return nil, err
	}
	names := jobkind.Names{Remotes: make(map[domain.RemoteID]string, len(refs)), Repositories: make(map[domain.ResticRepoID]string, len(repos))}
	for id, r := range refs {
		names.Remotes[id] = r.Name
	}
	for id, r := range repos {
		names.Repositories[id] = r.Name
	}
	var sampled []domain.JobID
	for _, j := range js {
		if _, needSample := pickStats(j); needSample {
			sampled = append(sampled, j.ID)
		}
	}
	var latest map[domain.JobID]jobs.Sample
	if len(sampled) > 0 {
		if latest, err = h.jobs.LatestSamples(ctx, sampled); err != nil {
			return nil, err
		}
	}
	out := make([]*syncrv1.Job, 0, len(js))
	for _, j := range js {
		stats, needSample := pickStats(j)
		if smp, ok := latest[j.ID]; needSample && ok {
			stats = &smp.Stats
		}
		msg, err := jobToProto(j, stats, refs)
		if err != nil {
			// Only the job's definition or output is affected; the rest is
			// still worth showing.
			h.log.WarnContext(ctx, "job definition or output is not decodable", "job_id", j.ID.String(), "error", err.Error())
		}
		if ref, ok := scheds[j.ScheduleID]; ok && !j.ScheduleID.IsZero() {
			msg.Schedule = &syncrv1.JobScheduleRef{Id: ref.ID.String(), Name: ref.Name}
		}
		if !j.RepositoryID.IsZero() {
			msg.Repository = repositoryRefToProto(j.RepositoryID, repos[j.RepositoryID])
		}
		if blocker, ok := waiting[j.ID]; ok {
			msg.WaitingForJobId = blocker.String()
		}
		if h.kinds != nil && msg.GetDefinition() != nil {
			msg.Title = h.kinds.Describe(msg.GetDefinition(), names)
			msg.Details = jobDetailsToProto(h.kinds.Summarize(j.Kind, msg.GetDefinition(), j.Output))
		}
		out = append(out, msg)
	}
	return out, nil
}

// remoteRefs looks up the remotes of js.
func (h *jobHandler) remoteRefs(ctx context.Context, js []jobs.Job) (map[domain.RemoteID]remotes.Ref, error) {
	if h.remotes == nil {
		return nil, nil
	}
	var ids []domain.RemoteID
	for _, j := range js {
		for _, id := range j.RemoteIDs {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	refs, err := h.remotes.Names(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("name the remotes of jobs: %w", err)
	}
	return refs, nil
}

// repositoryRefs looks up the restic repositories of js (deleted ones
// included).
func (h *jobHandler) repositoryRefs(ctx context.Context, js []jobs.Job) (map[domain.ResticRepoID]restic.Ref, error) {
	if h.repos == nil {
		return nil, nil
	}
	var ids []domain.ResticRepoID
	for _, j := range js {
		if !j.RepositoryID.IsZero() && !slices.Contains(ids, j.RepositoryID) {
			ids = append(ids, j.RepositoryID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	refs, err := h.repos.Names(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("name the repositories of jobs: %w", err)
	}
	return refs, nil
}

// blockers returns, for the queued jobs of js that hold concurrency keys,
// the older unfinished job each waits for (Job.waiting_for_job_id).
func (h *jobHandler) blockers(ctx context.Context, js []jobs.Job) (map[domain.JobID]domain.JobID, error) {
	var ids []domain.JobID
	for _, j := range js {
		if j.Status == domain.JobQueued && len(j.ConcurrencyKeys) > 0 {
			ids = append(ids, j.ID)
		}
	}
	out := map[domain.JobID]domain.JobID{}
	for chunk := range slices.Chunk(ids, jobs.MaxBlockerIDs) {
		got, err := h.jobs.Blockers(ctx, chunk)
		if err != nil {
			return nil, fmt.Errorf("find the jobs that queued jobs wait for: %w", err)
		}
		maps.Copy(out, got)
	}
	return out, nil
}

// scheduleRefs looks up the schedules of js; a deleted schedule has no
// reference (its jobs' schedule_id is cleared anyway).
func (h *jobHandler) scheduleRefs(ctx context.Context, js []jobs.Job) (map[domain.ScheduleID]schedules.Ref, error) {
	if h.schedules == nil {
		return nil, nil
	}
	var ids []domain.ScheduleID
	for _, j := range js {
		if !j.ScheduleID.IsZero() && !slices.Contains(ids, j.ScheduleID) {
			ids = append(ids, j.ScheduleID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	refs, err := h.schedules.Names(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("name the schedules of jobs: %w", err)
	}
	return refs, nil
}

// checkWorkerCanRun rejects a definition that the chosen worker is known
// not to be able to run: a backup with use_fs_snapshot (Windows VSS) on a
// worker that reported another operating system. A worker that has not
// reported one yet, or cannot be read, passes: the worker itself fails
// such a job.
func checkWorkerCanRun(ctx context.Context, ws WorkerNames, id domain.WorkerID, def *syncrv1.JobDefinition) error {
	if ws == nil || id.IsZero() || !def.GetResticBackup().GetUseFsSnapshot() {
		return nil
	}
	w, err := ws.Get(ctx, id)
	if err != nil || w.Info.OS == "" || w.Info.OS == "windows" {
		return nil
	}
	return invalidField("definition.restic_backup.use_fs_snapshot", "is only available on Windows workers; this worker runs "+w.Info.OS)
}

// RunJob checks the definition with its job kind and queues the job for
// the caller. The definition is secret-free and its remotes must be the
// caller's live remotes; the dispatcher builds the worker's spec, with the
// remotes' credentials, when it sends the job, and the worker validates it
// in depth and reports problems as a failed job.
func (h *jobHandler) RunJob(ctx context.Context, req *connect.Request[syncrv1.RunJobRequest]) (*connect.Response[syncrv1.RunJobResponse], error) {
	const procedure = syncrv1connect.JobServiceRunJobProcedure
	workerID, err := parseWorkerID(req.Msg.GetWorkerId())
	if err != nil {
		return nil, err
	}
	if h.kinds == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this controller has no job kinds"))
	}
	def := req.Msg.GetDefinition()
	if def == nil {
		return nil, invalidField("definition", "is required")
	}
	kind, refs, err := h.kinds.Check(def)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	// Kinds only the controller starts (restic_delete_data) are never
	// accepted from a caller.
	if ok, why := h.kinds.UserStartable(kind); !ok {
		return nil, invalidField("definition."+kind, why)
	}
	if err := checkWorkerCanRun(ctx, h.workers, workerID, def); err != nil {
		return nil, err
	}
	encoded, err := jobkind.Marshal(def)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	actor, _ := ActorFrom(ctx)
	j, err := h.jobs.Create(ctx, actor, jobs.CreateParams{
		WorkerID: workerID, Kind: kind, Definition: encoded, RemoteIDs: refs.RemoteIDs, SearchTerms: refs.SearchTerms,
		RepositoryID: refs.RepositoryID, ConcurrencyKeys: refs.ConcurrencyKeys,
	})
	switch {
	case errors.Is(err, jobs.ErrUnknownRemote):
		return nil, connect.NewError(connect.CodeNotFound, errors.New("remote not found"))
	case errors.Is(err, jobs.ErrUnknownRepository):
		return nil, connect.NewError(connect.CodeNotFound, errors.New("repository not found"))
	case errors.Is(err, jobs.ErrRepositoryDeleting):
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the repository's data is being deleted"))
	case errors.Is(err, jobs.ErrUnknownWorker):
		return nil, connect.NewError(connect.CodeNotFound, errors.New("worker not found"))
	case err != nil:
		return nil, h.fail(ctx, procedure, err)
	}
	msg, err := h.job(ctx, j)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	return connect.NewResponse(&syncrv1.RunJobResponse{Job: msg}), nil
}

// GetJob returns one job.
func (h *jobHandler) GetJob(ctx context.Context, req *connect.Request[syncrv1.GetJobRequest]) (*connect.Response[syncrv1.GetJobResponse], error) {
	id, err := parseJobID(req.Msg.GetJobId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	j, err := h.jobs.Get(ctx, actor, id)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.JobServiceGetJobProcedure, err)
	}
	msg, err := h.job(ctx, j)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.JobServiceGetJobProcedure, err)
	}
	return connect.NewResponse(&syncrv1.GetJobResponse{Job: msg}), nil
}

// ListJobs returns a page of the caller's jobs (every user's for an admin
// with all_users), newest first, filtered by the request.
func (h *jobHandler) ListJobs(ctx context.Context, req *connect.Request[syncrv1.ListJobsRequest]) (*connect.Response[syncrv1.ListJobsResponse], error) {
	const procedure = syncrv1connect.JobServiceListJobsProcedure
	m := req.Msg
	f := jobs.ListFilter{Kinds: m.GetKinds(), Query: m.GetQuery(), AllUsers: m.GetAllUsers()}
	if m.GetWorkerId() != "" {
		id, err := parseWorkerID(m.GetWorkerId())
		if err != nil {
			return nil, err
		}
		f.WorkerID = id
	}
	if m.GetRemoteId() != "" {
		id, err := domain.ParseRemoteID(m.GetRemoteId())
		if err != nil || id.IsZero() {
			return nil, invalidField("remote_id", "is not a valid ID")
		}
		f.RemoteID = id
	}
	if m.GetScheduleId() != "" {
		id, err := domain.ParseScheduleID(m.GetScheduleId())
		if err != nil || id.IsZero() {
			return nil, invalidField("schedule_id", "is not a valid ID")
		}
		f.ScheduleID = id
	}
	if m.GetRepositoryId() != "" {
		id, err := parseRepositoryID("repository_id", m.GetRepositoryId())
		if err != nil {
			return nil, err
		}
		f.RepositoryID = id
	}
	for _, s := range m.GetStatuses() {
		st, err := jobStatusFromProto(s)
		if err != nil {
			return nil, invalid("statuses: unknown or unspecified job status")
		}
		f.Statuses = append(f.Statuses, st)
	}
	var err error
	if f.CreatedAfter, err = optTime("created_after", m.GetCreatedAfter()); err != nil {
		return nil, err
	}
	if f.CreatedBefore, err = optTime("created_before", m.GetCreatedBefore()); err != nil {
		return nil, err
	}
	limit, err := clampPageSize(m.GetPageSize(), jobs.DefaultPageSize, jobs.MaxPageSize)
	if err != nil {
		return nil, invalid(err.Error())
	}
	f.Limit = limit
	if f.Before, err = decodePageToken(m.GetPageToken()); err != nil {
		return nil, invalid(err.Error())
	}

	actor, _ := ActorFrom(ctx)
	page, err := h.jobs.List(ctx, actor, f)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	msgs, err := h.protoJobs(ctx, page.Jobs)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	return connect.NewResponse(&syncrv1.ListJobsResponse{Jobs: msgs, NextPageToken: encodePageToken(page.Next)}), nil
}

// ListJobSamples returns the stored progress samples of a job, oldest
// first.
func (h *jobHandler) ListJobSamples(ctx context.Context, req *connect.Request[syncrv1.ListJobSamplesRequest]) (*connect.Response[syncrv1.ListJobSamplesResponse], error) {
	id, err := parseJobID(req.Msg.GetJobId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	smps, err := h.jobs.Samples(ctx, actor, id)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.JobServiceListJobSamplesProcedure, err)
	}
	resp := &syncrv1.ListJobSamplesResponse{Samples: make([]*syncrv1.JobProgress, 0, len(smps))}
	for _, smp := range smps {
		resp.Samples = append(resp.Samples, progressToProto(smp))
	}
	return connect.NewResponse(resp), nil
}

// CancelJob cancels a job and returns it after the request.
func (h *jobHandler) CancelJob(ctx context.Context, req *connect.Request[syncrv1.CancelJobRequest]) (*connect.Response[syncrv1.CancelJobResponse], error) {
	id, err := parseJobID(req.Msg.GetJobId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	j, err := h.jobs.Cancel(ctx, actor, id)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.JobServiceCancelJobProcedure, err)
	}
	msg, err := h.job(ctx, j)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.JobServiceCancelJobProcedure, err)
	}
	return connect.NewResponse(&syncrv1.CancelJobResponse{Job: msg}), nil
}

// WatchJob streams the job's current state, then its updates until it
// reaches a terminal status.
//
// The stream also ends, with an error, when:
//   - the client goes away (nothing is sent, the stream is just released);
//   - the watch duration limit is reached (DeadlineExceeded; the client
//     watches again, and the first message is the then current job);
//   - the controller shuts down (Unavailable);
//   - the caller's session or token stops working, the user is disabled
//     (Unauthenticated) or no longer sees the job (PermissionDenied): the
//     guard re-authenticates the stream periodically (streamauth.go).
//
// A slow client does not hold the job pipeline back: the broker coalesces
// progress samples per subscriber, so the handler only ever blocks its own
// goroutine in Send. While nothing else is sent, the last job message is
// repeated every keepalive so idle proxies do not drop the stream.
func (h *jobHandler) WatchJob(ctx context.Context, req *connect.Request[syncrv1.WatchJobRequest], stream *connect.ServerStream[syncrv1.WatchJobResponse]) error {
	const procedure = syncrv1connect.JobServiceWatchJobProcedure
	id, err := parseJobID(req.Msg.GetJobId())
	if err != nil {
		return err
	}
	// While the stream is open, the guard re-authenticates the caller; the
	// refreshed actor must still see the job (an admin may have lost the
	// role).
	onReauth(ctx, func(ctx context.Context, c Caller) error {
		_, err := h.jobs.Get(ctx, c.Actor, id)
		if errors.Is(err, domain.ErrNotFound) {
			return connect.NewError(connect.CodePermissionDenied, errors.New("access to the job was revoked"))
		}
		if err != nil {
			return h.fail(ctx, procedure, err)
		}
		return nil
	})

	wctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	limit := time.AfterFunc(h.watchMax, func() { cancel(errWatchLimit) })
	defer limit.Stop()
	stopBase := context.AfterFunc(h.base, func() { cancel(errShuttingDown) })
	defer stopBase()

	var (
		mu   sync.Mutex // serializes Send between the update loop and keepalive
		sent bool       // something was sent since the last keepalive tick
		last *syncrv1.WatchJobResponse
	)
	send := func(msg *syncrv1.WatchJobResponse, remember bool) error {
		mu.Lock()
		defer mu.Unlock()
		if err := stream.Send(msg); err != nil {
			return err
		}
		sent = true
		if remember {
			last = msg
		}
		return nil
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(h.keepalive)
		defer tick.Stop()
		for {
			select {
			case <-wctx.Done():
				return
			case <-tick.C:
			}
			mu.Lock()
			var err error
			if !sent && last != nil {
				err = stream.Send(last)
			}
			sent = false
			mu.Unlock()
			if err != nil {
				cancel(err)
				return
			}
		}
	}()

	actor, _ := ActorFrom(ctx)
	err = h.jobs.Watch(wctx, actor, id, func(u jobs.Update) error {
		switch {
		case u.Job != nil:
			msg, err := h.job(wctx, *u.Job)
			if err != nil {
				return err
			}
			return send(&syncrv1.WatchJobResponse{Update: &syncrv1.WatchJobResponse_Job{Job: msg}}, true)
		case u.Progress != nil:
			return send(&syncrv1.WatchJobResponse{Update: &syncrv1.WatchJobResponse_Progress{Progress: progressToProto(*u.Progress)}}, false)
		default:
			return nil
		}
	})
	cancel(nil)
	wg.Wait()

	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return connect.NewError(connect.CodeCanceled, errors.New("client went away"))
	case errors.Is(context.Cause(wctx), errWatchLimit):
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("watch duration limit reached; call WatchJob again"))
	case errors.Is(context.Cause(wctx), errShuttingDown):
		return connect.NewError(connect.CodeUnavailable, errors.New("the controller is shutting down; call WatchJob again"))
	default:
		return h.fail(ctx, procedure, err)
	}
}

// optTime converts an optional request timestamp (unset: the zero time)
// into an InvalidArgument error naming field if it is out of range.
func optTime(field string, ts *timestamppb.Timestamp) (time.Time, error) {
	if ts == nil {
		return time.Time{}, nil
	}
	if ts.CheckValid() != nil {
		return time.Time{}, invalidField(field, "is not a valid timestamp")
	}
	return ts.AsTime(), nil
}

// parseJobID parses a request's job ID into an InvalidArgument error naming
// the field.
func parseJobID(s string) (domain.JobID, error) {
	if s == "" {
		return domain.JobID{}, invalid("job_id is required")
	}
	id, err := domain.ParseJobID(s)
	if err != nil || id.IsZero() {
		return domain.JobID{}, invalid("job_id is not a valid ID")
	}
	return id, nil
}
