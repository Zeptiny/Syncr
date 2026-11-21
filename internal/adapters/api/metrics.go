package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/version"
)

// MetricsPath is where the controller serves its Prometheus metrics.
const MetricsPath = "/metrics"

// metricsRunTimeBounds are the upper bounds of syncr_job_run_duration_seconds.
func metricsRunTimeBounds() []time.Duration {
	return []time.Duration{
		time.Second, 10 * time.Second, 30 * time.Second, time.Minute, 5 * time.Minute, 15 * time.Minute,
		30 * time.Minute, time.Hour, 2 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour,
	}
}

// Last-run states of syncr_schedule_last_run_status (an OpenMetrics state
// set: one series per state, the current one 1). A run that is pending or
// launching, or whose job has not finished, is active; one that launched
// takes its job's terminal status; missed is a run that could not start in
// time, not_started one that could not start at all, cancelled also a run
// cancelled because its schedule was paused.
const (
	runStateActive      = "active"
	runStateSucceeded   = "succeeded"
	runStateFailed      = "failed"
	runStateInterrupted = "interrupted"
	runStateCancelled   = "cancelled"
	runStateMissed      = "missed"
	runStateNotStarted  = "not_started"
)

func runStates() []string {
	return []string{runStateActive, runStateSucceeded, runStateFailed, runStateInterrupted, runStateCancelled, runStateMissed, runStateNotStarted}
}

// metricsHandler serves GET /metrics in the Prometheus text format (or
// OpenMetrics, when the scraper asks for it). A scrape must authenticate
// with a personal API token ("Authorization: Bearer syncr_pat_...") and
// sees what the token's user sees on the dashboard: every worker, the
// user's own jobs (every user's for an admin), and the user's own
// schedules and restic repositories (owner-only, admins included). Only an
// admin's scrape adds the Go runtime and process metrics.
//
// Every value is read from the store on each scrape, so the job counters
// survive controller restarts: they count every finished job ever stored.
type metricsHandler struct {
	auth      Authenticator
	jobs      DashboardJobs
	workers   WorkerService
	schedules ScheduleService
	// restic adds the repository metrics; nil leaves them out.
	restic  ResticService
	build   version.Info
	version string // the controller's version, for syncr_worker_outdated
	timeout time.Duration
	log     *slog.Logger
	descs   metricDescs
}

func newMetricsHandler(deps Deps, auth Authenticator, activity DashboardJobs, logger *slog.Logger) *metricsHandler {
	return &metricsHandler{
		auth: auth, jobs: activity, workers: deps.Workers, schedules: deps.Schedules, restic: deps.Restic,
		build: deps.BuildInfo, version: deps.Install.Version,
		timeout: cmp.Or(deps.RequestTimeout, defaultRequestTimeout), log: logger, descs: newMetricDescs(),
	}
}

