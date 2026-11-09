package setup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	platformconfig "github.com/Zeptiny/Syncr/internal/platform/config"
	"github.com/Zeptiny/Syncr/internal/worker/config"
	"github.com/Zeptiny/Syncr/internal/worker/paths"
	"github.com/Zeptiny/Syncr/internal/worker/service"
	"github.com/Zeptiny/Syncr/internal/worker/tlscert"
)

const (
	enrolledID     = "0b6f8f0e-52a5-4a3f-9a5d-2f2a7c9f1e11"
	enrolledSecret = "wsec_Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5MGFiY2RlZmdoaWo"
	testToken      = "wtok_single-use-enrollment-token"
)

// fakeController is a Syncr enrollment endpoint on a TLS test server.
type fakeController struct {
	srv *httptest.Server
	// caFile holds the server's certificate as PEM.
	caFile string

	mu       sync.Mutex
	requests []*syncrv1.EnrollRequest
	// respond decides each answer; the default accepts testToken.
	respond func(*syncrv1.EnrollRequest) (*syncrv1.EnrollResponse, error)
	nextID  string
}

type enrollHandler struct {
	syncrv1connect.UnimplementedEnrollmentServiceHandler
	c *fakeController
}

func (h enrollHandler) Enroll(_ context.Context, req *connect.Request[syncrv1.EnrollRequest]) (*connect.Response[syncrv1.EnrollResponse], error) {
	h.c.mu.Lock()
	h.c.requests = append(h.c.requests, req.Msg)
	respond := h.c.respond
	id := h.c.nextID
	h.c.mu.Unlock()
	if respond != nil {
		resp, err := respond(req.Msg)
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(resp), nil
	}
	if req.Msg.GetEnrollmentToken() != testToken {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid, expired or used enrollment token"))
	}
	if id == "" {
		id = enrolledID
	}
	return connect.NewResponse(&syncrv1.EnrollResponse{WorkerId: id, WorkerSecret: enrolledSecret}), nil
}

func newFakeController(t *testing.T) *fakeController {
	t.Helper()
	c := &fakeController{}
	mux := http.NewServeMux()
	mux.Handle(syncrv1connect.NewEnrollmentServiceHandler(enrollHandler{c: c}))
	c.srv = httptest.NewTLSServer(mux)
	t.Cleanup(c.srv.Close)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.srv.Certificate().Raw})
	c.caFile = filepath.Join(t.TempDir(), "controller-ca.pem")
	if err := os.WriteFile(c.caFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *fakeController) seen() []*syncrv1.EnrollRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*syncrv1.EnrollRequest(nil), c.requests...)
}

// fakeServices is an in-memory service manager.
type fakeServices struct {
	mu    sync.Mutex
	calls []string
	// installed is the spec of the last Install.
	installed service.Spec
	state     service.State
	// afterStart is the state once Start has been called.
	afterStart   service.State
	preflightErr error
	installErr   error
	startErr     error
	statusErr    error
}

func newFakeServices() *fakeServices {
	return &fakeServices{state: service.StateNotInstalled, afterStart: service.StateRunning}
}

func (f *fakeServices) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
}

func (f *fakeServices) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeServices) Preflight(service.Spec) error { f.record("preflight"); return f.preflightErr }

func (f *fakeServices) Install(_ context.Context, spec service.Spec) error {
	f.record("install")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.installErr != nil {
		return f.installErr
	}
	f.installed = spec
	f.state = service.StateStopped
	return nil
}

func (f *fakeServices) Uninstall(context.Context, service.Spec) error {
	f.record("uninstall")
	f.mu.Lock()
	f.state = service.StateNotInstalled
	f.mu.Unlock()
	return nil
}

func (f *fakeServices) Start(context.Context, service.Spec) error {
	f.record("start")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.state = f.afterStart
	return nil
}

func (f *fakeServices) Stop(context.Context, service.Spec) error    { f.record("stop"); return nil }
func (f *fakeServices) Restart(context.Context, service.Spec) error { f.record("restart"); return nil }

func (f *fakeServices) Status(context.Context, service.Spec) (service.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.statusErr
}

func (f *fakeServices) Info(spec service.Spec) service.Info {
	return service.Info{Platform: "fake manager", Location: "fake://" + spec.Name, LogHint: "fake-logs " + spec.Name}
}

type fakeTools struct {
	mu      sync.Mutex
	dirs    []string
	err     error
	reports []ToolStatus
}

func (f *fakeTools) Provision(_ context.Context, dataDir string, progress func(string)) ([]ToolStatus, error) {
	f.mu.Lock()
	f.dirs = append(f.dirs, dataDir)
	f.mu.Unlock()
	progress("resolving rclone and restic")
	if f.err != nil {
		return nil, f.err
	}
	if f.reports != nil {
		return f.reports, nil
	}
	return []ToolStatus{
		{Name: "rclone", Version: "1.70.0", Source: "managed", Path: filepath.Join(dataDir, "tools", "rclone")},
		{Name: "restic", Version: "0.18.0", Source: "managed", Path: filepath.Join(dataDir, "tools", "restic")},
	}, nil
}

