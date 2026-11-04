package api

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/adapters/idgen"    //nolint:depguard // tests wire the services with the in-memory adapters
	"github.com/Zeptiny/Syncr/internal/adapters/memstore" //nolint:depguard // tests wire the services with the in-memory adapters
	"github.com/Zeptiny/Syncr/internal/adapters/secrets"  //nolint:depguard // tests wire the services with the in-memory adapters
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/clock"
	"github.com/Zeptiny/Syncr/internal/platform/redact"
)

const (
	// adminToken is the admin token of the test servers (a test value).
	adminToken = "test-admin-token-0123456789-abcdefghij"
	// specSecret is planted in test job specs; it must never be returned.
	specSecret = "SPEC-SECRET-VALUE-9f3a1c"
	// pairingSecret is a valid pairing secret (32+ printable characters).
	pairingSecret = "pairing-secret-0123456789-abcdefghij"
	testFP        = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testEpoch     = "epoch-1"
)

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
	broker *memstore.Broker
	clock  *clock.Fake
	wsvc   *workers.Service
	jsvc   *jobs.Service
	logs   *logBuffer
	worker domain.WorkerID // an active worker created by newEnv
}

// newEnv starts a server. mutate may adjust the Deps before the handler is
// built (e.g. to drop the authenticator).
func newEnv(t *testing.T, mutate ...func(*Deps)) *testEnv {
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
		broker: memstore.NewBroker(),
		clock:  clock.NewFake(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)),
		logs:   &logBuffer{},
	}
	ids := &idgen.Sequential{}
	e.jsvc, err = jobs.New(jobs.Deps{
		Jobs: e.store, Samples: e.store, Cipher: cipher, Clock: e.clock, IDs: ids,
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
	auth, err := NewAdminTokenAuth(redact.Secret(adminToken))
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{
		Logger:  newTestLogger(e.logs),
		Auth:    auth,
		Workers: e.wsvc,
		Jobs:    e.jsvc,
	}
	for _, m := range mutate {
		m(&deps)
	}
	e.srv = httptest.NewServer(NewHandler(deps))
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

// bearer adds an Authorization header to every request.
type bearer struct {
	base  http.RoundTripper
	value string
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	if b.value != "" {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", b.value)
	}
	return b.base.RoundTrip(r)
}

func (e *testEnv) httpClient(authorization string) *http.Client {
	return &http.Client{Transport: bearer{base: e.srv.Client().Transport, value: authorization}}
}

// workersAs and jobsAs return clients sending the given Authorization header
// value ("" sends none).
func (e *testEnv) workersAs(authorization string) syncrv1connect.WorkerAdminServiceClient {
	return syncrv1connect.NewWorkerAdminServiceClient(e.httpClient(authorization), e.srv.URL)
}

func (e *testEnv) jobsAs(authorization string) syncrv1connect.JobServiceClient {
	return syncrv1connect.NewJobServiceClient(e.httpClient(authorization), e.srv.URL)
}

func (e *testEnv) workersClient() syncrv1connect.WorkerAdminServiceClient {
	return e.workersAs("Bearer " + adminToken)
}

func (e *testEnv) jobsClient() syncrv1connect.JobServiceClient {
	return e.jobsAs("Bearer " + adminToken)
}

// testSpec is a valid rclone transfer spec carrying specSecret.
func testSpec() *syncrv1.JobSpec {
	return &syncrv1.JobSpec{
		Remotes: []*syncrv1.RemoteDefinition{{
			Alias: "s3", BackendType: "s3",
			Secrets: map[string]string{"secret_access_key": specSecret},
		}},
		Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{
			Mode:        syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY,
			Source:      &syncrv1.Location{Remote: "s3", Path: "bucket/prefix"},
			Destination: &syncrv1.Location{Path: "/data"},
		}},
	}
}

// runJob queues a job through the API.
func (e *testEnv) runJob() *syncrv1.Job {
	e.t.Helper()
	resp, err := e.jobsClient().RunJob(e.t.Context(), connect.NewRequest(&syncrv1.RunJobRequest{WorkerId: e.worker.String(), Spec: testSpec()}))
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

// noSpecLeak fails if the marshaled message contains the planted secret.
func noSpecLeak(t *testing.T, msg proto.Message) {
	t.Helper()
	b, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(specSecret)) {
		t.Errorf("%T leaks the job spec", msg)
	}
}

// newTestLogger returns a debug logger writing to w.
func newTestLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
