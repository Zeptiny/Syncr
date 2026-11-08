//go:build integration

package postgres

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
)

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
}

// milestone4Tables are the tables of migration 00003.
var milestone4Tables = []string{"contacts", "schedules", "schedule_remotes", "schedule_contacts", "schedule_runs", "deliveries"}

// allVersions are the versions of the embedded migrations.
var allVersions = []int64{1, 2, 3}

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
	ids := []string{"00000000-0000-7000-8000-000000000001", "00000000-0000-7000-8000-000000000002", "00000000-0000-7000-8000-000000000003"}
	for i, id := range ids {
		var removed *time.Time
		if i == 2 {
			now := time.Now()
			removed = &now
		}
		if _, err := s.pool.Exec(ctx, insertWorker, id, []string{"nas", "NAS", "nas"}[i], removed); err != nil {
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
	if names[ids[0]] != "nas" || names[ids[1]] != "NAS-00000000" || names[ids[2]] != "nas" {
		t.Errorf("worker names after up = %v, want the later live duplicate suffixed", names)
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