type rig struct {
	t      *testing.T
	setup  *Setup
	svc    *fakeServices
	tools  *fakeTools
	out    *bytes.Buffer
	dir    string
	data   string
	config string
	probe  func(ctx context.Context, addr string) (string, error)
}

// newRig builds a Setup whose every collaborator is a fake. Paths are set
// through options so that no test touches system locations.
func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, svc: newFakeServices(), tools: &fakeTools{}, out: &bytes.Buffer{}, dir: t.TempDir()}
	r.data = filepath.Join(r.dir, "data")
	r.config = filepath.Join(r.dir, "etc", "worker.yaml")
	r.build()
	return r
}

func (r *rig) build() {
	r.setup = New(Deps{
		System: System{
			GOOS: "linux", GOARCH: "amd64",
			Paths:      paths.Env{GOOS: "linux", LookupEnv: func(string) (string, bool) { return "", false }, UserHomeDir: func() (string, error) { return r.dir, nil }, Geteuid: func() int { return 0 }},
			Hostname:   func() (string, error) { return "build-host", nil },
			Executable: func() (string, error) { return "/opt/syncr/syncr-worker", nil },
		},
		Out:      r.out,
		Enroller: NewConnectEnroller,
		Tools:    r.tools,
		Services: r.svc,
		Random:   rand.Reader,
		Probe:    r.probe,
		Timing:   Timing{Timeout: 200 * time.Millisecond, Interval: time.Millisecond, Stable: 2},
	})
}

func (r *rig) options(o Options) Options {
	if o.Mode == "" {
		o.Mode = config.LinkActive
	}
	o.DataDir, o.ConfigPath = r.data, r.config
	return o
}

func (r *rig) activeOptions(c *fakeController) Options {
	return r.options(Options{ControllerURL: c.srv.URL, Token: testToken, CAFile: c.caFile, LocalRoots: []string{filepath.Join(r.dir, "files")}})
}

func (r *rig) passiveOptions(o Options) Options {
	o.Mode = config.LinkPassive
	if o.ListenAddr == "" {
		o.ListenAddr = ":7443"
	}
	if o.AdvertiseURL == "" {
		o.AdvertiseURL = "https://worker.example.com:7443"
	}
	return r.options(o)
}

func (r *rig) run(o Options) (*Result, error) {
	r.t.Helper()
	return r.setup.Run(r.t.Context(), o)
}

func (r *rig) loadConfig() config.Config {
	r.t.Helper()
	cfg := config.Default()
	if err := platformconfig.Load(r.config, nil, &cfg); err != nil {
		r.t.Fatalf("the written config does not load: %v", err)
	}
	return cfg
}

func (r *rig) mustRun(o Options) *Result {
	r.t.Helper()
	res, err := r.run(o)
	if err != nil {
		r.t.Fatalf("setup failed: %v\noutput:\n%s", err, r.out)
	}
	return res
}

func (r *rig) assertNothingWritten() {
	r.t.Helper()
	if _, err := os.Stat(r.config); !errors.Is(err, os.ErrNotExist) {
		r.t.Errorf("config exists after a failed setup (stat err = %v)", err)
	}
}

func mustContain(t *testing.T, what, text string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Errorf("%s does not contain %q:\n%s", what, w, text)
		}
	}
}

func TestActiveSetupEndToEnd(t *testing.T) {
	t.Parallel()
	c := newFakeController(t)
	r := newRig(t)
	res := r.mustRun(r.activeOptions(c))

	reqs := c.seen()
	if len(reqs) != 1 {
		t.Fatalf("controller saw %d enrollments, want 1", len(reqs))
	}
	got := reqs[0]
	if got.GetEnrollmentToken() != testToken || got.GetName() != "build-host" || got.GetOs() != "linux" || got.GetArch() != "amd64" ||
		got.GetMode() != syncrv1.WorkerMode_WORKER_MODE_ACTIVE || got.GetUrl() != "" || got.GetCertFingerprint() != "" {
		t.Errorf("enroll request = %v", got)
	}

	cfg := r.loadConfig()
	l := cfg.Link
	if l.Mode != config.LinkActive || l.WorkerID != enrolledID || l.WorkerSecret.Reveal() != enrolledSecret || l.ControllerURL != c.srv.URL {
		t.Errorf("link = %+v", l)
	}
	if cfg.DataDir != r.data || len(cfg.LocalRoots) != 1 {
		t.Errorf("config = %+v", cfg)
	}
	// The CA was copied next to the worker's data and the config points at it.
	if want := filepath.Join(r.data, "tls", "controller-ca.pem"); l.ControllerCAFile != want {
		t.Errorf("controller_ca_file = %q, want %q", l.ControllerCAFile, want)
	}
	if _, err := os.Stat(l.ControllerCAFile); err != nil {
		t.Error(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(r.config)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("config mode = %v, want 0600", info.Mode().Perm())
		}
	}

	if want := []string{"preflight", "install", "start"}; !equalPrefixed(r.svc.called(), want) {
		t.Errorf("service calls = %v, want %v in order", r.svc.called(), want)
	}
	if len(r.tools.dirs) != 1 || r.tools.dirs[0] != r.data {
		t.Errorf("tools provisioned in %v", r.tools.dirs)
	}
	if res.WorkerID != enrolledID || res.Service != service.StateRunning || len(res.Tools) != 2 {
		t.Errorf("result = %+v", res)
	}

	out := r.out.String()
	mustContain(t, "output", out, "Enrolling with", enrolledID, "Setup complete", "fake-logs syncr-worker", "rclone 1.70.0")
	for _, secret := range []string{testToken, enrolledSecret} {
		if strings.Contains(out, secret) {
			t.Errorf("output leaks %q", secret)
		}
	}
}

