package api

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/adapters/broker"
	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/adapters/memstore"
	"github.com/Zeptiny/Syncr/internal/adapters/secrets"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/platform/clock"
	"github.com/Zeptiny/Syncr/internal/platform/redact"
)

const (
	// adminToken is the provisional admin token of AdminTokenAuth tests (a
	// test value).
	adminToken = "test-admin-token-0123456789-abcdefghij"
	// pairingSecret is a valid pairing secret (32+ printable characters).
	pairingSecret = "pairing-secret-0123456789-abcdefghij"
	testFP        = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testEpoch     = "epoch-1"

	// testActorHeader is read by testAuth.
	testActorHeader = "X-Test-Actor"
)

// The test users (they need not exist in the store: testAuth trusts the
// header).
var (
	testAdmin = domain.Actor{UserID: mustUserID("00000000-0000-7000-8000-00000000a001"), Role: domain.RoleAdmin}
	testUser  = domain.Actor{UserID: mustUserID("00000000-0000-7000-8000-00000000a002"), Role: domain.RoleUser}
)

func mustUserID(s string) domain.UserID {
	id, err := domain.ParseUserID(s)
	if err != nil {
		panic(err)
	}
	return id
}

// testAuth is the Authenticator of the test servers: it trusts the
// X-Test-Actor header, "<role> <user id> [session|token]" (session by
// default; see actorHeader). It stands in for the session authenticator, so
// the API tests do not depend on the real authentication.
type testAuth struct{}

func (testAuth) Authenticate(ctx context.Context, h http.Header, _ connect.Spec) (context.Context, error) {
	fields := strings.Fields(h.Get(testActorHeader))
	if len(fields) < 2 || len(fields) > 3 {
		return ctx, fmt.Errorf("%w: no valid %s header", domain.ErrUnauthenticated, testActorHeader)
	}
	role, err := domain.ParseRole(fields[0])
	if err != nil {
		return ctx, fmt.Errorf("%w: %w", domain.ErrUnauthenticated, err)
	}
	id, err := domain.ParseUserID(fields[1])
	if err != nil {
		return ctx, fmt.Errorf("%w: %w", domain.ErrUnauthenticated, err)
	}
	method := AuthSession
	if len(fields) == 3 {
		switch fields[2] {
		case "session":
		case "token":
			method = AuthToken
		default:
			return ctx, fmt.Errorf("%w: unknown method %q", domain.ErrUnauthenticated, fields[2])
		}
	}
	return WithCaller(ctx, Caller{Actor: domain.Actor{UserID: id, Role: role}, Method: method}), nil
}

// actorHeader is the X-Test-Actor value of a, authenticating with method
// ("session" or "token").
func actorHeader(a domain.Actor, method string) string {
	return string(a.Role) + " " + a.UserID.String() + " " + method
}

// testKind is the "rclone_transfer" job kind of the test servers: it
// accepts a transfer with a source and a destination path.
//
// m3: pending track C: the real kinds (jobkind.Builtin) arrive with track C.
type testKind struct{}

func (testKind) Name() string { return "rclone_transfer" }

func (testKind) Check(def *syncrv1.JobDefinition) (jobkind.Refs, error) {
	tr := def.GetRcloneTransfer()
	var v domain.Validation
	if tr.GetSource().GetPath() == "" {
		v.Add("definition.rclone_transfer.source.path", "is required")
	}
	if tr.GetDestination().GetPath() == "" {
		v.Add("definition.rclone_transfer.destination.path", "is required")
	}
	return jobkind.Refs{}, v.Err()
}

func (testKind) Build(*syncrv1.JobDefinition, map[domain.RemoteID]jobkind.Remote) (*syncrv1.JobSpec, error) {
	return nil, errors.New("testKind does not build specs")
}

// logBuffer is a goroutine-safe log sink.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testEnv is an API server on real services backed by memstore.
type testEnv struct {
	t      *testing.T
	srv    *httptest.Server
	store  *memstore.Store
	broker *broker.Broker
	clock  *clock.Fake
	wsvc   *workers.Service
	jsvc   *jobs.Service
	logs   *logBuffer
	worker domain.WorkerID // an active worker created by newEnv
}

// newEnv starts a server authenticating with testAuth. mutate may adjust
// the Deps before the handler is built (e.g. withAdminToken, or to drop a
// service).
func newEnv(t *testing.T, mutate ...func(*Deps)) *testEnv {
	t.Helper()
	return newEnvServer(t, nil, mutate...)
}

// withAdminToken makes the server authenticate with the provisional
// AdminTokenAuth (token adminToken) instead of testAuth.
func withAdminToken(t *testing.T) func(*Deps) {
	t.Helper()
	auth, err := NewAdminTokenAuth(redact.Secret(adminToken))
	if err != nil {
		t.Fatal(err)
	}
	return func(d *Deps) { d.Auth = auth }
}

