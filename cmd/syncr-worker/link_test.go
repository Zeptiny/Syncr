package main

import (
	"bytes"
	"context"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch/passive"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/service"
	"github.com/Zeptiny/Syncr/internal/worker/setup"
	"github.com/Zeptiny/Syncr/internal/worker/tlscert"
)

// syncBuffer is a goroutine-safe writer for logs read while the worker runs.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startRun runs `syncr-worker run` in-process until stop is called, which
// waits for it and returns its exit code.
func startRun(t *testing.T, e *env, args ...string) (stop func() int) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)
	go func() { done <- run(ctx, append([]string{"run"}, args...), e) }()
	var once sync.Once
	code := -1
	stop = func() int {
		once.Do(func() {
			cancel()
			select {
			case code = <-done:
			case <-time.After(30 * time.Second):
				t.Error("worker did not stop")
			}
		})
		return code
	}
	t.Cleanup(func() { stop() })
	return stop
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fakeController is a ControllerLinkService that acknowledges every event
// and records the Hello and the credentials it saw.
type fakeController struct {
	mu      sync.Mutex
	hello   *syncrv1.Hello
	auth    []string
	reports int
}

func (c *fakeController) Report(_ context.Context, req *connect.Request[syncrv1.ReportRequest]) (*connect.Response[syncrv1.ReportResponse], error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reports++
	c.auth = append(c.auth, req.Header().Get("Authorization"))
	var last int64
	for _, ev := range req.Msg.GetEvents() {
		if h := ev.GetHello(); h != nil {
			c.hello = h
		}
		last = ev.GetSeq()
	}
	return connect.NewResponse(&syncrv1.ReportResponse{AckSeq: last}), nil
}

func (c *fakeController) Poll(ctx context.Context, _ *connect.Request[syncrv1.PollRequest]) (*connect.Response[syncrv1.PollResponse], error) {
	select {
	case <-ctx.Done():
	case <-time.After(200 * time.Millisecond):
	}
	return connect.NewResponse(&syncrv1.PollResponse{}), nil
}

func (c *fakeController) seen() (*syncrv1.Hello, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hello, append([]string(nil), c.auth...)
}

// TestRunActiveLink runs the worker in active mode against a TLS controller
// trusted through link.controller_ca_file: the Hello carries the worker's
// identity, every call carries its credentials, and a stop drains.
func TestRunActiveLink(t *testing.T) {
	t.Parallel()
	fc := &fakeController{}
	mux := http.NewServeMux()
	mux.Handle(syncrv1connect.NewControllerLinkServiceHandler(fc))
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}

	f := newFixture(t)
	logs := &syncBuffer{}
	e, _ := f.runEnv(map[string]string{
		"SYNCR_LINK_MODE":           "active",
		"SYNCR_CONTROLLER_URL":      srv.URL,
		"SYNCR_CONTROLLER_CA_FILE":  caFile,
		"SYNCR_WORKER_ID":           cmdWorkerID,
		"SYNCR_WORKER_SECRET":       cmdSecret,
		"SYNCR_MAX_CONCURRENT_JOBS": "3",
	})
	e.stderr = logs
	stop := startRun(t, e)

	waitFor(t, "the Hello", func() bool { h, _ := fc.seen(); return h != nil })
	hello, auth := fc.seen()
	host, _ := os.Hostname()
	if hello.GetWorkerVersion() != "v2.0.0-test" || hello.GetHostname() != host || hello.GetMaxConcurrentJobs() != 3 {
		t.Errorf("hello = %v", hello)
	}
	if got := hello.GetLocalRoots(); len(got) != 1 || got[0] != f.cfg.LocalRoots[0] {
		t.Errorf("hello local roots = %v, want %v", got, f.cfg.LocalRoots)
	}
	tools := map[string]string{}
	for _, tv := range hello.GetTools() {
		tools[tv.GetName()] = tv.GetVersion()
	}
	if tools[toolRclone] == "" || tools[toolRestic] == "" {
		t.Errorf("hello tools = %v", hello.GetTools())
	}
	if !strings.Contains(strings.Join(hello.GetJobKinds(), ","), "rclone_transfer") {
		t.Errorf("hello job kinds = %v", hello.GetJobKinds())
	}
	// An up-to-date worker reports every link feature and probe operation.
	if !slices.Equal(hello.GetFeatures(), link.Features()) || !slices.Equal(hello.GetProbeOps(), link.ProbeOps()) {
		t.Errorf("hello features = %v, probe ops = %v", hello.GetFeatures(), hello.GetProbeOps())
	}
	want := link.ActiveAuthorization(cmdWorkerID, cmdSecret)
	for _, a := range auth {
		if a != want {
			t.Fatalf("Authorization = %q", a)
		}
	}

	if code := stop(); code != exitOK {
		t.Fatalf("exit %d\n%s", code, logs)
	}
	for _, step := range []string{"shutdown: active link loops stopped", "shutdown: jobs stopped", "shutdown: every event delivered to the controller", "worker stopped"} {
		if !strings.Contains(logs.String(), step) {
			t.Errorf("log lacks %q:\n%s", step, logs)
		}
	}
	if strings.Contains(logs.String(), cmdSecret) {
		t.Error("worker secret in the log")
	}
}