// equalPrefixed reports whether want appears in order as a subsequence of
// got.
func equalPrefixed(got, want []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

func TestActiveSetupWithoutCASuggestsCAFile(t *testing.T) {
	t.Parallel()
	c := newFakeController(t)
	r := newRig(t)
	o := r.activeOptions(c)
	o.CAFile = "" // the test server's certificate is signed by nobody the system trusts
	_, err := r.run(o)
	var ee *EnrollError
	if !errors.As(err, &ee) || ee.Kind != EnrollTLS {
		t.Fatalf("err = %v, want a TLS EnrollError", err)
	}
	mustContain(t, "error", err.Error(), "--ca-file", "cannot be turned off")
	if len(c.seen()) != 0 {
		t.Error("the controller handled a request over an unverified connection")
	}
	r.assertNothingWritten()
	if got := r.svc.called(); equalPrefixed(got, []string{"install"}) {
		t.Errorf("service installed despite the failed enrollment: %v", got)
	}
}

func TestActiveSetupRejectedToken(t *testing.T) {
	t.Parallel()
	c := newFakeController(t)
	r := newRig(t)
	o := r.activeOptions(c)
	o.Token = "wtok_used-already"
	_, err := r.run(o)
	var ee *EnrollError
	if !errors.As(err, &ee) || ee.Kind != EnrollTokenRejected {
		t.Fatalf("err = %v, want a rejected-token EnrollError", err)
	}
	mustContain(t, "error", err.Error(), "wrong, expired or already used", "new token")
	if strings.Contains(err.Error(), "wtok_used-already") {
		t.Error("error echoes the token")
	}
	r.assertNothingWritten()
}

func TestActiveSetupUnreachableController(t *testing.T) {
	t.Parallel()
	c := newFakeController(t)
	r := newRig(t)
	o := r.activeOptions(c)
	c.srv.Close()
	_, err := r.run(o)
	var ee *EnrollError
	if !errors.As(err, &ee) || ee.Kind != EnrollUnreachable {
		t.Fatalf("err = %v, want an unreachable EnrollError", err)
	}
	mustContain(t, "error", err.Error(), "cannot reach the controller", o.ControllerURL)
	r.assertNothingWritten()
}

func TestActiveSetupWrongURLIsNotAController(t *testing.T) {
	t.Parallel()
	notSyncr := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(notSyncr.Close)
	r := newRig(t)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: notSyncr.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := r.run(r.options(Options{ControllerURL: notSyncr.URL, Token: testToken, CAFile: caFile}))
	var ee *EnrollError
	if !errors.As(err, &ee) || ee.Kind != EnrollNotController {
		t.Fatalf("err = %v, want a not-a-controller EnrollError", err)
	}
	mustContain(t, "error", err.Error(), "base URL", "reverse proxy")
}

func TestActiveSetupInvalidRequestFromController(t *testing.T) {
	t.Parallel()
	c := newFakeController(t)
	c.respond = func(*syncrv1.EnrollRequest) (*syncrv1.EnrollResponse, error) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is longer than 64 characters"))
	}
	r := newRig(t)
	_, err := r.run(r.activeOptions(c))
	mustContain(t, "error", err.Error(), "rejected the request", "name is longer")
}

func TestBadCAFile(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	bad := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := r.run(r.options(Options{ControllerURL: "https://syncr.example.com", Token: testToken, CAFile: bad}))
	mustContain(t, "error", err.Error(), "no PEM certificate")
	if len(r.tools.dirs) != 0 {
		t.Error("tools were provisioned before the request was validated")
	}
	_, err = r.run(r.options(Options{ControllerURL: "https://syncr.example.com", Token: testToken, CAFile: filepath.Join(t.TempDir(), "missing.pem")}))
	mustContain(t, "error", err.Error(), "read the CA file")
}

