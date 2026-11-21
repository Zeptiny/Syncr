package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/broker"
	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/adapters/memstore"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/version"
)

// fakeMetricsRestic lists canned repositories and records the actors.
type fakeMetricsRestic struct {
	ResticService

	mu     sync.Mutex
	repos  []restic.Repo
	actors []domain.Actor
}

func (f *fakeMetricsRestic) List(_ context.Context, actor domain.Actor, _ restic.ListFilter) ([]restic.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actors = append(f.actors, actor)
	return f.repos, nil
}

// metricsEnv is a metrics handler over the dashboard env (jobs with their
// activity port, the env's workers), fake schedules and repositories.
type metricsEnv struct {
	*dashEnv
	h     *metricsHandler
	repos *fakeMetricsRestic
}

func newMetricsEnv(t *testing.T) *metricsEnv {
	t.Helper()
	d := newDashEnv(t)
	repos := &fakeMetricsRestic{}
	h := newMetricsHandler(Deps{
		Workers: d.wsvc, Schedules: d.sched, Restic: repos,
		BuildInfo: version.Info{Version: "v2.1.0", Commit: "abc123", GoVersion: "go1.27.4"},
		Install:   InstallConfig{Version: "v2.1.0"},
	}, testAuth{}, d.jobs, newTestLogger(d.logs))
	return &metricsEnv{dashEnv: d, h: h, repos: repos}
}