// passiveEnv configures a passive worker serving a new self-signed
// certificate on addr.
func passiveEnv(t *testing.T, f *fixture, addr, secret string) (*env, *syncBuffer, tlscert.Pair) {
	t.Helper()
	pair, err := tlscert.Generate(tlscert.Options{Hostname: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "worker.crt"), filepath.Join(dir, "worker.key")
	if _, err := tlscert.Write(certFile, keyFile, pair, true); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	e, _ := f.runEnv(map[string]string{
		"SYNCR_LINK_MODE":      "passive",
		"SYNCR_LISTEN_ADDR":    addr,
		"SYNCR_ADVERTISE_URL":  "https://" + addr,
		"SYNCR_TLS_CERT_FILE":  certFile,
		"SYNCR_TLS_KEY_FILE":   keyFile,
		"SYNCR_PAIRING_SECRET": secret,
	})
	e.stderr = logs
	return e, logs, pair
}

// TestRunPassiveLink runs the worker in passive mode and talks to it with the
// controller's own pinned client: the first Pull returns the Hello, a Pull
// acknowledging it leaves nothing pending, and a stop is clean.
func TestRunPassiveLink(t *testing.T) {
	t.Parallel()
	const secret = "pairing-secret-0123456789-abcdefghij"
	f := newFixture(t)
	addr := freeAddr(t)
	e, logs, pair := passiveEnv(t, f, addr, secret)
	stop := startRun(t, e)

	client, err := passive.NewClient(workers.PassiveTarget{URL: "https://" + addr, CertFingerprint: pair.Fingerprint, Secret: secret}, passive.ClientConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var resp *syncrv1.PullResponse
	waitFor(t, "the passive listener", func() bool {
		resp, err = client.Pull(t.Context(), &syncrv1.PullRequest{})
		return err == nil
	})
	evs := resp.GetEvents()
	if len(evs) == 0 || evs[0].GetHello().GetWorkerVersion() != "v2.0.0-test" {
		t.Fatalf("first pull = %v", resp)
	}
	last := evs[len(evs)-1].GetSeq()
	resp, err = client.Pull(t.Context(), &syncrv1.PullRequest{Epoch: resp.GetEpoch(), AfterSeq: last})
	if err != nil || len(resp.GetEvents()) != 0 {
		t.Fatalf("second pull = %v, %v", resp, err)
	}

	// A wrong pin is refused by the controller's client.
	other, err := passive.NewClient(workers.PassiveTarget{URL: "https://" + addr, CertFingerprint: strings.Repeat("ab", 32), Secret: secret}, passive.ClientConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Pull(t.Context(), &syncrv1.PullRequest{}); err == nil {
		t.Error("a client with the wrong pin was answered")
	}

	if code := stop(); code != exitOK {
		t.Fatalf("exit %d\n%s", code, logs)
	}
	for _, step := range []string{"passive link listening", "shutdown: jobs stopped", "shutdown: every event collected by the controller", "worker stopped"} {
		if !strings.Contains(logs.String(), step) {
			t.Errorf("log lacks %q:\n%s", step, logs)
		}
	}
	if strings.Contains(logs.String(), secret) {
		t.Error("pairing secret in the log")
	}
}

func TestRunPassiveLinkPortInUseFailsStartup(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	f := newFixture(t)
	e, logs, _ := passiveEnv(t, f, l.Addr().String(), "pairing-secret-0123456789-abcdefghij")
	if code := run(t.Context(), []string{"run"}, e); code != exitError {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(logs.String(), "listen on "+l.Addr().String()) {
		t.Errorf("stderr = %s", logs)
	}
}

// runningServices is a service manager whose Start runs the worker
// in-process, as the real service would run `syncr-worker run --config`.
type runningServices struct {
	cmdServices
	start func() func() int
	stop  func() int
}

func (s *runningServices) Start(ctx context.Context, sp service.Spec) error {
	s.stop = s.start()
	return s.cmdServices.Start(ctx, sp)
}

// TestSetupPassiveSelfCheckProbesTheWorker runs `setup --passive` with a
// service that really runs the worker: the self-check's TLS probe finds the
// worker serving the certificate setup generated.
func TestSetupPassiveSelfCheckProbesTheWorker(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	sf, e, stdout, stderr := newSetupFixture(t, nil)
	addr := freeAddr(t)
	cfgPath := filepath.Join(sf.dir, "worker.yaml")
	svc := &runningServices{}
	svc.start = func() func() int {
		runEnv, _ := f.runEnv(nil)
		runEnv.stderr = &syncBuffer{}
		return startRun(t, runEnv, "--config", cfgPath)
	}
	e.setup.services = svc
	e.setup.probe = nil // the real probe
	e.setup.timing = setup.Timing{Timeout: 5 * time.Second, Interval: 50 * time.Millisecond, Stable: 2}

	args := sf.args("--passive", "--listen", addr, "--advertise-url", "https://"+addr)
	if code := run(t.Context(), args, e); code != exitOK {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout(), stderr())
	}
	mustHave(t, stdout(), "ok    the worker serves the certificate with the printed fingerprint on "+addr)
	if svc.stop == nil {
		t.Fatal("the service was never started")
	}
	if code := svc.stop(); code != exitOK {
		t.Errorf("worker exit %d", code)
	}
}

// CompleteProbe implements ControllerLinkService; workers of this build run
// no probes (m5: Track A).
func (*fakeController) CompleteProbe(context.Context, *connect.Request[syncrv1.CompleteProbeRequest]) (*connect.Response[syncrv1.CompleteProbeResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
