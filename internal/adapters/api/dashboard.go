package api

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Sizes of the dashboard's lists.
const (
	dashboardUpcoming       = 8
	dashboardRecentFailures = 5
	dashboardRecentMissed   = 5
	// maxTimezoneLen bounds GetDashboardRequest.timezone.
	maxTimezoneLen = 64
)

// dashboardHandler implements syncr.v1.DashboardService, composed from the
// jobs, workers and schedules use cases (no use case of its own).
type dashboardHandler struct {
	syncrv1connect.UnimplementedDashboardServiceHandler

	jobs      JobService
	activity  DashboardJobs
	workers   WorkerService
	schedules ScheduleService
	// remotes names the remotes of the recent failures; nil leaves them
	// without names.
	remotes RemoteNames
	errs    errorMapper
	// now is the clock of the range; nil means time.Now.
	now func() time.Time
}

var _ syncrv1connect.DashboardServiceHandler = (*dashboardHandler)(nil)

func (h *dashboardHandler) fail(ctx context.Context, err error) error {
	return h.errs.toConnect(ctx, syncrv1connect.DashboardServiceGetDashboardProcedure, "dashboard", err)
}

func (h *dashboardHandler) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// dashboardRange is the time range and bucketing of a request.
type dashboardRange struct {
	from, until time.Time
	unit        jobs.BucketUnit
}

// rangeFor returns the range of r ending at now in loc: DAY is the current
// hour and the 23 before it, WEEK and MONTH the current local day and the
// 6 or 29 before it. The first bucket starts at from and the last one
// (the current hour or day) is partial.
func rangeFor(r syncrv1.DashboardRange, now time.Time, loc *time.Location) dashboardRange {
	lt := now.In(loc)
	switch r {
	case syncrv1.DashboardRange_DASHBOARD_RANGE_WEEK, syncrv1.DashboardRange_DASHBOARD_RANGE_MONTH:
		days := 7
		if r == syncrv1.DashboardRange_DASHBOARD_RANGE_MONTH {
			days = 30
		}
		y, m, d := lt.Date()
		return dashboardRange{from: time.Date(y, m, d-(days-1), 0, 0, 0, 0, loc).UTC(), until: now.UTC(), unit: jobs.BucketDay}
	default:
		hour := lt.Add(-(time.Duration(lt.Minute())*time.Minute + time.Duration(lt.Second())*time.Second + time.Duration(lt.Nanosecond())))
		return dashboardRange{from: hour.Add(-23 * time.Hour).UTC(), until: now.UTC(), unit: jobs.BucketHour}
	}
}

