//go:build integration

package postgres

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Zeptiny/Syncr/internal/adapters/storetest"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/redact"
)

// postgresImage is the image of deploy/docker-compose.dev.yml.
const postgresImage = "postgres:17-alpine"

const (
	pgUser     = "syncr"
	pgPassword = "syncr-test"
	// templateDB holds the migrated schema; every test database is a copy.
	templateDB = "syncr_template"
)

// server is the one Postgres container shared by the package's tests, started
// by the first test that needs it and terminated by TestMain.
var server struct {
	once sync.Once
	err  error
	ctr  testcontainers.Container
	host string
	port string
	// createMu serializes CREATE DATABASE ... TEMPLATE, which needs the
	// template to be free of other sessions.
	createMu sync.Mutex
	nextDB   atomic.Int64
}

func TestMain(m *testing.M) {
	code := m.Run()
	if server.ctr != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		if err := server.ctr.Terminate(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "terminate postgres container: %v\n", err)
		}
		cancel()
	}
	os.Exit(code)
}

// dbURL returns the connection URL of database name on the shared server.
func dbURL(name string) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(pgUser, pgPassword),
		Host:     net.JoinHostPort(server.host, server.port),
		Path:     "/" + name,
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

func testConfig(name string) Config {
	cfg := DefaultConfig()
	cfg.URL = redact.Secret(dbURL(name))
	cfg.MaxConns = 20
	return cfg
}

// startServer starts the container and builds the migrated template database.
func startServer(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("skipping: testcontainers needs Linux containers, and Windows CI runners only run Windows containers")
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)
	server.once.Do(func() { server.err = initServer() })
	if server.err != nil {
		t.Fatalf("shared postgres: %v", server.err)
	}
}

func initServer() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        postgresImage,
			ExposedPorts: []string{"5432/tcp"},
			Env: map[string]string{
				"POSTGRES_USER":     pgUser,
				"POSTGRES_PASSWORD": pgPassword,
				"POSTGRES_DB":       "postgres",
			},
			// Memory-backed storage without durability: fast, and nothing is
			// left behind.
			Tmpfs: map[string]string{"/var/lib/postgresql/data": "rw"},
			Cmd: []string{
				"postgres", "-c", "fsync=off", "-c", "synchronous_commit=off",
				"-c", "full_page_writes=off", "-c", "max_connections=200",
			},
			// The entrypoint starts a temporary server to initialize the
			// cluster, then the real one: the message appears twice.
			WaitingFor: wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	server.ctr = ctr // set even on error: a created container must be terminated
	if err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	if server.host, err = ctr.Host(ctx); err != nil {
		return err
	}
	port, err := ctr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return err
	}
	server.port = port.Port()

	if err := execAdmin(ctx, "CREATE DATABASE "+templateDB); err != nil {
		return err
	}
	tmpl, err := Open(ctx, testConfig(templateDB))
	if err != nil {
		return fmt.Errorf("open template: %w", err)
	}
	defer tmpl.Close()
	if err := tmpl.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate template: %w", err)
	}
	return nil
}

// execAdmin runs one statement on the server's maintenance database.
func execAdmin(ctx context.Context, sql string) error {
	conn, err := pgx.Connect(ctx, dbURL("postgres"))
	if err != nil {
		return fmt.Errorf("connect as admin: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, sql); err != nil {
		return fmt.Errorf("%s: %w", sql, err)
	}
	return nil
}

// createDB creates a database for one test (a copy of the migrated template,
// or an empty one) and drops it when the test ends. It returns its name.
func createDB(t *testing.T, migrated bool) string {
	t.Helper()
	startServer(t)
	name := fmt.Sprintf("test_%d", server.nextDB.Add(1))
	stmt := "CREATE DATABASE " + name
	if migrated {
		stmt += " TEMPLATE " + templateDB
	}
	ctx := t.Context()
	server.createMu.Lock()
	err := execAdmin(ctx, stmt)
	server.createMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := execAdmin(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database: %v", err)
		}
	})
	return name
}

// openStore opens a store on database name; it is closed when the test ends.
func openStore(t *testing.T, name string) *Store {
	t.Helper()
	s, err := Open(t.Context(), testConfig(name))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// newMigratedStore returns a store on a fresh, fully migrated database.
func newMigratedStore(t *testing.T) *Store {
	t.Helper()
	return openStore(t, createDB(t, true))
}

func TestContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) storetest.Store { return newMigratedStore(t) })
}

// seedUser inserts a user (e.g. to own test jobs) and returns its ID.
func seedUser(t *testing.T, s *Store) domain.UserID {
	t.Helper()
	id := domain.NewUserID()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	u := users.User{
		ID: id, Username: "u" + id.String()[len(id.String())-12:], Role: domain.RoleUser, PasswordHash: "x",
		CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now, Version: 1,
	}
	if err := s.CreateUser(t.Context(), u); err != nil {
		t.Fatal(err)
	}
	return id
}