func TestPassiveManualRegistration(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	o := r.passiveOptions(Options{TLSNames: []string{"worker-alias.lan", "10.0.0.7"}})
	res := r.mustRun(o)

	cfg := r.loadConfig()
	l := cfg.Link
	if l.Mode != config.LinkPassive || l.ListenAddr != ":7443" || l.AdvertiseURL != "https://worker.example.com:7443" || l.WorkerID != "" {
		t.Errorf("link = %+v", l)
	}
	if err := config.ValidatePairingSecret(l.PairingSecret.Reveal()); err != nil {
		t.Errorf("pairing secret: %v", err)
	}
	if l.PairingSecret != res.PairingSecret {
		t.Error("printed and stored pairing secrets differ")
	}

	// The printed fingerprint is exactly what a client sees and what the
	// controller would pin.
	fp, err := tlscert.FingerprintFile(l.TLSCertFile)
	if err != nil || fp != res.Fingerprint {
		t.Errorf("fingerprint of the stored certificate = %q, %v; printed %q", fp, err, res.Fingerprint)
	}
	cert, err := tlscert.Load(l.TLSCertFile, l.TLSKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(cert.Leaf.DNSNames, []string{"worker.example.com", "worker-alias.lan", "build-host"}) || len(cert.Leaf.IPAddresses) != 1 {
		t.Errorf("SANs: DNS %q IPs %v", cert.Leaf.DNSNames, cert.Leaf.IPAddresses)
	}

	out := r.out.String()
	mustContain(t, "output", out,
		"URL:             https://worker.example.com:7443",
		"Fingerprint:     "+res.Fingerprint,
		"Pairing secret:  "+res.PairingSecret.Reveal(),
		"Register this worker")
	if !equalPrefixed(r.svc.called(), []string{"install", "start"}) {
		t.Errorf("service calls = %v", r.svc.called())
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPassiveEnrollAutomatically(t *testing.T) {
	t.Parallel()
	c := newFakeController(t)
	r := newRig(t)
	o := r.passiveOptions(Options{ControllerURL: c.srv.URL, Token: testToken, CAFile: c.caFile, Name: "nas"})
	res := r.mustRun(o)

	reqs := c.seen()
	if len(reqs) != 1 {
		t.Fatalf("enrollments = %d", len(reqs))
	}
	got := reqs[0]
	if got.GetMode() != syncrv1.WorkerMode_WORKER_MODE_PASSIVE || got.GetUrl() != "https://worker.example.com:7443" ||
		got.GetCertFingerprint() != res.Fingerprint || got.GetName() != "nas" {
		t.Errorf("request = %v (printed fingerprint %s)", got, res.Fingerprint)
	}
	l := r.loadConfig().Link
	if l.PairingSecret.Reveal() != enrolledSecret || l.WorkerID != enrolledID {
		t.Errorf("link = %+v: the controller's secret must become the pairing secret", l)
	}
	out := r.out.String()
	if strings.Contains(out, enrolledSecret) {
		t.Error("the controller-issued secret was printed")
	}
	mustContain(t, "output", out, "pinned fingerprint "+res.Fingerprint)
}

func TestPassiveEnrollRejectsUnusableSecret(t *testing.T) {
	t.Parallel()
	c := newFakeController(t)
	c.respond = func(*syncrv1.EnrollRequest) (*syncrv1.EnrollResponse, error) {
		return &syncrv1.EnrollResponse{WorkerId: enrolledID, WorkerSecret: "short"}, nil
	}
	r := newRig(t)
	_, err := r.run(r.passiveOptions(Options{ControllerURL: c.srv.URL, Token: testToken, CAFile: c.caFile}))
	mustContain(t, "error", err.Error(), "cannot use", enrolledID, "new token")
	r.assertNothingWritten()
}

func TestPassiveWithProvidedCertificate(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	pair, err := tlscert.Generate(tlscert.Options{Hostname: "mine.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(r.dir, "mine.crt"), filepath.Join(r.dir, "mine.key")
	if _, err := tlscert.Write(certPath, keyPath, pair, false); err != nil {
		t.Fatal(err)
	}
	res := r.mustRun(r.passiveOptions(Options{TLSCertFile: certPath, TLSKeyFile: keyPath}))
	if res.Fingerprint != pair.Fingerprint {
		t.Errorf("fingerprint = %s, want %s", res.Fingerprint, pair.Fingerprint)
	}
	if l := r.loadConfig().Link; l.TLSCertFile != certPath || l.TLSKeyFile != keyPath {
		t.Errorf("link = %+v", l)
	}
	if _, err := os.Stat(filepath.Join(r.data, "tls", "worker.crt")); err == nil {
		t.Error("a certificate was generated although one was provided")
	}

	// A key that does not belong to the certificate is caught.
	other, _ := tlscert.Generate(tlscert.Options{Hostname: "other"})
	otherKey := filepath.Join(r.dir, "other.key")
	if err := os.WriteFile(otherKey, other.KeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	r2 := newRig(t)
	_, err = r2.run(r2.passiveOptions(Options{TLSCertFile: certPath, TLSKeyFile: otherKey}))
	mustContain(t, "error", err.Error(), "cannot be used", "does not match")
}

func TestAlreadySetUpIsRefusedAndForceReenrolls(t *testing.T) {
	t.Parallel()
	c := newFakeController(t)
	r := newRig(t)
	o := r.activeOptions(c)
	r.mustRun(o)
	before, err := os.ReadFile(r.config)
	if err != nil {
		t.Fatal(err)
	}

	// Again, without --force: refused, and the token is not spent.
	_, err = r.run(o)
	if !errors.Is(err, ErrAlreadySetUp) {
		t.Fatalf("err = %v, want ErrAlreadySetUp", err)
	}
	mustContain(t, "error", err.Error(), "--force", r.config, "active worker "+enrolledID, "service (running)")
	if len(c.seen()) != 1 {
		t.Errorf("enrollments = %d: the refused run contacted the controller", len(c.seen()))
	}
	if after, _ := os.ReadFile(r.config); !bytes.Equal(before, after) {
		t.Error("the config changed although the run was refused")
	}

	// With --force: new enrollment, new config, service reinstalled.
	c.mu.Lock()
	c.nextID = "11111111-2222-4333-8444-555555555555"
	c.mu.Unlock()
	r.out.Reset()
	o.Force = true
	r.mustRun(o)
	if len(c.seen()) != 2 {
		t.Errorf("enrollments = %d, want 2", len(c.seen()))
	}
	if id := r.loadConfig().Link.WorkerID; id != "11111111-2222-4333-8444-555555555555" {
		t.Errorf("worker id after --force = %s", id)
	}
	if calls := r.svc.called(); !equalPrefixed(calls, []string{"install", "start", "uninstall", "install", "start"}) {
		t.Errorf("service calls = %v, want the service replaced", calls)
	}
}

func TestExistingConfigWithoutServiceIsAlsoRefused(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := os.MkdirAll(filepath.Dir(r.config), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.config, []byte("log:\n  level: info\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := r.options(Options{ControllerURL: "https://syncr.example.com", Token: testToken})
	if _, err := r.run(o); !errors.Is(err, ErrAlreadySetUp) {
		t.Fatalf("err = %v", err)
	}
	if len(r.tools.dirs) != 0 {
		t.Error("tools provisioned before the already-set-up check")
	}
}

func TestExistingServiceWithoutConfigIsAlsoRefused(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.svc.state = service.StateStopped
	o := r.options(Options{ControllerURL: "https://syncr.example.com", Token: testToken})
	_, err := r.run(o)
	if !errors.Is(err, ErrAlreadySetUp) {
		t.Fatalf("err = %v", err)
	}
	mustContain(t, "error", err.Error(), "syncr-worker service")
}

func TestPassiveRerunKeepsCertificateAndForceRegeneratesIt(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	o := r.passiveOptions(Options{NoService: true})
	first := r.mustRun(o)

	// A failed first run leaves the certificate but no config: simulate by
	// removing the config. The next run reuses the certificate, so the
	// fingerprint an admin may already have copied stays valid.
	if err := os.Remove(r.config); err != nil {
		t.Fatal(err)
	}
	second := r.mustRun(o)
	if second.Fingerprint != first.Fingerprint {
		t.Errorf("fingerprint changed on a plain re-run: %s -> %s", first.Fingerprint, second.Fingerprint)
	}
	mustContain(t, "output", r.out.String(), "reusing the existing certificate")

	o.Force = true
	third := r.mustRun(o)
	if third.Fingerprint == first.Fingerprint {
		t.Error("--force kept the old certificate")
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	t.Parallel()
	c := newFakeController(t)
	for _, tt := range []struct {
		name string
		opts func(*rig) Options
		want []string
	}{
		{"active", func(r *rig) Options { return r.activeOptions(c) }, []string{"Dry run", "enroll with " + c.srv.URL, "provision the pinned rclone and restic", "fake manager"}},
		{"passive manual", func(r *rig) Options { return r.passiveOptions(Options{}) }, []string{"create a self-signed TLS certificate for worker.example.com", "generate a pairing secret"}},
		{"passive enroll", func(r *rig) Options {
			return r.passiveOptions(Options{ControllerURL: c.srv.URL, Token: testToken, CAFile: c.caFile})
		}, []string{"(passive, https://worker.example.com:7443)"}},
		{"no service", func(r *rig) Options { o := r.activeOptions(c); o.NoService = true; return o }, []string{"skip the service"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			o := tt.opts(r)
			o.DryRun = true
			res, err := r.run(o)
			if err != nil {
				t.Fatal(err)
			}
			if !res.DryRun {
				t.Error("result is not marked as a dry run")
			}
			mustContain(t, "plan", r.out.String(), tt.want...)
			if entries, _ := os.ReadDir(r.dir); len(entries) != 0 {
				t.Errorf("dry run created %d entries in the sandbox", len(entries))
			}
			if len(c.seen()) != 0 || len(r.tools.dirs) != 0 {
				t.Errorf("dry run enrolled (%d) or provisioned tools (%v)", len(c.seen()), r.tools.dirs)
			}
			for _, call := range r.svc.called() {
				if call != "preflight" {
					t.Errorf("dry run called the service manager: %s", call)
				}
			}
			if strings.Contains(r.out.String(), testToken) {
				t.Error("plan prints the token")
			}
		})
	}
}

func TestDryRunStillRefusesExistingSetup(t *testing.T) {
	t.Parallel()
	c := newFakeController(t)
	r := newRig(t)
	r.mustRun(r.activeOptions(c))
	o := r.activeOptions(c)
	o.DryRun = true
	if _, err := r.run(o); !errors.Is(err, ErrAlreadySetUp) {
		t.Fatalf("err = %v", err)
	}
	o.Force = true
	r.out.Reset()
	if _, err := r.run(o); err != nil {
		t.Fatal(err)
	}
	mustContain(t, "plan", r.out.String(), "remove the existing syncr-worker service", "would be replaced")
}

func TestDryRunDefaultPaths(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	programData := filepath.Join(home, "ProgramData")
	localAppData := filepath.Join(home, "AppData", "Local")
	tests := []struct {
		name  string
		goos  string
		user  bool
		wants []string
	}{
		{"linux system", "linux", false, []string{"/var/lib/syncr", "/etc/syncr/worker.yaml"}},
		{"linux user", "linux", true, []string{filepath.Join(home, ".local", "share", "syncr"), filepath.Join(home, ".config", "syncr", "worker.yaml")}},
		{"windows system", "windows", false, []string{filepath.Join(programData, "Syncr")}},
		{"windows user", "windows", true, []string{filepath.Join(localAppData, "Syncr"), filepath.Join(localAppData, "Syncr", "worker.yaml")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// The config is validated with the host's filepath rules, so a simulated
			// foreign-OS path (e.g. /var/lib/syncr on Windows) cannot pass validation.
			if tt.goos != runtime.GOOS && !filepath.IsAbs(tt.wants[0]) {
				t.Skipf("simulated %s default paths are not absolute on a %s host", tt.goos, runtime.GOOS)
			}
			r := newRig(t)
			env := map[string]string{"ProgramData": programData, "LocalAppData": localAppData}
			r.setup.System.GOOS = tt.goos
			r.setup.System.Paths = paths.Env{GOOS: tt.goos, LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
				UserHomeDir: func() (string, error) { return home, nil }, Geteuid: func() int { return 0 }}
			o := Options{Mode: config.LinkActive, ControllerURL: "https://syncr.example.com", Token: testToken, User: tt.user, DryRun: true}
			if _, err := r.run(o); err != nil {
				t.Fatal(err)
			}
			mustContain(t, "plan", r.out.String(), tt.wants...)
		})
	}
}

func TestMissingPermissionsFailBeforeAnythingChanges(t *testing.T) {
	t.Parallel()
	c := newFakeController(t)
	r := newRig(t)
	r.svc.preflightErr = &service.PermissionError{Scope: service.ScopeSystem, Hint: "run it as root (for example with sudo), or use --user"}
	_, err := r.run(r.activeOptions(c))
	var perm *service.PermissionError
	if !errors.As(err, &perm) {
		t.Fatalf("err = %v, want a PermissionError", err)
	}
	mustContain(t, "error", err.Error(), "--user", "root")
	if len(c.seen()) != 0 || len(r.tools.dirs) != 0 {
		t.Error("enrolled or provisioned tools before checking permissions")
	}
	if entries, _ := os.ReadDir(r.dir); len(entries) != 0 {
		t.Error("files created before checking permissions")
	}
}

func TestUnwritableDirectoriesFailBeforeEnrolling(t *testing.T) {
	t.Parallel()
	c := newFakeController(t)
	r := newRig(t)
	blocker := filepath.Join(r.dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := r.activeOptions(c)
	o.ConfigPath = filepath.Join(blocker, "worker.yaml") // its parent is a file
	_, err := r.run(o)
	if err == nil {
		t.Fatal("setup succeeded")
	}
	if len(c.seen()) != 0 {
		t.Error("the token was spent although the config could not be stored")
	}
}

func TestOptionValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{"no mode", Options{}, "choose a mode"},
		{"active without controller", Options{Mode: config.LinkActive, Token: testToken}, "--controller is required"},
		{"active without token", Options{Mode: config.LinkActive, ControllerURL: "https://c.example.com"}, "SYNCR_ENROLLMENT_TOKEN"},
		{"controller over plain http", Options{Mode: config.LinkActive, ControllerURL: "http://c.example.com", Token: testToken}, "plain http"},
		{"controller not a URL", Options{Mode: config.LinkActive, ControllerURL: "c.example.com", Token: testToken}, "--controller"},
		{"active with passive flag", Options{Mode: config.LinkActive, ControllerURL: "https://c.example.com", Token: testToken, ListenAddr: ":7443"}, "--listen only applies to a passive worker"},
		{"active with tls name", Options{Mode: config.LinkActive, ControllerURL: "https://c.example.com", Token: testToken, TLSNames: []string{"x"}}, "--tls-name only applies"},
		{"passive without advertise", Options{Mode: config.LinkPassive, ListenAddr: ":7443"}, "--advertise-url"},
		{"passive without listen", Options{Mode: config.LinkPassive, AdvertiseURL: "https://w.example.com"}, "--listen"},
		{"passive bad listen", Options{Mode: config.LinkPassive, ListenAddr: "7443", AdvertiseURL: "https://w.example.com"}, "--listen"},
		{"passive http advertise", Options{Mode: config.LinkPassive, ListenAddr: ":7443", AdvertiseURL: "http://w.example.com"}, "https"},
		{"passive token without controller", Options{Mode: config.LinkPassive, ListenAddr: ":7443", AdvertiseURL: "https://w.example.com", Token: testToken}, "both --controller and --token"},
		{"passive controller without token", Options{Mode: config.LinkPassive, ListenAddr: ":7443", AdvertiseURL: "https://w.example.com", ControllerURL: "https://c.example.com"}, "both --controller and --token"},
		{"passive cert without key", Options{Mode: config.LinkPassive, ListenAddr: ":7443", AdvertiseURL: "https://w.example.com", TLSCertFile: "/c.pem"}, "--tls-cert and --tls-key go together"},
		{"passive ca without controller", Options{Mode: config.LinkPassive, ListenAddr: ":7443", AdvertiseURL: "https://w.example.com", CAFile: "/ca.pem"}, "--ca-file"},
		{"passive bad tls name", Options{Mode: config.LinkPassive, ListenAddr: ":7443", AdvertiseURL: "https://w.example.com", TLSNames: []string{"http://x"}}, "--tls-name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			tt.opts.DataDir, tt.opts.ConfigPath = r.data, r.config
			_, err := r.run(tt.opts)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
			if len(r.tools.dirs) != 0 {
				t.Error("tools provisioned for an invalid request")
			}
		})
	}
}

func TestRelativeLocalRootIsMadeAbsolute(t *testing.T) {
	t.Parallel()
	c := newFakeController(t)
	r := newRig(t)
	o := r.activeOptions(c)
	o.LocalRoots = []string{"relative/dir"}
	r.mustRun(o)
	roots := r.loadConfig().LocalRoots
	if len(roots) != 1 || !filepath.IsAbs(roots[0]) || !strings.HasSuffix(roots[0], filepath.Join("relative", "dir")) {
		t.Errorf("local_roots = %q", roots)
	}
}

// TestRelativePathsAreMadeAbsolute runs setup with relative paths: the
// service starts in another directory (systemd's WorkingDirectory, System32
// for the Windows SCM), so the config and the service must name absolute
// paths. Not parallel: it changes the working directory.
func TestRelativePathsAreMadeAbsolute(t *testing.T) {
	sameFile := func(t *testing.T, what, got, want string) {
		t.Helper()
		if !filepath.IsAbs(got) {
			t.Errorf("%s = %q, want an absolute path", what, got)
			return
		}
		gi, gerr := os.Stat(got)
		wi, werr := os.Stat(want)
		if gerr != nil || werr != nil || !os.SameFile(gi, wi) {
			t.Errorf("%s = %q, want %q (%v, %v)", what, got, want, gerr, werr)
		}
	}

	t.Run("active", func(t *testing.T) {
		c := newFakeController(t)
		r := newRig(t)
		t.Chdir(r.dir)
		caRel, err := filepath.Rel(r.dir, c.caFile)
		if err != nil {
			t.Skipf("the CA file has no path relative to %s: %v", r.dir, err)
		}
		// Only --config relative: before the fix this passed validation (and
		// the self-check, which runs in the same directory), but the service
		// was given "run --config etc/worker.yaml" and could not find it.
		o := r.activeOptions(c)
		o.ConfigPath, o.CAFile = filepath.Join("etc", "worker.yaml"), caRel
		res := r.mustRun(o)

		sameFile(t, "result config path", res.ConfigPath, r.config)
		sameFile(t, "result data dir", res.DataDir, r.data)
		cfg := r.loadConfig()
		sameFile(t, "data_dir", cfg.DataDir, r.data)
		spec := r.svc.installed
		if len(spec.Args) < 3 || spec.Args[1] != "--config" {
			t.Fatalf("service args = %q", spec.Args)
		}
		sameFile(t, "service --config", spec.Args[2], r.config)
		sameFile(t, "service working directory", spec.WorkDir, r.data)
	})

	t.Run("passive with a provided certificate", func(t *testing.T) {
		r := newRig(t)
		t.Chdir(r.dir)
		pair, err := tlscert.Generate(tlscert.Options{Hostname: "mine.example.com"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tlscert.Write(filepath.Join(r.dir, "mine.crt"), filepath.Join(r.dir, "mine.key"), pair, false); err != nil {
			t.Fatal(err)
		}
		o := r.passiveOptions(Options{TLSCertFile: "mine.crt", TLSKeyFile: "mine.key"})
		o.DataDir, o.ConfigPath = "data", filepath.Join("etc", "worker.yaml")
		r.mustRun(o)
		l := r.loadConfig().Link
		sameFile(t, "tls_cert_file", l.TLSCertFile, filepath.Join(r.dir, "mine.crt"))
		sameFile(t, "tls_key_file", l.TLSKeyFile, filepath.Join(r.dir, "mine.key"))
	})
}

func TestToolsFailureStopsBeforeEnrollment(t *testing.T) {
	t.Parallel()
	c := newFakeController(t)
	r := newRig(t)
	r.tools.err = errors.New("checksum mismatch")
	_, err := r.run(r.activeOptions(c))
	mustContain(t, "error", err.Error(), "provision the tools", "checksum mismatch", "no internet access")
	if len(c.seen()) != 0 {
		t.Error("token spent although the tools failed")
	}
	r.assertNothingWritten()
}

func TestServiceInstallFailureKeepsConfigAndExplainsRecovery(t *testing.T) {
	t.Parallel()
	c := newFakeController(t)
	r := newRig(t)
	r.svc.installErr = errors.New("systemctl: Access denied")
	_, err := r.run(r.activeOptions(c))
	if err == nil {
		t.Fatal("setup succeeded")
	}
	mustContain(t, "error", err.Error(), "do not enroll again", "service install --start", "fake-logs", "Access denied")
	if _, serr := os.Stat(r.config); serr != nil {
		t.Errorf("config was not kept: %v", serr)
	}
	if len(c.seen()) != 1 {
		t.Errorf("enrollments = %d", len(c.seen()))
	}
}

func TestServicePermissionFailureDuringInstall(t *testing.T) {
	t.Parallel()
	c := newFakeController(t)
	r := newRig(t)
	r.svc.installErr = &service.PermissionError{Scope: service.ScopeSystem, Hint: "run it as root, or use --user"}
	o := r.activeOptions(c)
	o.User = true
	_, err := r.run(o)
	mustContain(t, "error", err.Error(), "--user", "service install --start --user")
}

func TestSelfCheckDetectsCrashLoop(t *testing.T) {
	t.Parallel()
	c := newFakeController(t)
	r := newRig(t)
	r.svc.afterStart = service.StateFailing
	res, err := r.run(r.activeOptions(c))
	if err == nil {
		t.Fatal("a crash-looping service passed the self-check")
	}
	mustContain(t, "error", err.Error(), "did not stay running", "failing", "fake-logs syncr-worker", "service restart")
	if res == nil || res.Service != service.StateFailing {
		t.Errorf("result = %+v", res)
	}
}

func TestSelfCheckPassiveProbe(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		probe func(fp string) TLSProbe
		want  string
	}{
		{"serving", func(fp string) TLSProbe { return func(context.Context, string) (string, error) { return fp, nil } }, "ok    the worker serves the certificate"},
		{"not listening", func(string) TLSProbe {
			return func(context.Context, string) (string, error) { return "", errors.New("connection refused") }
		}, "warn  no TLS answer from 127.0.0.1:7443 yet"},
		{"foreign certificate", func(string) TLSProbe {
			return func(context.Context, string) (string, error) { return strings.Repeat("ab", 32), nil }
		}, "another program may own that port"},
		{"listening after a moment", func(fp string) TLSProbe {
			var calls atomic.Int32
			return func(context.Context, string) (string, error) {
				if calls.Add(1) < 3 {
					return "", errors.New("connection refused")
				}
				return fp, nil
			}
		}, "ok    the worker serves the certificate"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			// The probe is told the fingerprint through a pointer filled in
			// after the certificate exists: use the stored certificate.
			var probe TLSProbe
			r.setup.Probe = func(ctx context.Context, addr string) (string, error) {
				fp, _ := tlscert.FingerprintFile(filepath.Join(r.data, "tls", "worker.crt"))
				if probe == nil {
					probe = tt.probe(fp)
				}
				return probe(ctx, addr)
			}
			r.mustRun(r.passiveOptions(Options{}))
			mustContain(t, "output", r.out.String(), tt.want)
		})
	}
}

func TestNoServiceSkipsServiceManager(t *testing.T) {
	t.Parallel()
	c := newFakeController(t)
	r := newRig(t)
	o := r.activeOptions(c)
	o.NoService = true
	res := r.mustRun(o)
	if len(r.svc.called()) != 0 {
		t.Errorf("service manager used with --no-service: %v", r.svc.called())
	}
	mustContain(t, "output", r.out.String(), "No service was installed", "syncr-worker run --config "+r.config)
	if res.Service != service.StateNotInstalled {
		t.Errorf("service = %v", res.Service)
	}
}

func TestEnrolledButConfigCannotBeWritten(t *testing.T) {
	t.Parallel()
	c := newFakeController(t)
	r := newRig(t)
	// A directory where the config file must go: the final rename fails
	// after the token was spent.
	if err := os.MkdirAll(r.config, 0o700); err != nil {
		t.Fatal(err)
	}
	o := r.activeOptions(c)
	o.Force = true
	_, err := r.run(o)
	if err == nil {
		t.Fatal("setup succeeded")
	}
	mustContain(t, "error", err.Error(), "enrolled this worker (ID "+enrolledID+")", "token is spent", "new token")
	if strings.Contains(err.Error(), enrolledSecret) {
		t.Error("the error leaks the secret")
	}
}

func TestNoLocalRootNote(t *testing.T) {
	t.Parallel()
	c := newFakeController(t)
	r := newRig(t)
	o := r.activeOptions(c)
	o.LocalRoots = nil
	r.mustRun(o)
	mustContain(t, "output", r.out.String(), "no --local-root was given")
}

func TestExplicitToolPathsAreWrittenToTheConfig(t *testing.T) {
	t.Parallel()
	c := newFakeController(t)
	r := newRig(t)
	rclone := filepath.Join(r.dir, "bin", "rclone")
	r.tools.reports = []ToolStatus{
		{Name: "rclone", Version: "1", Source: "configured", Path: rclone},
		{Name: "restic", Version: "2", Source: "managed", Path: "/managed/restic"},
	}
	r.mustRun(r.activeOptions(c))
	cfg := r.loadConfig()
	if cfg.Tools.RclonePath != rclone || cfg.Tools.ResticPath != "" {
		t.Errorf("tools = %+v: only explicitly configured binaries belong in the config", cfg.Tools)
	}
}
