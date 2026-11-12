//go:build integration

package postgres

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

func testTime() time.Time { return time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC) }

func newTestWorker() workers.Worker {
	id := domain.NewWorkerID()
	return workers.Worker{
		ID: id, Name: "w-" + id.String(), Mode: domain.WorkerActive, // names are unique among live workers
		SecretEnc: []byte("enc"), CreatedAt: testTime(),
	}
}

func newTestJob(w domain.WorkerID, owner domain.UserID) jobs.Job {
	return jobs.Job{
		ID: domain.NewJobID(), WorkerID: w, OwnerID: owner, Kind: "rclone_transfer", Status: domain.JobQueued,
		Definition: []byte(`{"rclone_transfer": {}}`), CreatedAt: testTime(), QueuedAt: testTime(), ExitCode: -1, Version: 1,
	}
}

// TestCountersAreBigint guards the "all byte and counter columns are 64-bit"
// rule against schema edits.
func TestCountersAreBigint(t *testing.T) {
	s := newMigratedStore(t)
	rows, err := s.pool.Query(t.Context(), `
		SELECT table_name || '.' || column_name, data_type
		FROM information_schema.columns
		WHERE table_schema = 'public'
		  AND ((table_name = 'jobs' AND column_name LIKE 'stat\_%')
		    OR (table_name = 'job_samples' AND column_name <> 'job_id' AND column_name <> 't')
		    OR (table_name = 'workers' AND column_name = 'ack_seq'))`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var col, typ string
		if err := rows.Scan(&col, &typ); err != nil {
			t.Fatal(err)
		}
		n++
		if typ != "bigint" {
			t.Errorf("column %s has type %s, want bigint", col, typ)
		}
	}
	if n != 9+9+1 {
		t.Errorf("checked %d counter columns, want 19", n)
	}
}

// TestIndexes checks that the hot-path indexes exist and are partial.
func TestIndexes(t *testing.T) {
	s := newMigratedStore(t)
	for name, want := range map[string]string{
		"jobs_worker_unfinished_idx": "status = ANY",
		"jobs_queued_idx":            "status = 'queued'",
		// One pending run per schedule (migration 00005).
		"schedule_runs_one_pending_key": "status = 'pending'",
		// The dashboard's problem lookups (migration 00006).
		"schedule_runs_problem_idx": "status = ANY",
		"jobs_failed_idx":           "status = ANY",
	} {
		var def string
		err := s.pool.QueryRow(t.Context(), "SELECT indexdef FROM pg_indexes WHERE indexname = $1", name).Scan(&def)
		if err != nil {
			t.Errorf("index %s: %v", name, err)
			continue
		}
		if !strings.Contains(def, "WHERE") || !strings.Contains(def, want) {
			t.Errorf("index %s = %q, want a partial index containing %q", name, def, want)
		}
	}
}

// TestUnfinishedJobsUsesPartialIndex proves the dispatcher's query matches its
// partial index (the planner can only pick it if the predicate is implied).
func TestUnfinishedJobsUsesPartialIndex(t *testing.T) {
	s := newMigratedStore(t)
	plan := explain(t, s, `SELECT * FROM jobs
		WHERE worker_id = '00000000-0000-0000-0000-000000000001' AND status IN ('queued', 'dispatched', 'running')
		ORDER BY id`, true)
	if !strings.Contains(plan, "jobs_worker_unfinished_idx") {
		t.Errorf("plan does not use jobs_worker_unfinished_idx:\n%s", plan)
	}
}