// parseTimezone loads the request's IANA zone; empty means UTC.
func parseTimezone(name string) (*time.Location, error) {
	if name == "" || name == "UTC" {
		return time.UTC, nil
	}
	if len(name) > maxTimezoneLen || name == "Local" || strings.ContainsAny(name, `\`) || strings.HasPrefix(name, "/") {
		return nil, invalidField("timezone", "is not a known IANA time zone")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, invalidField("timezone", "is not a known IANA time zone")
	}
	return loc, nil
}

// GetDashboard returns the caller's dashboard. Job statistics, unfinished
// jobs and recent failures cover every user's jobs for an admin with
// all_users; schedules are always the caller's own; workers are shared.
func (h *dashboardHandler) GetDashboard(ctx context.Context, req *connect.Request[syncrv1.GetDashboardRequest]) (*connect.Response[syncrv1.GetDashboardResponse], error) {
	actor, _ := ActorFrom(ctx)
	loc, err := parseTimezone(req.Msg.GetTimezone())
	if err != nil {
		return nil, err
	}
	allUsers := req.Msg.GetAllUsers() && actor.IsAdmin()
	rng := rangeFor(req.Msg.GetRange(), h.clock(), loc)

	buckets, err := h.activity.Activity(ctx, actor, jobs.ActivityFilter{
		From: rng.from, Until: rng.until, Unit: rng.unit, Location: loc, AllUsers: allUsers,
	})
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	unfinished, err := h.activity.CountUnfinished(ctx, actor, allUsers)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	missed, err := h.schedules.CountMissed(ctx, actor, rng.from, rng.until)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	resp := &syncrv1.GetDashboardResponse{
		From:       ts(rng.from),
		Until:      ts(rng.until),
		BucketUnit: string(rng.unit),
		Totals:     totalsToProto(buckets, unfinished, missed),
		Activity:   make([]*syncrv1.ActivityBucket, 0, len(buckets)),
	}
	for _, b := range buckets {
		resp.Activity = append(resp.Activity, &syncrv1.ActivityBucket{
			Start: ts(b.Start), Succeeded: b.Succeeded, Failed: b.Failed, Interrupted: b.Interrupted,
			Cancelled: b.Cancelled, Bytes: b.Bytes, Files: b.Files,
		})
	}

	ws, err := h.workers.List(ctx, workers.ListFilter{})
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	resp.Workers = h.workerHealth(ws)
	if resp.Upcoming, err = h.upcoming(ctx, actor, ws); err != nil {
		return nil, h.fail(ctx, err)
	}
	if resp.RecentFailures, err = h.recentFailures(ctx, actor, allUsers); err != nil {
		return nil, h.fail(ctx, err)
	}
	runs, err := h.schedules.RecentProblems(ctx, actor, dashboardRecentMissed)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	resp.RecentMissed = make([]*syncrv1.ScheduleRun, 0, len(runs))
	for _, r := range runs {
		resp.RecentMissed = append(resp.RecentMissed, scheduleRunToProto(r))
	}
	return connect.NewResponse(resp), nil
}

// totalsToProto sums the buckets and adds the jobs active now and the
// missed runs.
func totalsToProto(buckets []jobs.ActivityBucket, unfinished map[domain.JobStatus]int64, missed int64) *syncrv1.JobTotals {
	t := &syncrv1.JobTotals{
		Running:    unfinished[domain.JobRunning],
		Queued:     unfinished[domain.JobQueued] + unfinished[domain.JobDispatched],
		MissedRuns: missed,
	}
	for _, b := range buckets {
		t.Succeeded += b.Succeeded
		t.Failed += b.Failed
		t.Interrupted += b.Interrupted
		t.Cancelled += b.Cancelled
		t.Bytes += b.Bytes
		t.Files += b.Files
	}
	if done := t.Succeeded + t.Failed + t.Interrupted; done > 0 {
		t.SuccessRate = float64(t.Succeeded) / float64(done)
	}
	return t
}

// workerHealth counts the workers by status; ws is sorted by name.
func (h *dashboardHandler) workerHealth(ws []workers.Worker) *syncrv1.WorkerHealth {
	out := &syncrv1.WorkerHealth{Workers: make([]*syncrv1.Worker, 0, len(ws))}
	for _, w := range ws {
		st := h.workers.Status(w)
		switch st {
		case domain.WorkerOnline:
			out.Online++
		case domain.WorkerOffline:
			out.Offline++
		default:
			out.Pending++
		}
		out.Workers = append(out.Workers, workerToProto(w, st))
	}
	return out
}

// upcoming returns the caller's next runs with their target worker: the
// worker itself, or for a label the first online worker with it (by name),
// else the first with it.
func (h *dashboardHandler) upcoming(ctx context.Context, actor domain.Actor, ws []workers.Worker) ([]*syncrv1.UpcomingRun, error) {
	ups, err := h.schedules.Upcoming(ctx, actor, dashboardUpcoming)
	if err != nil {
		return nil, err
	}
	out := make([]*syncrv1.UpcomingRun, 0, len(ups))
	for _, u := range ups {
		msg := &syncrv1.UpcomingRun{
			ScheduleId:   u.ScheduleID.String(),
			ScheduleName: u.Name,
			At:           ts(u.At),
			Target:       scheduleTargetToProto(u.Target),
		}
		if w, ok := h.targetWorker(u.Target, ws); ok {
			msg.WorkerName = w.Name
			msg.WorkerStatus = workerStatusToProto(h.workers.Status(w))
		}
		out = append(out, msg)
	}
	return out, nil
}

func (h *dashboardHandler) targetWorker(t schedules.Target, ws []workers.Worker) (workers.Worker, bool) {
	var found *workers.Worker
	for i := range ws {
		w := &ws[i]
		switch {
		case !t.WorkerID.IsZero():
			if w.ID == t.WorkerID {
				return *w, true
			}
		case t.Label != "" && slices.Contains(w.Labels, t.Label):
			if h.workers.Status(*w) == domain.WorkerOnline {
				return *w, true
			}
			if found == nil {
				found = w
			}
		}
	}
	if found == nil {
		return workers.Worker{}, false
	}
	return *found, true
}

// recentFailures returns the latest failed or interrupted jobs, with their
// remote and schedule references.
func (h *dashboardHandler) recentFailures(ctx context.Context, actor domain.Actor, allUsers bool) ([]*syncrv1.Job, error) {
	page, err := h.jobs.List(ctx, actor, jobs.ListFilter{
		Statuses: []domain.JobStatus{domain.JobFailed, domain.JobInterrupted},
		AllUsers: allUsers,
		Limit:    dashboardRecentFailures,
	})
	if err != nil {
		return nil, err
	}
	conv := &jobHandler{jobs: h.jobs, remotes: h.remotes, schedules: h.schedules, errs: h.errs, log: cmp.Or(h.errs.log, slog.New(slog.DiscardHandler))}
	return conv.protoJobs(ctx, page.Jobs)
}
