//go:build integration

package postgres

import (
	"context"
	"sync"
	"testing"
	"time"
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

var schemaTables = []string{"workers", "enrollment_tokens", "jobs", "job_samples"}

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
	if got := appliedVersions(t, s); len(got) != 1 || got[0] != 1 {
		t.Errorf("applied versions = %v, want [1]", got)
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
	if got := appliedVersions(t, stores[0]); len(got) != 1 || got[0] != 1 {
		t.Errorf("applied versions = %v, want [1] (each migration applied once)", got)
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