// job stores a job of owner on worker that was created at t0, accepted
// after wait, started at t0 + 10 s and ran for run (zero: never started),
// then ended with status and code.
func (m *metricsEnv) job(t *testing.T, owner domain.Actor, worker domain.WorkerID, kind string, status domain.JobStatus, code string, wait, run time.Duration, bytes int64) {
	t.Helper()
	ctx := context.Background()
	j, err := m.jobs.Create(ctx, owner, jobs.CreateParams{WorkerID: worker, Kind: kind, Definition: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if status.Terminal() {
		t0 := j.CreatedAt
		if wait > 0 {
			j.DispatchedAt, j.Epoch = t0.Add(wait), "e1"
		}
		j.FinishedAt = t0.Add(time.Second)
		if run > 0 {
			j.StartedAt = t0.Add(10 * time.Second)
			j.FinishedAt = j.StartedAt.Add(run)
		}
		j.Stats = jobs.Stats{Bytes: bytes, Files: 1}
		if code != "" {
			j.Error = jobs.JobError{Code: code, Message: "boom"}
		}
	}
	j.Status = status
	if _, err := m.store.UpdateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
}

// scraped is the outcome of a scrape.
type scraped struct {
	StatusCode int
	Header     http.Header
}

// scrape GETs /metrics with the X-Test-Actor header value auth (none when
// empty) and the header pairs, and returns the status, headers and body.
func (m *metricsEnv) scrape(t *testing.T, auth string, header ...string) (scraped, string) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, MetricsPath, nil)
	if auth != "" {
		req.Header.Set(testActorHeader, auth)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	m.h.ServeHTTP(rec, req)
	return scraped{StatusCode: rec.Code, Header: rec.Header()}, rec.Body.String()
}

func wantLines(t *testing.T, who, body string, lines ...string) {
	t.Helper()
	got := strings.Split(body, "\n")
	for _, l := range lines {
		if !slices.Contains(got, l) {
			t.Errorf("%s: missing line %q", who, l)
		}
	}
}

func refuseLines(t *testing.T, who, body string, prefixes ...string) {
	t.Helper()
	for _, l := range strings.Split(body, "\n") {
		for _, p := range prefixes {
			if strings.HasPrefix(l, p) {
				t.Errorf("%s: unexpected line %q", who, l)
			}
		}
	}
}

func TestMetrics(t *testing.T) {
	t.Parallel()
	m := newMetricsEnv(t)
	other := m.addWorker()
	const transfer, backup = "rclone_transfer", "restic_backup"
	// The user: two successes (run 45 s and 90 s, waits 2 s and 4 s), a
	// failure that never started, a queued job; the other user: a success
	// on the second worker; the admin: nothing.
	m.job(t, testUser, m.worker, transfer, domain.JobSucceeded, "", 2*time.Second, 45*time.Second, 100)
	m.job(t, testUser, m.worker, transfer, domain.JobSucceeded, "", 4*time.Second, 90*time.Second, 50)
	m.job(t, testUser, m.worker, backup, domain.JobFailed, "execution_failed", 0, 0, 0)
	m.job(t, testUser, m.worker, transfer, domain.JobQueued, "", 0, 0, 0)
	m.job(t, testOther, other, transfer, domain.JobSucceeded, "", time.Second, time.Hour+time.Second, 7)

	success := time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)
	m.sched.health = []schedules.Health{
		{
			Schedule:      schedules.Schedule{Name: "Nightly", Kind: backup, Enabled: true, NextRunAt: time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC)},
			HasRun:        true,
			LatestRun:     schedules.Run{Status: schedules.RunLaunched, JobStatus: domain.JobFailed, ScheduledFor: time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)},
			LastSuccessAt: success,
		},
		{Schedule: schedules.Schedule{Name: `Paused "old"`, Kind: transfer}},
	}
	m.repos.repos = []restic.Repo{
		{Name: "Photos", Stats: restic.RepoStats{
			SnapshotCount: 12, SnapshotsRefreshedAt: time.Date(2026, 11, 4, 11, 0, 0, 0, time.UTC),
			LatestSnapshotAt: time.Date(2026, 11, 4, 3, 30, 0, 0, time.UTC), LatestSnapshotBytes: 5 << 30,
			LastBackup: restic.JobRef{ID: domain.NewJobID(), Status: domain.JobSucceeded, FinishedAt: time.Date(2026, 11, 4, 3, 40, 0, 0, time.UTC)},
		}},
		{Name: "Gone", DeletedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
	}

	resp, body := m.scrape(t, actorHeader(testUser, "token"))
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("user scrape = %d %q:\n%s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	wantLines(t, "user", body,
		`syncr_build_info{commit="abc123",go_version="go1.27.4",version="v2.1.0"} 1`,
		// Workers are shared: the user sees both.
		`syncr_workers{status="pending"} 2`,
		`syncr_workers{status="online"} 0`,
		`syncr_worker_up{worker="worker"} 0`,
		`syncr_worker_info{arch="",mode="active",os="",version="",worker="worker"} 1`,
		`syncr_worker_outdated{worker="worker"} 0`,
		// The user's jobs only.
		`syncr_jobs{kind="rclone_transfer",status="queued",worker="worker"} 1`,
		`syncr_jobs_finished_total{error_code="",kind="rclone_transfer",status="succeeded",worker="worker"} 2`,
		`syncr_jobs_finished_total{error_code="execution_failed",kind="restic_backup",status="failed",worker="worker"} 1`,
		`syncr_job_transferred_bytes_total{kind="rclone_transfer",worker="worker"} 150`,
		`syncr_job_transferred_files_total{kind="rclone_transfer",worker="worker"} 2`,
		`syncr_job_run_duration_seconds_bucket{kind="rclone_transfer",worker="worker",le="30"} 0`,
		`syncr_job_run_duration_seconds_bucket{kind="rclone_transfer",worker="worker",le="60"} 1`,
		`syncr_job_run_duration_seconds_bucket{kind="rclone_transfer",worker="worker",le="300"} 2`,
		`syncr_job_run_duration_seconds_bucket{kind="rclone_transfer",worker="worker",le="+Inf"} 2`,
		`syncr_job_run_duration_seconds_sum{kind="rclone_transfer",worker="worker"} 135`,
		`syncr_job_run_duration_seconds_count{kind="rclone_transfer",worker="worker"} 2`,
		`syncr_job_run_duration_seconds_count{kind="restic_backup",worker="worker"} 0`,
		`syncr_job_queue_wait_seconds_sum{kind="rclone_transfer",worker="worker"} 6`,
		`syncr_job_queue_wait_seconds_count{kind="rclone_transfer",worker="worker"} 2`,
		// The user's schedules (label values escaped).
		`syncr_schedule_info{kind="restic_backup",schedule="Nightly"} 1`,
		`syncr_schedule_enabled{schedule="Nightly"} 1`,
		`syncr_schedule_enabled{schedule="Paused \"old\""} 0`,
		`syncr_schedule_next_run_timestamp_seconds{schedule="Nightly"} 1.7938476e+09`,
		`syncr_schedule_last_run_timestamp_seconds{schedule="Nightly"} 1.7937612e+09`,
		`syncr_schedule_last_success_timestamp_seconds{schedule="Nightly"} 1.7937612e+09`,
		`syncr_schedule_last_run_status{schedule="Nightly",status="failed"} 1`,
		`syncr_schedule_last_run_status{schedule="Nightly",status="succeeded"} 0`,
		`syncr_schedule_last_run_status{schedule="Nightly",status="active"} 0`,
		// The user's repositories; deleted ones are left out.
		`syncr_restic_repository_snapshots{repository="Photos"} 12`,
		`syncr_restic_repository_latest_snapshot_size_bytes{repository="Photos"} 5.36870912e+09`,
		`syncr_restic_repository_last_backup_succeeded{repository="Photos"} 1`,
	)
	refuseLines(t, "user", body,
		`syncr_jobs_finished_total{error_code="",kind="rclone_transfer",status="succeeded",worker="worker-`, // the other user's job
		`syncr_schedule_last_run_status{schedule="Paused`,                                                   // never ran
		`syncr_schedule_last_success_timestamp_seconds{schedule="Paused`,
		`syncr_restic_repository_snapshots{repository="Gone"}`,
		`go_goroutines`, `process_`, // admins only
	)

	resp, body = m.scrape(t, actorHeader(testAdmin, "token"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin scrape = %d:\n%s", resp.StatusCode, body)
	}
	wantLines(t, "admin", body,
		`syncr_jobs_finished_total{error_code="",kind="rclone_transfer",status="succeeded",worker="worker"} 2`,
		`syncr_job_run_duration_seconds_bucket{kind="rclone_transfer",worker="worker-`+other.String()+`",le="3600"} 0`,
		`syncr_job_run_duration_seconds_bucket{kind="rclone_transfer",worker="worker-`+other.String()+`",le="7200"} 1`,
	)
	if !strings.Contains(body, "\ngo_goroutines ") {
		t.Error("admin: no Go runtime metrics")
	}
	// Schedules and repositories are always the scraper's own.
	if !slices.Equal(m.sched.actors, []domain.Actor{testUser, testAdmin}) || !slices.Equal(m.repos.actors, []domain.Actor{testUser, testAdmin}) {
		t.Errorf("schedules read for %v, repositories for %v, want the scrapers", m.sched.actors, m.repos.actors)
	}

	_, body = m.scrape(t, actorHeader(testUser, "token"), "Accept", "application/openmetrics-text; version=1.0.0")
	if !strings.HasSuffix(body, "# EOF\n") {
		t.Errorf("OpenMetrics scrape does not end with # EOF:\n%s", body)
	}
}

func TestMetricsMergesWorkerNames(t *testing.T) {
	t.Parallel()
	m := newMetricsEnv(t)
	m.job(t, testUser, m.worker, "rclone_transfer", domain.JobSucceeded, "", time.Second, time.Second, 1)
	// The worker is removed and a new one takes its name: both workers'
	// jobs count under one name, instead of failing the scrape with a
	// repeated series.
	w, err := m.store.RemoveWorker(context.Background(), m.worker, m.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	again := domain.NewWorkerID()
	w.ID, w.RemovedAt = again, time.Time{}
	if err := m.store.CreateWorker(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	m.job(t, testUser, again, "rclone_transfer", domain.JobSucceeded, "", time.Second, time.Second, 1)
	resp, body := m.scrape(t, actorHeader(testUser, "token"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scrape = %d:\n%s", resp.StatusCode, body)
	}
	wantLines(t, "user", body, `syncr_jobs_finished_total{error_code="",kind="rclone_transfer",status="succeeded",worker="worker"} 2`,
		`syncr_workers{status="pending"} 1`)
}

func TestMetricsAuthentication(t *testing.T) {
	t.Parallel()
	m := newMetricsEnv(t)
	for name, auth := range map[string]string{
		"no credentials": "",
		"a session":      actorHeader(testAdmin, "session"),
		"bad header":     "nonsense",
	} {
		resp, body := m.scrape(t, auth)
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != `Bearer realm="syncr"` {
			t.Errorf("%s: %d %q %q", name, resp.StatusCode, resp.Header.Get("WWW-Authenticate"), body)
		}
		if strings.Contains(body, "syncr_build_info") {
			t.Errorf("%s: metrics leaked: %s", name, body)
		}
	}
	if len(m.sched.actors) != 0 {
		t.Errorf("refused scrapes read the schedules for %v", m.sched.actors)
	}
}

func TestMetricsStoreError(t *testing.T) {
	t.Parallel()
	m := newMetricsEnv(t)
	m.sched.healthErr = errors.New("database is down")
	resp, body := m.scrape(t, actorHeader(testUser, "token"))
	if resp.StatusCode != http.StatusInternalServerError || strings.Contains(body, "syncr_build_info") || strings.Contains(body, "database") {
		t.Errorf("scrape with a failing store = %d %q, want 500 without metrics or details", resp.StatusCode, body)
	}
	if !strings.Contains(m.logs.String(), "database is down") {
		t.Errorf("the error was not logged:\n%s", m.logs.String())
	}
}

// TestMetricsMounted checks that NewHandler serves GET /metrics when the
// jobs service has its activity port and the schedules are wired, and that
// the web UI never answers the path otherwise.
func TestMetricsMounted(t *testing.T) {
	t.Parallel()
	store := memstore.New()
	js, err := jobs.New(jobs.Deps{
		Jobs: store, Samples: store, Clock: fakeClock{}, IDs: &idgen.Sequential{},
		Broker: broker.New(), Notifier: jobs.NopNotifier{}, Waker: nopWaker{}, Remotes: newFakeJobRemotes(), Activity: store,
	}, jobs.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ui := testUI()
	mounted := newEnv(t, func(d *Deps) { d.Jobs, d.Schedules, d.UI = js, &fakeDashSchedules{}, ui })
	unmounted := newEnv(t, func(d *Deps) { d.UI = ui })
	for name, tt := range map[string]struct {
		e    *testEnv
		auth string
		want int
	}{
		"mounted, a token":     {mounted, actorHeader(testUser, "token"), http.StatusOK},
		"mounted, no token":    {mounted, "", http.StatusUnauthorized},
		"not mounted, a token": {unmounted, actorHeader(testUser, "token"), http.StatusNotFound},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tt.e.srv.URL+MetricsPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tt.auth != "" {
			req.Header.Set(testActorHeader, tt.auth)
		}
		resp, err := tt.e.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != tt.want || strings.Contains(string(body), "<html") {
			t.Errorf("%s: GET /metrics = %d %.80q, want %d", name, resp.StatusCode, body, tt.want)
		}
	}
}

// fakeClock is a fixed clock for services the test does not advance.
type fakeClock struct{}

func (fakeClock) Now() time.Time { return time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC) }
