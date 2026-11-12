//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// jobsFilterAll lists every job.
func jobsFilterAll() jobs.ListFilter { return jobs.ListFilter{Limit: -1} }

// appliedVersions returns the goose bookkeeping rows of the store's database.
func appliedVersions(t *testing.T, s *Store) []int64 {
	t.Helper()
	rows, err := s.pool.Query(t.Context(),
		"SELECT version_id FROM goose_db_version WHERE is_applied AND version_id > 0 ORDER BY version_id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func tableExists(t *testing.T, s *Store, name string) bool {
	t.Helper()
	var ok bool
	err := s.pool.QueryRow(t.Context(), "SELECT to_regclass($1) IS NOT NULL", "public."+name).Scan(&ok)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

var schemaTables = []string{
	"workers", "enrollment_tokens", "jobs", "job_samples",
	"users", "sessions", "api_tokens", "remotes", "union_members", "job_remotes",
	"contacts", "schedules", "schedule_remotes", "schedule_contacts", "schedule_runs", "deliveries",
	"restic_repositories", "restic_snapshots",
}

// milestone4Tables are the tables of migration 00003.
var milestone4Tables = []string{"contacts", "schedules", "schedule_remotes", "schedule_contacts", "schedule_runs", "deliveries"}

// milestone5Tables are the tables of migration 00007.
var milestone5Tables = []string{"restic_repositories", "restic_snapshots"}

// allVersions are the versions of the embedded migrations.
var allVersions = []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

func columnExists(t *testing.T, s *Store, table, column string) bool {
	t.Helper()
	var ok bool
	err := s.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2)`, table, column).Scan(&ok)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestMigrateIdempotent(t *testing.T) {
	s := openStore(t, createDB(t, false))
	for _, table := range schemaTables {
		if tableExists(t, s, table) {
			t.Fatalf("table %s exists before Migrate", table)
		}
	}
	for i := range 3 {
		if err := s.Migrate(t.Context()); err != nil {
			t.Fatalf("Migrate #%d: %v", i+1, err)
		}
	}
	for _, table := range schemaTables {
		if !tableExists(t, s, table) {
			t.Errorf("table %s missing after Migrate", table)
		}
	}
	if got := appliedVersions(t, s); !slices.Equal(got, allVersions) {
		t.Errorf("applied versions = %v, want %v", got, allVersions)
	}
}

// TestMigrateConcurrent starts several "controllers" (separate stores, as in
// separate processes) plus several callers on one store, all migrating an
// empty database at once.
func TestMigrateConcurrent(t *testing.T) {
	name := createDB(t, false)
	stores := make([]*Store, 4)
	for i := range stores {
		stores[i] = openStore(t, name)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Go(func() { errs <- stores[i%len(stores)].Migrate(t.Context()) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Migrate: %v", err)
		}
	}
	if got := appliedVersions(t, stores[0]); !slices.Equal(got, allVersions) {
		t.Errorf("applied versions = %v, want %v (each migration applied once)", got, allVersions)
	}
}

// TestMigrateBlocksOnLock checks the advisory lock: while another session
// holds it, Migrate waits (and gives up with the context), and it proceeds
// once the lock is released.
func TestMigrateBlocksOnLock(t *testing.T) {
	s := openStore(t, createDB(t, false))
	ctx := t.Context()
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		t.Fatal(err)
	}

	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	err = s.Migrate(short)
	cancel()
	if err == nil {
		t.Fatal("Migrate succeeded while the lock was held")
	}
	if tableExists(t, s, "workers") {
		t.Fatal("Migrate changed the schema without holding the lock")
	}

	done := make(chan error, 1)
	go func() { done <- s.Migrate(ctx) }()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", migrationLockID); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	if err := <-done; err != nil {
		t.Fatalf("Migrate after unlock: %v", err)
	}
	if !tableExists(t, s, "workers") {
		t.Error("schema missing after Migrate")
	}
}

func TestMigrateDownUp(t *testing.T) {
	s := openStore(t, createDB(t, true))
	ctx := t.Context()
	p, closeDB, err := s.migrationProvider()
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()

	if _, err := p.DownTo(ctx, 0); err != nil {
		t.Fatalf("down: %v", err)
	}
	for _, table := range schemaTables {
		if tableExists(t, s, table) {
			t.Errorf("table %s survived the down migration", table)
		}
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
	for _, table := range schemaTables {
		if !tableExists(t, s, table) {
			t.Errorf("table %s missing after down+up", table)
		}
	}
}

// TestMigrateUsersRemotesDown checks the down migration of 00002: back at
// version 1 the Milestone 3 tables are gone and jobs has its Milestone 2
// columns again; migrating up restores version 2.
func TestMigrateUsersRemotesDown(t *testing.T) {
	s := openStore(t, createDB(t, true))
	ctx := t.Context()
	// A Milestone 3 job, which the down migration drops.
	w := newTestWorker()
	if err := s.CreateWorker(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateJob(ctx, newTestJob(w.ID, seedUser(t, s))); err != nil {
		t.Fatal(err)
	}
	p, closeDB, err := s.migrationProvider()
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()

	if _, err := p.DownTo(ctx, 1); err != nil {
		t.Fatalf("down to 1: %v", err)
	}
	for _, table := range []string{"users", "sessions", "api_tokens", "remotes", "union_members", "job_remotes"} {
		if tableExists(t, s, table) {
			t.Errorf("table %s survived the down migration", table)
		}
	}
	if !columnExists(t, s, "jobs", "spec_enc") || columnExists(t, s, "jobs", "owner_id") ||
		columnExists(t, s, "jobs", "definition") || columnExists(t, s, "jobs", "search_text") {
		t.Error("jobs does not have its version 1 columns after the down migration")
	}
	var jobs int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM jobs").Scan(&jobs); err != nil || jobs != 0 {
		t.Errorf("jobs after down = (%d, %v), want 0", jobs, err)
	}
	if got := appliedVersions(t, s); !slices.Equal(got, []int64{1}) {
		t.Errorf("applied versions = %v, want [1]", got)
	}

	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if got := appliedVersions(t, s); !slices.Equal(got, allVersions) {
		t.Errorf("applied versions = %v, want %v", got, allVersions)
	}
	if !columnExists(t, s, "jobs", "owner_id") || columnExists(t, s, "jobs", "spec_enc") {
		t.Error("jobs does not have its version 2 columns after up")
	}
}

// TestMigrateSchedulesNotificationsDown checks migration 00003: going up it
// suffixes duplicate live worker names and marks finished job history
// notified; going down to version 2 removes its tables and columns, and
// migrating up again restores version 3.
func TestMigrateSchedulesNotificationsDown(t *testing.T) {
	s := openStore(t, createDB(t, true))
	ctx := t.Context()
	p, closeDB, err := s.migrationProvider()
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()

	if _, err := p.DownTo(ctx, 2); err != nil {
		t.Fatalf("down to 2: %v", err)
	}
	for _, table := range milestone4Tables {
		if tableExists(t, s, table) {
			t.Errorf("table %s survived the down migration", table)
		}
	}
	if columnExists(t, s, "workers", "labels") || columnExists(t, s, "jobs", "schedule_id") || columnExists(t, s, "jobs", "notified_at") {
		t.Error("the 00003 columns survived the down migration")
	}
	if got := appliedVersions(t, s); !slices.Equal(got, []int64{1, 2}) {
		t.Errorf("applied versions = %v, want [1 2]", got)
	}

	// Version 2 data the up migration rewrites: two live workers named
	// alike (and a removed one), and a finished job.
	owner := seedUser(t, s)
	const insertWorker = `INSERT INTO workers (id, name, mode, secret_enc, info, created_at, removed_at)
		VALUES ($1, $2, 'active', '\x00', '{}', now(), $3)`
	// UUIDv7 IDs of the same millisecond (equal timestamp bits, so equal
	// first 12 hex digits), in creation order. Among them: three live
	// duplicates, a removed one, a live name equal to the first suffix the
	// duplicates would get, and duplicates of the maximum length.
	long := strings.Repeat("x", 64)
	workers := []struct {
		name, want string
		removed    bool
	}{
		{name: "nas", want: "nas"},
		{name: "NAS", want: "NAS-3"}, // "nas-2" is taken by a live worker
		{name: "nas", want: "nas", removed: true},
		{name: "Nas", want: "Nas-4"}, // "NAS-3" was taken by the rename above
		{name: "nas-2", want: "nas-2"},
		{name: long, want: long},
		{name: strings.ToUpper(long), want: strings.ToUpper(long[:62]) + "-2"},
	}
	ids := make([]string, len(workers))
	for i, w := range workers {
		ids[i] = fmt.Sprintf("0193a1b2-c3d4-7abc-8def-%012x", i+1)
		var removed *time.Time
		if w.removed {
			now := time.Now()
			removed = &now
		}
		if _, err := s.pool.Exec(ctx, insertWorker, ids[i], w.name, removed); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO jobs (id, worker_id, owner_id, kind, status, definition, created_at, queued_at, finished_at, exit_code, version)
		VALUES ('00000000-0000-7000-8000-000000000010', $1, $2, 'rclone_transfer', 'succeeded', '{}', now(), now(), now(), 0, 1),
		       ('00000000-0000-7000-8000-000000000011', $1, $2, 'rclone_transfer', 'queued', '{}', now(), now(), NULL, -1, 1)`,
		ids[0], owner.String()); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if got := appliedVersions(t, s); !slices.Equal(got, allVersions) {
		t.Errorf("applied versions = %v, want %v", got, allVersions)
	}
	for _, table := range milestone4Tables {
		if !tableExists(t, s, table) {
			t.Errorf("table %s missing after up", table)
		}
	}
	names := map[string]string{}
	rows, err := s.pool.Query(ctx, "SELECT id::text, name FROM workers ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatal(err)
		}
		names[id] = name
	}
	rows.Close()
	for i, w := range workers {
		if got := names[ids[i]]; got != w.want {
			t.Errorf("worker %d (%q) is named %q after up, want %q", i+1, w.name, got, w.want)
		}
	}
	var notified, pending int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE notified_at IS NOT NULL), count(*) FILTER (WHERE notified_at IS NULL) FROM jobs`).
		Scan(&notified, &pending); err != nil || notified != 1 || pending != 1 {
		t.Errorf("notified jobs = %d, unnotified = %d (%v); want the finished one marked", notified, pending, err)
	}
	// The unique index now holds.
	w := newTestWorker()
	w.Name = "Nas"
	wantErr := s.CreateWorker(ctx, w)
	if !errors.Is(wantErr, domain.ErrAlreadyExists) {
		t.Errorf("CreateWorker(taken name) = %v, want ErrAlreadyExists", wantErr)
	}
}

// TestMigrateResticDown checks migration 00007: going down to version 6
// removes the restic tables and columns, together with the restic jobs and
// schedules (and their deliveries), which cannot exist without them; other
// jobs survive. Migrating up again restores version 7.
func TestMigrateResticDown(t *testing.T) {
	s := openStore(t, createDB(t, true))
	ctx := t.Context()
	owner := seedUser(t, s)
	w := newTestWorker()
	if err := s.CreateWorker(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateJob(ctx, newTestJob(w.ID, owner)); err != nil {
		t.Fatal(err)
	}
	// Version 7 data the down migration drops: a repository with a cached
	// snapshot, a restic job with a delivery and a restic schedule.
	const (
		remote = "00000000-0000-7000-8000-000000000101"
		repo   = "00000000-0000-7000-8000-000000000102"
		job    = "00000000-0000-7000-8000-000000000103"
		sched  = "00000000-0000-7000-8000-000000000104"
		cont   = "00000000-0000-7000-8000-000000000105"
		deliv  = "00000000-0000-7000-8000-000000000106"
	)
	hex64 := strings.Repeat("ab", 32)
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO remotes (id, owner_id, name, backend_type, options, created_at, updated_at, version)
			VALUES ($1, $2, 'B2', 'b2', '{}', now(), now(), 1)`, []any{remote, owner.String()}},
		{`INSERT INTO restic_repositories (id, owner_id, name, remote_id, path, password_enc, restic_id, created_at, updated_at)
			VALUES ($1, $2, 'Photos', $3, 'restic', '\x00', $4, now(), now())`, []any{repo, owner.String(), remote, hex64}},
		{`INSERT INTO restic_snapshots (repository_id, snapshot_id, taken_at) VALUES ($1, $2, now())`, []any{repo, hex64}},
		{`INSERT INTO jobs (id, worker_id, owner_id, kind, status, definition, created_at, queued_at, exit_code, version, repository_id, concurrency_keys)
			VALUES ($1, $2, $3, 'restic_backup', 'queued', '{}', now(), now(), -1, 1, $4, $5)`,
			[]any{job, w.ID.String(), owner.String(), repo, []string{"restic-repo:" + repo}}},
		{`INSERT INTO schedules (id, owner_id, name, cron, timezone, kind, definition, worker_id, missed_action, overlap,
			enabled, created_at, updated_at, version, repository_id)
			VALUES ($1, $2, 'Nightly', '0 2 * * *', 'UTC', 'restic_backup', '{}', $3, 'run_late', 'skip', false, now(), now(), 1, $4)`,
			[]any{sched, owner.String(), w.ID.String(), repo}},
		{`INSERT INTO contacts (id, owner_id, name, channel_type, created_at, updated_at) VALUES ($1, $2, 'Me', 'webhook', now(), now())`,
			[]any{cont, owner.String()}},
		{`INSERT INTO deliveries (id, contact_id, owner_id, event, dedupe_key, job_id, payload, status, max_attempts, next_attempt_at, created_at)
			VALUES ($1, $2, $3, 'succeeded', $4, $5, '{}', 'pending', 5, now(), now())`,
			[]any{deliv, cont, owner.String(), "job:" + job + ":succeeded", job}},
	} {
		if _, err := s.pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("%s: %v", stmt.sql, err)
		}
	}
	p, closeDB, err := s.migrationProvider()
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()

	if _, err := p.DownTo(ctx, 6); err != nil {
		t.Fatalf("down to 6: %v", err)
	}
	for _, table := range milestone5Tables {
		if tableExists(t, s, table) {
			t.Errorf("table %s survived the down migration", table)
		}
	}
	if columnExists(t, s, "jobs", "repository_id") || columnExists(t, s, "jobs", "concurrency_keys") || columnExists(t, s, "schedules", "repository_id") {
		t.Error("the 00007 columns survived the down migration")
	}
	var jobCount, scheduleCount, deliveryCount int
	if err := s.pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM jobs), (SELECT count(*) FROM schedules), (SELECT count(*) FROM deliveries)").
		Scan(&jobCount, &scheduleCount, &deliveryCount); err != nil || jobCount != 1 || scheduleCount != 0 || deliveryCount != 0 {
		t.Errorf("after down: %d jobs, %d schedules, %d deliveries (%v); want the transfer job only", jobCount, scheduleCount, deliveryCount, err)
	}
	if got := appliedVersions(t, s); !slices.Equal(got, []int64{1, 2, 3, 4, 5, 6}) {
		t.Errorf("applied versions = %v, want [1 2 3 4 5 6]", got)
	}

	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if got := appliedVersions(t, s); !slices.Equal(got, allVersions) {
		t.Errorf("applied versions = %v, want %v", got, allVersions)
	}
	for _, table := range milestone5Tables {
		if !tableExists(t, s, table) {
			t.Errorf("table %s missing after up", table)
		}
	}
	if !columnExists(t, s, "jobs", "concurrency_keys") || !columnExists(t, s, "schedules", "repository_id") {
		t.Error("the 00007 columns are missing after up")
	}
	// The surviving job reads back without a repository or keys.
	js, err := s.ListJobs(ctx, jobsFilterAll())
	if err != nil || len(js) != 1 || !js[0].RepositoryID.IsZero() || js[0].ConcurrencyKeys != nil {
		t.Errorf("jobs after up = %+v (%v)", js, err)
	}
}

