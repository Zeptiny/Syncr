// Package postgres is the PostgreSQL implementation of the controller's
// repository ports (workers.Repository, workers.TokenRepository,
// jobs.Repository, jobs.SampleRepository, jobs.ActivityRepository,
// users.Repository, users.SessionRepository, users.APITokenRepository,
// remotes.Repository, schedules.Repository, contacts.Repository,
// notifications.Repository), built on pgx/v5, sqlc-generated queries
// (package db) and goose migrations.
//
// Concurrency safety lives in the statements, not in application locks: every
// conditional update is a single UPDATE ... WHERE <condition> RETURNING ...
// statement, so competing controllers and goroutines are serialized by the
// row lock Postgres takes (see docs/adr/0004-postgres-store.md). The store
// holds no state besides the connection pool and is safe for concurrent use.
//
// Allowed dependencies: the standard library, pgx, goose, internal/domain,
// the internal/app packages whose ports it implements and
// internal/platform/redact.
package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Zeptiny/Syncr/internal/adapters/postgres/db"
	"github.com/Zeptiny/Syncr/internal/app/contacts"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/notifications"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/app/workers"
)

// Compile-time checks that Store implements the ports.
var (
	_ workers.Repository       = (*Store)(nil)
	_ workers.TokenRepository  = (*Store)(nil)
	_ jobs.Repository          = (*Store)(nil)
	_ jobs.SampleRepository    = (*Store)(nil)
	_ jobs.ActivityRepository  = (*Store)(nil)
	_ users.Repository         = (*Store)(nil)
	_ users.SessionRepository  = (*Store)(nil)
	_ users.APITokenRepository = (*Store)(nil)
	_ remotes.Repository       = (*Store)(nil)
	_ schedules.Repository     = (*Store)(nil)
	_ contacts.Repository      = (*Store)(nil)
	_ notifications.Repository = (*Store)(nil)
)

// applicationName identifies the controller in pg_stat_activity.
const applicationName = "syncr-controller"

// Store is a Postgres-backed store. Create it with Open and release it with
// Close.
type Store struct {
	pool *pgxpool.Pool
	q    *db.Queries
	// connConfig is the connection settings of the pool; Migrate uses them
	// to open its own dedicated connections instead of competing with
	// request traffic for pool slots.
	connConfig *pgx.ConnConfig
}

// Open connects to the database described by cfg and verifies it with a
// ping. It does not change the schema: call Migrate for that.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	pc, err := pgxpool.ParseConfig(cfg.URL.Reveal())
	if err != nil {
		// pgx redacts the password in this error.
		return nil, fmt.Errorf("postgres: invalid database url: %w", err)
	}
	pc.MaxConns = cfg.MaxConns
	pc.MinConns = cfg.MinConns
	pc.MaxConnLifetime = cfg.MaxConnLifetime
	pc.MaxConnIdleTime = cfg.MaxConnIdleTime
	pc.HealthCheckPeriod = cfg.HealthCheckPeriod
	pc.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	if _, ok := pc.ConnConfig.RuntimeParams["application_name"]; !ok {
		pc.ConnConfig.RuntimeParams["application_name"] = applicationName
	}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}
	s := &Store{pool: pool, q: db.New(pool), connConfig: pc.ConnConfig}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := s.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Ping verifies that the database answers.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres: ping: %w", err)
	}
	return nil
}

// Close closes the pool, waiting for checked-out connections to be returned.
// The store must not be used afterwards.
func (s *Store) Close() {
	s.pool.Close()
}
