package api

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// Why a WatchJob stream was ended by the server.
var (
	errWatchLimit   = errors.New("watch duration limit reached")
	errShuttingDown = errors.New("controller is shutting down")
)

// jobHandler implements syncr.v1.JobService.
type jobHandler struct {
	jobs JobService
	errs errorMapper
	log  *slog.Logger
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

// job converts j, looking up the live counters when the job's status
// calls for them (see pickStats).
func (h *jobHandler) job(ctx context.Context, j jobs.Job) (*syncrv1.Job, error) {
	stats, needSample := pickStats(j)
	if needSample {
		smp, ok, err := h.jobs.LatestSample(ctx, j.ID)
		if err != nil {
			return nil, err
		}
		if ok {
			stats = &smp.Stats
		}
	}
	msg, err := jobToProto(j, stats)
	if err != nil {
		// Only the job's output is affected; the rest is still worth showing.
		h.log.WarnContext(ctx, "job output is not decodable", "job_id", j.ID.String(), "error", err.Error())
	}
	return msg, nil
}

// RunJob validates the request shape and queues the job. The spec is
// encoded as is and stored encrypted by the jobs service; the worker
// validates it in depth and reports problems as a failed job.
func (h *jobHandler) RunJob(ctx context.Context, req *connect.Request[syncrv1.RunJobRequest]) (*connect.Response[syncrv1.RunJobResponse], error) {
	workerID, err := parseWorkerID(req.Msg.GetWorkerId())
	if err != nil {
		return nil, err
	}
	spec := req.Msg.GetSpec()
	if spec == nil {
		return nil, invalid("spec is required")
	}
	kind := link.KindOf(spec)
	if kind == "" {
		return nil, invalid("spec.operation must be set")
	}
	encoded, err := marshalSpec(spec)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.JobServiceRunJobProcedure, err)
	}
	j, err := h.jobs.Create(ctx, jobs.CreateParams{WorkerID: workerID, Kind: kind, Spec: encoded})
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("worker not found"))
		}
		return nil, h.fail(ctx, syncrv1connect.JobServiceRunJobProcedure, err)
	}
	msg, err := h.job(ctx, j)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.JobServiceRunJobProcedure, err)
	}
	return connect.NewResponse(&syncrv1.RunJobResponse{Job: msg}), nil
}

// GetJob returns one job.
func (h *jobHandler) GetJob(ctx context.Context, req *connect.Request[syncrv1.GetJobRequest]) (*connect.Response[syncrv1.GetJobResponse], error) {
	id, err := parseJobID(req.Msg.GetJobId())
	if err != nil {
		return nil, err
	}
	j, err := h.jobs.Get(ctx, id)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.JobServiceGetJobProcedure, err)
	}
	msg, err := h.job(ctx, j)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.JobServiceGetJobProcedure, err)
	}
	return connect.NewResponse(&syncrv1.GetJobResponse{Job: msg}), nil
}

// ListJobs returns a page of jobs, newest first.
func (h *jobHandler) ListJobs(ctx context.Context, req *connect.Request[syncrv1.ListJobsRequest]) (*connect.Response[syncrv1.ListJobsResponse], error) {
	m := req.Msg
	f := jobs.ListFilter{}
	if m.GetWorkerId() != "" {
		id, err := parseWorkerID(m.GetWorkerId())
		if err != nil {
			return nil, err
		}
		f.WorkerID = id
	}
	for _, s := range m.GetStatuses() {
		st, err := jobStatusFromProto(s)
		if err != nil {
			return nil, invalid("statuses: unknown or unspecified job status")
		}
		f.Statuses = append(f.Statuses, st)
	}
	limit, err := clampPageSize(m.GetPageSize(), jobs.DefaultPageSize, jobs.MaxPageSize)
	if err != nil {
		return nil, invalid(err.Error())
	}
	f.Limit = limit
	if f.Before, err = decodePageToken(m.GetPageToken()); err != nil {
		return nil, invalid(err.Error())
	}

	page, err := h.jobs.List(ctx, f)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.JobServiceListJobsProcedure, err)
	}
	resp := &syncrv1.ListJobsResponse{
		Jobs:          make([]*syncrv1.Job, 0, len(page.Jobs)),
		NextPageToken: encodePageToken(page.Next),
	}
	for _, j := range page.Jobs {
		msg, err := h.job(ctx, j)
		if err != nil {
			return nil, h.fail(ctx, syncrv1connect.JobServiceListJobsProcedure, err)
		}
		resp.Jobs = append(resp.Jobs, msg)
	}
	return connect.NewResponse(resp), nil
}

// CancelJob cancels a job and returns it after the request.
func (h *jobHandler) CancelJob(ctx context.Context, req *connect.Request[syncrv1.CancelJobRequest]) (*connect.Response[syncrv1.CancelJobResponse], error) {
	id, err := parseJobID(req.Msg.GetJobId())
	if err != nil {
		return nil, err
	}
	j, err := h.jobs.Cancel(ctx, id)
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
//   - the controller shuts down (Unavailable).
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

	err = h.jobs.Watch(wctx, id, func(u jobs.Update) error {
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