// newEnvServer is newEnv with a hook that configures the http.Server before
// it starts (e.g. its timeouts).
func newEnvServer(t *testing.T, configure func(*http.Server), mutate ...func(*Deps)) *testEnv {
	t.Helper()
	key := secrets.Key{ID: 1, Bytes: make([]byte, secrets.KeySize)}
	if _, err := crand.Read(key.Bytes); err != nil {
		t.Fatal(err)
	}
	cipher, err := secrets.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	e := &testEnv{
		t:      t,
		store:  memstore.New(),
		broker: broker.New(),
		clock:  clock.NewFake(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)),
		logs:   &logBuffer{},
	}
	ids := &idgen.Sequential{}
	e.jsvc, err = jobs.New(jobs.Deps{
		Jobs: e.store, Samples: e.store, Clock: e.clock, IDs: ids,
		Broker: e.broker, Notifier: jobs.NopNotifier{}, Waker: nopWaker{},
	}, jobs.Config{})
	if err != nil {
		t.Fatal(err)
	}
	e.wsvc, err = workers.New(workers.Deps{
		Workers: e.store, Tokens: e.store, Cipher: cipher, Clock: e.clock, IDs: ids,
		Jobs: e.jsvc, Random: crand.Reader,
	}, workers.Config{})
	if err != nil {
		t.Fatal(err)
	}
	kinds, err := jobkind.NewRegistry(testKind{})
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{
		Logger:  newTestLogger(e.logs),
		Auth:    testAuth{},
		Workers: e.wsvc,
		Jobs:    e.jsvc,
		Kinds:   kinds,
	}
	for _, m := range mutate {
		m(&deps)
	}
	e.srv = httptest.NewUnstartedServer(NewHandler(deps))
	if configure != nil {
		configure(e.srv.Config)
	}
	e.srv.Start()
	t.Cleanup(e.srv.Close)
	e.worker = e.addWorker()
	return e
}

type nopWaker struct{}

func (nopWaker) Wake(domain.WorkerID) {}

// addWorker stores an active worker directly.
func (e *testEnv) addWorker() domain.WorkerID {
	e.t.Helper()
	id := domain.NewWorkerID()
	w := workers.Worker{ID: id, Name: "worker", Mode: domain.WorkerActive, CreatedAt: e.clock.Now()}
	if err := e.store.CreateWorker(context.Background(), w); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// headerTransport adds a request header to every request.
type headerTransport struct {
	base       http.RoundTripper
	key, value string
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if h.value != "" {
		r = r.Clone(r.Context())
		r.Header.Set(h.key, h.value)
	}
	return h.base.RoundTrip(r)
}

// clientWith returns an HTTP client sending header key: value on every
// request ("" sends none).
func (e *testEnv) clientWith(key, value string) *http.Client {
	return &http.Client{Transport: headerTransport{base: e.srv.Client().Transport, key: key, value: value}}
}

// workersAs and jobsAs return clients sending the given Authorization header
// value ("" sends none), for the authenticator tests.
func (e *testEnv) workersAs(authorization string) syncrv1connect.WorkerAdminServiceClient {
	return syncrv1connect.NewWorkerAdminServiceClient(e.clientWith("Authorization", authorization), e.srv.URL)
}

func (e *testEnv) jobsAs(authorization string) syncrv1connect.JobServiceClient {
	return syncrv1connect.NewJobServiceClient(e.clientWith("Authorization", authorization), e.srv.URL)
}

// workersFor and jobsFor return clients acting as a (testAuth), with a
// session.
func (e *testEnv) workersFor(a domain.Actor) syncrv1connect.WorkerAdminServiceClient {
	return syncrv1connect.NewWorkerAdminServiceClient(e.clientWith(testActorHeader, actorHeader(a, "session")), e.srv.URL)
}

func (e *testEnv) jobsFor(a domain.Actor) syncrv1connect.JobServiceClient {
	return syncrv1connect.NewJobServiceClient(e.clientWith(testActorHeader, actorHeader(a, "session")), e.srv.URL)
}

// workersClient and jobsClient act as the test admin.
func (e *testEnv) workersClient() syncrv1connect.WorkerAdminServiceClient {
	return e.workersFor(testAdmin)
}

func (e *testEnv) jobsClient() syncrv1connect.JobServiceClient {
	return e.jobsFor(testAdmin)
}

// testDefinition is a valid rclone transfer definition.
func testDefinition() *syncrv1.JobDefinition {
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransferDefinition{
		Mode:        syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY,
		Source:      &syncrv1.JobLocation{RemoteId: "00000000-0000-7000-8000-00000000b001", Path: "bucket/prefix"},
		Destination: &syncrv1.JobLocation{Path: "/data"},
	}}}
}

// runJob queues a job through the API, as the test admin.
func (e *testEnv) runJob() *syncrv1.Job {
	e.t.Helper()
	resp, err := e.jobsClient().RunJob(e.t.Context(), connect.NewRequest(&syncrv1.RunJobRequest{WorkerId: e.worker.String(), Definition: testDefinition()}))
	if err != nil {
		e.t.Fatal(err)
	}
	return resp.Msg.GetJob()
}

func (e *testEnv) jobID(j *syncrv1.Job) domain.JobID {
	e.t.Helper()
	id, err := domain.ParseJobID(j.GetId())
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// codeOf returns err's Connect code, or fails the test if it is not a
// Connect error.
func codeOf(t *testing.T, err error) connect.Code {
	t.Helper()
	if err == nil {
		return 0
	}
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("error %v is not a Connect error", err)
	}
	return cerr.Code()
}

func wantCode(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("got no error, want %v", want)
	}
	if got := codeOf(t, err); got != want {
		t.Fatalf("code = %v (%v), want %v", got, err, want)
	}
}

// newTestLogger returns a debug logger writing to w.
func newTestLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