// TestDashboardQueriesUsePartialIndexes proves that the dashboard's problem
// lookups use their partial indexes on a history of successful jobs and
// launched runs, instead of walking it. The status filters are written as
// ListJobs, ListRuns and CountRuns write them (cardinality(statuses) = 0
// OR status = ANY (statuses)): they imply the index predicates once the
// statuses are known.
func TestDashboardQueriesUsePartialIndexes(t *testing.T) {
	s := newMigratedStore(t)
	ctx := t.Context()
	owner := seedUser(t, s)
	w := newTestWorker()
	if err := s.CreateWorker(ctx, w); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO schedules (id, owner_id, name, cron, timezone, kind, definition, worker_id,
		                        missed_action, overlap, enabled, created_at, updated_at)
		 VALUES (gen_random_uuid(), $1, 'hourly', '0 * * * *', 'UTC', 'transfer', '{}', $2, 'skip', 'skip', false, now(), now())`,
			[]any{pgID(owner), pgID(w.ID)}},
		{`INSERT INTO jobs (id, worker_id, owner_id, kind, status, definition, created_at, queued_at, finished_at)
		 SELECT gen_random_uuid(), $2, $1, 'transfer', 'succeeded', '{}', now(), now(), now()
		 FROM generate_series(1, 5000)`,
			[]any{pgID(owner), pgID(w.ID)}},
		{`INSERT INTO schedule_runs (id, schedule_id, trigger, scheduled_for, deadline, status, job_id, created_at, updated_at)
		 SELECT gen_random_uuid(), (SELECT id FROM schedules), 'cron', now() - n * interval '1 hour', now(), 'launched',
		        gen_random_uuid(), now(), now()
		 FROM generate_series(1, 5000) n`,
			nil},
	} {
		if _, err := s.pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, "ANALYZE jobs, schedule_runs, schedules"); err != nil {
		t.Fatal(err)
	}
	ownerLit := "'" + owner.UUID().String() + "'::uuid"
	for name, tt := range map[string]struct{ query, index string }{
		"recent failures": {
			`SELECT * FROM jobs
			WHERE owner_id = ` + ownerLit + `
			  AND (cardinality('{failed,interrupted}'::text[]) = 0 OR status = ANY ('{failed,interrupted}'::text[]))
			ORDER BY id DESC LIMIT 5`,
			"jobs_failed_idx",
		},
		"recent problems": {
			`SELECT r.* FROM schedule_runs r
			WHERE EXISTS (SELECT 1 FROM schedules s WHERE s.id = r.schedule_id AND s.owner_id = ` + ownerLit + `)
			  AND (cardinality('{missed,failed}'::text[]) = 0 OR r.status = ANY ('{missed,failed}'::text[]))
			ORDER BY r.id DESC LIMIT 5`,
			"schedule_runs_problem_idx",
		},
		"missed count": {
			`SELECT count(*) FROM schedule_runs r
			WHERE EXISTS (SELECT 1 FROM schedules s WHERE s.id = r.schedule_id AND s.owner_id = ` + ownerLit + `)
			  AND (cardinality('{missed}'::text[]) = 0 OR r.status = ANY ('{missed}'::text[]))
			  AND r.scheduled_for >= now() - interval '30 days'`,
			"schedule_runs_problem_idx",
		},
	} {
		if plan := explain(t, s, tt.query, false); !strings.Contains(plan, tt.index) {
			t.Errorf("%s: plan does not use %s:\n%s", name, tt.index, plan)
		}
	}
}

// explain returns the plan of query. noSeqScan makes sequential scans
// unattractive, so that on an empty table the plan shows whether an index
// is usable at all.
func explain(t *testing.T, s *Store, query string, noSeqScan bool) string {
	t.Helper()
	ctx := t.Context()
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if noSeqScan {
		if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := conn.Query(ctx, "EXPLAIN "+query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(plan, "\n")
}

func TestLargeCountersRoundTrip(t *testing.T) {
	s := newMigratedStore(t)
	ctx := t.Context()
	owner := seedUser(t, s)
	w := newTestWorker()
	if err := s.CreateWorker(ctx, w); err != nil {
		t.Fatal(err)
	}
	const big = int64(1)<<62 + 12345
	j := newTestJob(w.ID, owner)
	j.Stats = jobs.Stats{Bytes: big, TotalBytes: big + 1, Files: 1 << 40, TotalFiles: 1 << 41, Checks: 1 << 33, Elapsed: 100 * time.Hour}
	if err := s.CreateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetJob(ctx, j.ID)
	if err != nil || got.Stats != j.Stats {
		t.Errorf("job stats = %+v (%v), want %+v", got.Stats, err, j.Stats)
	}
	smp := jobs.Sample{JobID: j.ID, Time: testTime(), Stats: j.Stats}
	if err := s.AppendJobSample(ctx, smp); err != nil {
		t.Fatal(err)
	}
	if latest, ok, err := s.LatestJobSample(ctx, j.ID); err != nil || !ok || latest.Stats != j.Stats {
		t.Errorf("sample = (%+v, %v, %v)", latest, ok, err)
	}
}

func TestTimesRoundTrip(t *testing.T) {
	s := newMigratedStore(t)
	ctx := t.Context()
	w := newTestWorker()
	w.CreatedAt = time.Date(2026, 11, 4, 12, 0, 0, 123456789, time.FixedZone("x", 3600))
	w.Info.ReportedAt = w.CreatedAt
	if err := s.CreateWorker(ctx, w); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetWorker(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Stored with microsecond precision, read back in UTC.
	if want := w.CreatedAt.Truncate(time.Microsecond); !got.CreatedAt.Equal(want) || got.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want)
	}
	// Info keeps full precision (it is JSON).
	if !got.Info.ReportedAt.Equal(w.Info.ReportedAt) {
		t.Errorf("Info.ReportedAt = %v, want %v", got.Info.ReportedAt, w.Info.ReportedAt)
	}
	// Zero times round-trip as zero (NULL in nullable columns).
	if !got.RemovedAt.IsZero() || !got.Link.LastSeen.IsZero() || !got.Link.EpochStartedAt.IsZero() {
		t.Errorf("zero times came back set: %+v", got)
	}
	var nulls bool
	if err := s.pool.QueryRow(ctx, "SELECT removed_at IS NULL AND last_seen IS NULL AND epoch_started_at IS NULL FROM workers WHERE id = $1", pgID(w.ID)).Scan(&nulls); err != nil || !nulls {
		t.Errorf("zero times not stored as NULL: %v, %v", nulls, err)
	}
}

func TestInvalidValuesAreRejected(t *testing.T) {
	s := newMigratedStore(t)
	ctx := t.Context()
	owner := seedUser(t, s)
	w := newTestWorker()
	w.Mode = "sideways"
	if err := s.CreateWorker(ctx, w); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("CreateWorker(bad mode) = %v, want ErrInvalidArgument", err)
	}
	w = newTestWorker()
	if err := s.CreateWorker(ctx, w); err != nil {
		t.Fatal(err)
	}
	j := newTestJob(w.ID, owner)
	j.Status = "exploded"
	if err := s.CreateJob(ctx, j); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("CreateJob(bad status) = %v, want ErrInvalidArgument", err)
	}
	w2 := newTestWorker()
	w2.Name = "nul\x00byte"
	if err := s.CreateWorker(ctx, w2); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("CreateWorker(NUL in name) = %v, want ErrInvalidArgument", err)
	}
}

// TestToolOutputIsSanitized: job messages come from tool output, which may hold
// NUL bytes or invalid UTF-8 that Postgres refuses; they must not wedge a job.
func TestToolOutputIsSanitized(t *testing.T) {
	s := newMigratedStore(t)
	ctx := t.Context()
	owner := seedUser(t, s)
	w := newTestWorker()
	if err := s.CreateWorker(ctx, w); err != nil {
		t.Fatal(err)
	}
	j := newTestJob(w.ID, owner)
	if err := s.CreateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	j.Status = domain.JobFailed
	j.Error = jobs.JobError{Code: "unspecified", Message: "bad \x00 byte \xff"}
	j.Warnings = []string{"w\x00"}
	got, err := s.UpdateJob(ctx, j)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(got.Error.Message, 0) || !slices.Equal(got.Warnings, []string{"w�"}) {
		t.Errorf("stored error %q, warnings %q", got.Error.Message, got.Warnings)
	}
}

// TestCreateJobRacesWithRemoveWorker: whatever the interleaving, a job is
// either rejected or committed before the removal, never created for a
// removed worker.
func TestCreateJobRacesWithRemoveWorker(t *testing.T) {
	s := newMigratedStore(t)
	ctx := t.Context()
	owner := seedUser(t, s)
	for range 20 {
		w := newTestWorker()
		if err := s.CreateWorker(ctx, w); err != nil {
			t.Fatal(err)
		}
		errc := make(chan error, 1)
		go func() { errc <- s.CreateJob(ctx, newTestJob(w.ID, owner)) }()
		if _, err := s.RemoveWorker(ctx, w.ID, testTime()); err != nil {
			t.Fatal(err)
		}
		cerr := <-errc
		if cerr != nil && !errors.Is(cerr, domain.ErrNotFound) {
			t.Fatalf("CreateJob: %v", cerr)
		}
		// Once the removal is done, creating must fail.
		if err := s.CreateJob(ctx, newTestJob(w.ID, owner)); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("CreateJob after removal = %v, want ErrNotFound", err)
		}
	}
}
