package postgres

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLockID is the key of the session-level advisory lock that
// serializes migrators (an arbitrary constant; "SYNCR" in ASCII-ish hex).
const migrationLockID int64 = 0x53594e4352

// Migrate applies every pending migration. It is idempotent and safe to call
// from several controllers at once: each caller takes a Postgres advisory
// lock (blocking until it is free, or until ctx ends) before looking at the
// schema version, so the later ones find nothing left to do.
//
// Migrations only move forward at runtime; the down scripts exist for
// development and tests (see docs/adr/0004-postgres-store.md).
func (s *Store) Migrate(ctx context.Context) error {
	p, closeDB, err := s.migrationProvider()
	if err != nil {
		return err
	}
	defer closeDB()
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("postgres: migrate up: %w", err)
	}
	return nil
}

// migrationProvider returns a goose provider that runs on its own small set
// of connections, so a migration never waits behind (or starves) pool
// traffic, plus a function releasing them.
func (s *Store) migrationProvider() (*goose.Provider, func(), error) {
	files, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return nil, nil, fmt.Errorf("postgres: open embedded migrations: %w", err)
	}
	sqlDB := stdlib.OpenDB(*s.connConfig.Copy())
	// One connection holds the advisory lock while another runs the
	// migrations.
	sqlDB.SetMaxOpenConns(2)
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, files,
		goose.WithSessionLocker(advisoryLocker{id: migrationLockID}))
	if err != nil {
		_ = sqlDB.Close()
		return nil, nil, fmt.Errorf("postgres: create migration provider: %w", err)
	}
	return p, func() {
		_ = p.Close()
		_ = sqlDB.Close()
	}, nil
}

// advisoryLocker is a goose lock.SessionLocker on a blocking
// pg_advisory_lock. goose's own Postgres locker polls once per second at
// best; blocking in the server wakes the next migrator as soon as the lock is
// released, and an abandoned lock disappears with its session.
type advisoryLocker struct{ id int64 }

func (l advisoryLocker) SessionLock(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", l.id); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	return nil
}

func (l advisoryLocker) SessionUnlock(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", l.id); err != nil {
		return fmt.Errorf("release migration lock: %w", err)
	}
	return nil
}