// TestMigrateWorkerInfoEpoch checks migration 00008: the info stored before
// it is the Hello of the worker's current epoch (a running worker does not
// send it again), so it is tagged with that epoch; a worker that never
// connected keeps its enrollment info untagged. Down removes the tags.
func TestMigrateWorkerInfoEpoch(t *testing.T) {
	s := openStore(t, createDB(t, true))
	ctx := t.Context()
	p, closeDB, err := s.migrationProvider()
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	if _, err := p.DownTo(ctx, 7); err != nil {
		t.Fatalf("down to 7: %v", err)
	}
	const (
		running = "00000000-0000-7000-8000-000000000201"
		never   = "00000000-0000-7000-8000-000000000202"
	)
	for _, w := range []struct{ id, name, epoch, info string }{
		{running, "nas", "e1", `{"version":"v2.0.0","os":"linux","features":["probe","restic.v2"]}`},
		{never, "pc", "", `{"os":"windows","arch":"amd64"}`},
	} {
		if _, err := s.pool.Exec(ctx, `INSERT INTO workers (id, name, mode, secret_enc, info, epoch, created_at)
			VALUES ($1, $2, 'active', '\x00', $3, $4, now())`, w.id, w.name, w.info, w.epoch); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	got, err := s.GetWorker(ctx, mustWorkerID(t, running))
	if err != nil || got.Info.Epoch != "e1" || !got.InfoCurrent() || got.Info.Version != "v2.0.0" || !got.Info.HasFeatures("restic.v2") {
		t.Errorf("connected worker after up = %+v (%v), want its info tagged with e1", got.Info, err)
	}
	got, err = s.GetWorker(ctx, mustWorkerID(t, never))
	if err != nil || got.Info.Epoch != "" || got.InfoCurrent() || got.Info.OS != "windows" {
		t.Errorf("never connected worker after up = %+v (%v)", got.Info, err)
	}
	if _, err := p.DownTo(ctx, 7); err != nil {
		t.Fatalf("down to 7 again: %v", err)
	}
	var tagged int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM workers WHERE info ? 'epoch'`).Scan(&tagged); err != nil || tagged != 0 {
		t.Errorf("tagged infos after down = %d (%v), want 0", tagged, err)
	}
}

// TestMigrateRelease checks migration 00009: it adds the partial index on
// deliveries.schedule_id that deleting a schedule uses, down removes it and
// up restores it.
func TestMigrateRelease(t *testing.T) {
	s := openStore(t, createDB(t, true))
	ctx := t.Context()
	indexDef := func() string {
		t.Helper()
		var def string
		err := s.pool.QueryRow(ctx, "SELECT coalesce((SELECT indexdef FROM pg_indexes WHERE indexname = 'deliveries_schedule_idx'), '')").Scan(&def)
		if err != nil {
			t.Fatal(err)
		}
		return def
	}
	if def := indexDef(); !strings.Contains(def, "(schedule_id)") || !strings.Contains(def, "WHERE (schedule_id IS NOT NULL)") {
		t.Errorf("deliveries_schedule_idx = %q, want a partial index on schedule_id", def)
	}
	p, closeDB, err := s.migrationProvider()
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	if _, err := p.DownTo(ctx, 8); err != nil {
		t.Fatalf("down to 8: %v", err)
	}
	if def := indexDef(); def != "" {
		t.Errorf("deliveries_schedule_idx survived the down migration: %q", def)
	}
	if got := appliedVersions(t, s); !slices.Equal(got, allVersions[:8]) {
		t.Errorf("applied versions = %v, want %v", got, allVersions[:8])
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if indexDef() == "" {
		t.Error("deliveries_schedule_idx is missing after up")
	}
	if got := appliedVersions(t, s); !slices.Equal(got, allVersions) {
		t.Errorf("applied versions = %v, want %v", got, allVersions)
	}
}

// TestMigrateJobHooks checks migration 00010: it adds jobs.hook_runs, down
// removes it and up restores it; a job that was there before reads back
// without hook runs.
func TestMigrateJobHooks(t *testing.T) {
	s := openStore(t, createDB(t, true))
	ctx := t.Context()
	w := newTestWorker()
	if err := s.CreateWorker(ctx, w); err != nil {
		t.Fatal(err)
	}
	j := newTestJob(w.ID, seedUser(t, s))
	j.HookRuns = []jobs.HookRun{{Name: "stop-app", Phase: domain.HookBefore, Status: domain.HookSucceeded, Output: "stopped"}}
	if err := s.CreateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetJob(ctx, j.ID); err != nil || len(got.HookRuns) != 1 || got.HookRuns[0] != j.HookRuns[0] {
		t.Fatalf("GetJob = %+v, %v; want the hook run", got.HookRuns, err)
	}
	p, closeDB, err := s.migrationProvider()
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	if _, err := p.DownTo(ctx, 9); err != nil {
		t.Fatalf("down to 9: %v", err)
	}
	if columnExists(t, s, "jobs", "hook_runs") {
		t.Error("jobs.hook_runs survived the down migration")
	}
	if got := appliedVersions(t, s); !slices.Equal(got, allVersions[:9]) {
		t.Errorf("applied versions = %v, want %v", got, allVersions[:9])
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if got := appliedVersions(t, s); !slices.Equal(got, allVersions) {
		t.Errorf("applied versions = %v, want %v", got, allVersions)
	}
	// The job is one from before the column existed now.
	var runs string
	if err := s.pool.QueryRow(ctx, `SELECT hook_runs::text FROM jobs WHERE id = $1`, j.ID.String()).Scan(&runs); err != nil || runs != "[]" {
		t.Errorf("hook_runs of a job from before the migration = %q (%v), want []", runs, err)
	}
	if got, err := s.GetJob(ctx, j.ID); err != nil || got.HookRuns != nil {
		t.Errorf("GetJob of a job from before the migration: hook runs %+v, %v", got.HookRuns, err)
	}
}

func mustWorkerID(t *testing.T, s string) domain.WorkerID {
	t.Helper()
	id, err := domain.ParseWorkerID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