func (h *metricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, err := h.auth.Authenticate(r.Context(), r.Header, connect.Spec{Procedure: MetricsPath, StreamType: connect.StreamTypeUnary})
	caller, ok := CallerFrom(ctx)
	switch {
	case err != nil && !errors.Is(err, domain.ErrUnauthenticated):
		h.log.ErrorContext(r.Context(), "cannot authenticate a metrics scrape", "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	case err != nil || !ok || caller.Method != AuthToken:
		reason := "the scrape did not send a personal API token"
		if err != nil {
			reason = err.Error()
		}
		h.log.InfoContext(r.Context(), "metrics scrape refused", "reason", reason)
		w.Header().Set("WWW-Authenticate", `Bearer realm="syncr"`)
		http.Error(w, "unauthenticated: send a personal API token as \"Authorization: Bearer syncr_pat_...\"", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	set, err := h.collect(ctx, caller.Actor)
	if err != nil {
		h.log.ErrorContext(ctx, "cannot collect the metrics", "error", err.Error())
		http.Error(w, "cannot collect the metrics", http.StatusInternalServerError)
		return
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(set)
	if caller.Actor.IsAdmin() {
		reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	}
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		ErrorLog:          slog.NewLogLogger(h.log.Handler(), slog.LevelError),
		ErrorHandling:     promhttp.HTTPErrorOnError,
		EnableOpenMetrics: true,
	}).ServeHTTP(w, r)
}

// metricSet is one scrape's metrics, collected before the registry
// gathers them (so a store error fails the scrape with a status, not with
// a partial page). It is an unchecked collector: its metrics differ from
// scrape to scrape.
type metricSet struct{ metrics []prometheus.Metric }

func (*metricSet) Describe(chan<- *prometheus.Desc) {}

func (m *metricSet) Collect(ch chan<- prometheus.Metric) {
	for _, x := range m.metrics {
		ch <- x
	}
}

func (m *metricSet) add(d *prometheus.Desc, t prometheus.ValueType, v float64, labels ...string) {
	m.metrics = append(m.metrics, prometheus.MustNewConstMetric(d, t, v, labels...))
}

func (m *metricSet) gauge(d *prometheus.Desc, v float64, labels ...string) {
	m.add(d, prometheus.GaugeValue, v, labels...)
}

// timestamp adds a Unix-seconds gauge unless t is zero.
func (m *metricSet) timestamp(d *prometheus.Desc, t time.Time, labels ...string) {
	if !t.IsZero() {
		m.gauge(d, float64(t.UnixNano())/1e9, labels...)
	}
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// collect reads everything a scrape by actor shows.
func (h *metricsHandler) collect(ctx context.Context, actor domain.Actor) (*metricSet, error) {
	d := &h.descs
	set := &metricSet{}
	set.gauge(d.buildInfo, 1, h.build.Version, h.build.Commit, h.build.GoVersion)

	ws, err := h.workers.List(ctx, workers.ListFilter{IncludeRemoved: true})
	if err != nil {
		return nil, fmt.Errorf("workers: %w", err)
	}
	names := make(map[domain.WorkerID]string, len(ws))
	byStatus := map[domain.WorkerStatus]int{domain.WorkerOnline: 0, domain.WorkerOffline: 0, domain.WorkerPending: 0}
	for _, w := range ws {
		names[w.ID] = w.Name
		if w.Removed() {
			continue
		}
		st := h.workers.Status(w)
		byStatus[st]++
		set.gauge(d.workerUp, boolValue(st == domain.WorkerOnline), w.Name)
		set.timestamp(d.workerLastSeen, w.Link.LastSeen, w.Name)
		set.gauge(d.workerInfo, 1, w.Name, string(w.Mode), w.Info.Version, w.Info.OS, w.Info.Arch)
		set.gauge(d.workerOutdated, boolValue(workerOutdated(w.Info, h.version)), w.Name)
	}
	for st, n := range byStatus {
		set.gauge(d.workers, float64(n), string(st))
	}
	workerName := func(id domain.WorkerID) string {
		if n, ok := names[id]; ok {
			return n
		}
		return id.String()
	}

	if err := h.collectJobs(ctx, set, actor, workerName); err != nil {
		return nil, err
	}
	if err := h.collectSchedules(ctx, set, actor); err != nil {
		return nil, err
	}
	if err := h.collectRepositories(ctx, set, actor); err != nil {
		return nil, err
	}
	return set, nil
}

// collectJobs adds the jobs' gauges and counters. Rows are merged by their
// labels: a removed worker may share its name with a live one.
func (h *metricsHandler) collectJobs(ctx context.Context, set *metricSet, actor domain.Actor, workerName func(domain.WorkerID) string) error {
	d := &h.descs
	allUsers := actor.IsAdmin()
	unfinished, err := h.jobs.UnfinishedCounts(ctx, actor, allUsers)
	if err != nil {
		return fmt.Errorf("unfinished jobs: %w", err)
	}
	type active struct{ status, kind, worker string }
	type kindWorker struct{ kind, worker string }
	counts := map[active]int64{}
	oldest := map[kindWorker]time.Time{}
	for _, r := range unfinished {
		w := workerName(r.WorkerID)
		counts[active{string(r.Status), r.Kind, w}] += r.Jobs
		if k := (kindWorker{r.Kind, w}); r.Status == domain.JobQueued && (oldest[k].IsZero() || r.OldestQueuedAt.Before(oldest[k])) {
			oldest[k] = r.OldestQueuedAt
		}
	}
	for k, n := range counts {
		set.gauge(d.jobs, float64(n), k.status, k.kind, k.worker)
	}
	for k, t := range oldest {
		set.timestamp(d.oldestQueued, t, k.kind, k.worker)
	}

	bounds := metricsRunTimeBounds()
	totals, err := h.jobs.Totals(ctx, actor, jobs.TotalsFilter{AllUsers: allUsers, RunTimeBounds: bounds})
	if err != nil {
		return fmt.Errorf("job totals: %w", err)
	}
	type outcome struct{ kind, worker, status, code string }
	type perKindWorker struct {
		jobs.Sums
		buckets []uint64 // per run-time bucket, not cumulative; the last is above every bound
	}
	finished := map[outcome]int64{}
	sums := map[kindWorker]*perKindWorker{}
	for _, r := range totals {
		w := workerName(r.WorkerID)
		finished[outcome{r.Kind, w, string(r.Status), r.ErrorCode}] += r.Jobs
		k := kindWorker{r.Kind, w}
		s := sums[k]
		if s == nil {
			s = &perKindWorker{buckets: make([]uint64, len(bounds)+1)}
			sums[k] = s
		}
		s.Add(r.Sums)
		if r.RunTimeBucket >= 0 && r.RunTimeBucket < len(s.buckets) {
			s.buckets[r.RunTimeBucket] += uint64(max(r.Started, 0))
		}
	}
	for k, n := range finished {
		set.add(d.finished, prometheus.CounterValue, float64(n), k.kind, k.worker, k.status, k.code)
	}
	for k, s := range sums {
		set.add(d.bytes, prometheus.CounterValue, float64(s.Bytes), k.kind, k.worker)
		set.add(d.files, prometheus.CounterValue, float64(s.Files), k.kind, k.worker)
		cumulative := make(map[float64]uint64, len(bounds))
		var acc uint64
		for i, b := range bounds {
			acc += s.buckets[i]
			cumulative[b.Seconds()] = acc
		}
		set.metrics = append(set.metrics,
			prometheus.MustNewConstHistogram(d.runTime, uint64(max(s.Started, 0)), microsToSeconds(s.RunTimeMicros), cumulative, k.kind, k.worker),
			prometheus.MustNewConstSummary(d.queueWait, uint64(max(s.Dispatched, 0)), microsToSeconds(s.QueueWaitMicros), nil, k.kind, k.worker))
	}
	return nil
}

// collectSchedules adds the actor's schedules.
func (h *metricsHandler) collectSchedules(ctx context.Context, set *metricSet, actor domain.Actor) error {
	d := &h.descs
	health, err := h.schedules.Health(ctx, actor)
	if err != nil {
		return fmt.Errorf("schedules: %w", err)
	}
	for _, hs := range health {
		sc := hs.Schedule
		set.gauge(d.scheduleInfo, 1, sc.Name, sc.Kind, hs.GroupName)
		set.gauge(d.scheduleEnabled, boolValue(sc.Enabled), sc.Name)
		set.timestamp(d.scheduleNextRun, sc.NextRunAt, sc.Name)
		set.timestamp(d.scheduleLastSuccess, hs.LastSuccessAt, sc.Name)
		if !hs.HasRun {
			continue
		}
		set.timestamp(d.scheduleLastRun, hs.LatestRun.ScheduledFor, sc.Name)
		if state, ok := lastRunState(hs.LatestRun); ok {
			for _, s := range runStates() {
				set.gauge(d.scheduleLastRunStatus, boolValue(s == state), sc.Name, s)
			}
		}
	}
	return nil
}

// lastRunState is the state of r in syncr_schedule_last_run_status; false
// for a launched run whose job is unknown.
func lastRunState(r schedules.Run) (string, bool) {
	switch r.Status {
	case schedules.RunPending, schedules.RunLaunching:
		return runStateActive, true
	case schedules.RunMissed:
		return runStateMissed, true
	case schedules.RunFailed:
		return runStateNotStarted, true
	case schedules.RunCancelled:
		return runStateCancelled, true
	case schedules.RunLaunched:
		switch r.JobStatus {
		case domain.JobQueued, domain.JobDispatched, domain.JobRunning:
			return runStateActive, true
		case domain.JobSucceeded:
			return runStateSucceeded, true
		case domain.JobFailed:
			return runStateFailed, true
		case domain.JobInterrupted:
			return runStateInterrupted, true
		case domain.JobCancelled:
			return runStateCancelled, true
		}
	}
	return "", false
}

// collectRepositories adds the actor's restic repositories.
func (h *metricsHandler) collectRepositories(ctx context.Context, set *metricSet, actor domain.Actor) error {
	if h.restic == nil {
		return nil
	}
	d := &h.descs
	repos, err := h.restic.List(ctx, actor, restic.ListFilter{})
	if err != nil {
		return fmt.Errorf("restic repositories: %w", err)
	}
	for _, r := range repos {
		if r.Deleted() {
			continue
		}
		st := r.Stats
		set.timestamp(d.repoRefreshed, st.SnapshotsRefreshedAt, r.Name)
		if !st.SnapshotsRefreshedAt.IsZero() {
			set.gauge(d.repoSnapshots, float64(st.SnapshotCount), r.Name)
		}
		set.timestamp(d.repoLatestSnapshot, st.LatestSnapshotAt, r.Name)
		if st.LatestSnapshotBytes > 0 {
			set.gauge(d.repoLatestSnapshotBytes, float64(st.LatestSnapshotBytes), r.Name)
		}
		if b := st.LastBackup; !b.ID.IsZero() {
			set.timestamp(d.repoLastBackup, b.FinishedAt, r.Name)
			if b.Status.Terminal() {
				set.gauge(d.repoLastBackupSucceeded, boolValue(b.Status == domain.JobSucceeded), r.Name)
			}
		}
	}
	return nil
}

// metricDescs are the metric descriptions, built once per handler.
type metricDescs struct {
	buildInfo *prometheus.Desc

	workers, workerUp, workerLastSeen, workerInfo, workerOutdated *prometheus.Desc

	jobs, oldestQueued, finished, bytes, files, runTime, queueWait *prometheus.Desc

	scheduleInfo, scheduleEnabled, scheduleNextRun, scheduleLastRun, scheduleLastSuccess, scheduleLastRunStatus *prometheus.Desc

	repoSnapshots, repoRefreshed, repoLatestSnapshot, repoLatestSnapshotBytes, repoLastBackup, repoLastBackupSucceeded *prometheus.Desc
}

func newMetricDescs() metricDescs {
	desc := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc("syncr_"+name, help, labels, nil)
	}
	return metricDescs{
		buildInfo: desc("build_info", "The controller's build; always 1.", "version", "commit", "go_version"),

		workers:        desc("workers", "Workers by status (online, offline, pending: enrolled but never connected).", "status"),
		workerUp:       desc("worker_up", "Whether the worker is online (1) or not (0).", "worker"),
		workerLastSeen: desc("worker_last_seen_timestamp_seconds", "When the controller last heard from the worker.", "worker"),
		workerInfo:     desc("worker_info", "What the worker reported about itself; always 1.", "worker", "mode", "version", "os", "arch"),
		workerOutdated: desc("worker_outdated", "Whether the worker should be upgraded to the controller's version (1) or not (0).", "worker"),

		jobs:         desc("jobs", "Jobs that have not finished, by status (queued, dispatched, running).", "status", "kind", "worker"),
		oldestQueued: desc("jobs_oldest_queued_timestamp_seconds", "When the longest-waiting queued job entered the queue.", "kind", "worker"),
		finished:     desc("jobs_finished_total", "Finished jobs, by final status and error code (empty when there is none).", "kind", "worker", "status", "error_code"),
		bytes:        desc("job_transferred_bytes_total", "Bytes the finished jobs transferred.", "kind", "worker"),
		files:        desc("job_transferred_files_total", "Files the finished jobs transferred.", "kind", "worker"),
		runTime:      desc("job_run_duration_seconds", "How long the finished jobs that started ran (from start to end, the worker's clock).", "kind", "worker"),
		queueWait:    desc("job_queue_wait_seconds", "How long the finished jobs a worker accepted waited for it (from creation to acceptance).", "kind", "worker"),

		scheduleInfo:          desc("schedule_info", "The schedule, the job kind it runs and its group (empty for none); always 1.", "schedule", "kind", "group"),
		scheduleEnabled:       desc("schedule_enabled", "Whether the schedule is enabled (1) or paused (0).", "schedule"),
		scheduleNextRun:       desc("schedule_next_run_timestamp_seconds", "When the schedule runs next.", "schedule"),
		scheduleLastRun:       desc("schedule_last_run_timestamp_seconds", "When the schedule's newest run was due.", "schedule"),
		scheduleLastSuccess:   desc("schedule_last_success_timestamp_seconds", "When the newest successful job the schedule launched finished.", "schedule"),
		scheduleLastRunStatus: desc("schedule_last_run_status", "The state of the schedule's newest run: 1 for the current state, 0 for the others.", "schedule", "status"),

		repoSnapshots:           desc("restic_repository_snapshots", "Snapshots in the repository, as of the snapshot list's last refresh.", "repository"),
		repoRefreshed:           desc("restic_repository_snapshots_refreshed_timestamp_seconds", "When the repository's snapshot list was last refreshed.", "repository"),
		repoLatestSnapshot:      desc("restic_repository_latest_snapshot_timestamp_seconds", "When the repository's newest snapshot was taken.", "repository"),
		repoLatestSnapshotBytes: desc("restic_repository_latest_snapshot_size_bytes", "Bytes the newest snapshot's backup processed (the size of what it backed up).", "repository"),
		repoLastBackup:          desc("restic_repository_last_backup_timestamp_seconds", "When the newest backup job of the repository finished.", "repository"),
		repoLastBackupSucceeded: desc("restic_repository_last_backup_succeeded", "Whether the newest finished backup job of the repository succeeded (1) or not (0).", "repository"),
	}
}
