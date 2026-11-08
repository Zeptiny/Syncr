package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/api"
	controllerconfig "github.com/Zeptiny/Syncr/internal/controller/config"
	"github.com/Zeptiny/Syncr/internal/platform/clientip"
	"github.com/Zeptiny/Syncr/internal/worker/tlscert"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
)

func envOf(vars map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
}

// testKey is 32 zero bytes in base64 (a test value, not a secret).
const testKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

// devEnv is the environment of a controller on the memory store, listening
// on a free loopback port and logging JSON, plus extra.
func devEnv(extra map[string]string) map[string]string {
	env := map[string]string{
		"SYNCR_HTTP_ADDR":        "127.0.0.1:0",
		"SYNCR_LOG_FORMAT":       "json",
		"SYNCR_SECRETS_KEY":      testKey,
		"SYNCR_DEV_MEMORY_STORE": "true",
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// syncBuffer is a goroutine-safe log sink.
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

func TestRunServesAndShutsDown(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var logs syncBuffer
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	env := envOf(devEnv(nil))
	go func() { done <- run(ctx, nil, io.Discard, &logs, env, ready) }()

	var addr net.Addr
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("run() exited early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("controller did not start")
	}
	base := "http://" + addr.String()

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/healthz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz status %d", resp.StatusCode)
	}

	client := syncrv1connect.NewSystemServiceClient(http.DefaultClient, base)
	info, err := client.GetInfo(t.Context(), connect.NewRequest(&syncrv1.GetInfoRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if info.Msg.GetVersion() == "" {
		t.Error("GetInfo returned an empty version")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() = %v after shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("controller did not shut down")
	}
	// Every shutdown step is logged, in order.
	out := logs.String()
	last := -1
	for _, step := range []string{
		`"msg":"shutdown: started`,
		`"msg":"shutdown: http server stopped"`,
		`"msg":"` + backgroundStoppedMessage + `"`,
		`"msg":"shutdown: store closed"`,
		`"msg":"controller stopped"`,
	} {
		i := strings.Index(out, step)
		if i < 0 || i < last {
			t.Errorf("log step %s missing or out of order:\n%s", step, out)
		}
		last = i
	}
	if !strings.Contains(out, "DEVELOPMENT MODE") {
		t.Errorf("no memory store warning:\n%s", out)
	}
}

func TestRunVersion(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := run(t.Context(), []string{"-version"}, &out, io.Discard, envOf(nil), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "syncr-controller ") {
		t.Errorf("output %q", out.String())
	}
}

func TestRunRefusesToStartWithoutStoreOrKey(t *testing.T) {
	t.Parallel()
	err := run(t.Context(), nil, io.Discard, io.Discard, envOf(map[string]string{"SYNCR_HTTP_ADDR": "127.0.0.1:0"}), nil)
	for _, want := range []string{"secrets.key is required", "database.url is required"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("run() = %v, want error containing %q", err, want)
		}
	}
	env := devEnv(nil)
	delete(env, "SYNCR_DEV_MEMORY_STORE")
	env["SYNCR_DATABASE_URL"] = "postgres://syncr:hunter2@127.0.0.1:1/syncr?sslmode=disable&connect_timeout=1"
	env["SYNCR_DATABASE_CONNECT_TIMEOUT"] = "1s"
	err = run(t.Context(), nil, io.Discard, io.Discard, envOf(env), nil)
	if err == nil || !strings.Contains(err.Error(), "open the database") {
		t.Errorf("run() with an unreachable database = %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks the database password: %v", err)
	}
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	err := run(t.Context(), nil, io.Discard, io.Discard, envOf(map[string]string{"SYNCR_HTTP_ADDR": "nope"}), nil)
	if err == nil || !strings.Contains(err.Error(), "http.addr") {
		t.Fatalf("run() = %v, want config validation error", err)
	}
	err = run(t.Context(), []string{"-config", "/does/not/exist.yaml"}, io.Discard, io.Discard, envOf(nil), nil)
	if err == nil {
		t.Fatal("run() with a missing config file = nil")
	}
	if err := run(t.Context(), []string{"-bogus"}, io.Discard, io.Discard, envOf(nil), nil); !errors.Is(err, errUsage) {
		t.Fatalf("run(-bogus) = %v, want errUsage", err)
	}
}

// startController runs the controller with env until the test ends and
// returns its address.
func startController(t *testing.T, env map[string]string, logs io.Writer) net.Addr {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, nil, io.Discard, logs, envOf(env), ready) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run() = %v after shutdown", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("controller did not shut down")
		}
	})
	select {
	case addr := <-ready:
		return addr
	case err := <-done:
		t.Fatalf("run() exited early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("controller did not start")
	}
	return nil
}

// testSetupCode is the preset first-run setup code of test controllers (a
// test value).
const testSetupCode = "TESTS-ETUPC-ODE01-23456"

// headers is a RoundTripper adding headers to every request.
type headers struct {
	next http.RoundTripper
	set  func(http.Header)
}

func (h headers) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	h.set(r.Header)
	return h.next.RoundTrip(r)
}

// bearerClient returns a client sending the personal API token with every
// request.
func bearerClient(tr http.RoundTripper, token string) *http.Client {
	return &http.Client{Transport: headers{next: tr, set: func(h http.Header) { h.Set("Authorization", "Bearer "+token) }}}
}

// cookieClient returns a client sending the session cookie with every
// request, with the CSRF header if csrf (as the web UI does).
func cookieClient(tr http.RoundTripper, cookie *http.Cookie, csrf bool) *http.Client {
	return &http.Client{Transport: headers{next: tr, set: func(h http.Header) {
		h.Add("Cookie", cookie.Name+"="+cookie.Value)
		if csrf {
			h.Set(api.CSRFHeader, "1")
		}
	}}}
}

// sessionCookie returns the session cookie a response sets.
func sessionCookie(t *testing.T, h http.Header) *http.Cookie {
	t.Helper()
	for _, line := range h.Values("Set-Cookie") {
		c, err := http.ParseSetCookie(line)
		if err == nil && (c.Name == api.SessionCookie || c.Name == api.SessionCookieSecure) && c.Value != "" {
			return c
		}
	}
	t.Fatalf("no session cookie in %q", h.Values("Set-Cookie"))
	return nil
}

// bootstrap completes the first-run setup of the controller at base with
// code, as the web UI's wizard does (CompleteSetup with the CSRF header,
// which sets the session cookie), creates an API token with that session
// and returns the token: what an operator does before scripting the API.
func bootstrap(t *testing.T, tr http.RoundTripper, base, code string) string {
	t.Helper()
	ctx := t.Context()
	setup := syncrv1connect.NewSetupServiceClient(&http.Client{Transport: tr}, base)
	status, err := setup.GetSetupStatus(ctx, connect.NewRequest(&syncrv1.GetSetupStatusRequest{}))
	if err != nil || !status.Msg.GetSetupRequired() {
		t.Fatalf("GetSetupStatus = (%v, %v), want setup required", status, err)
	}
	req := connect.NewRequest(&syncrv1.CompleteSetupRequest{SetupCode: code, Username: "admin", DisplayName: "Admin", Password: "a long admin password"})
	req.Header().Set(api.CSRFHeader, "1")
	done, err := setup.CompleteSetup(ctx, req)
	if err != nil {
		t.Fatalf("CompleteSetup: %v", err)
	}
	cookie := sessionCookie(t, done.Header())
	auth := syncrv1connect.NewAuthServiceClient(cookieClient(tr, cookie, true), base)
	tok, err := auth.CreateAPIToken(ctx, connect.NewRequest(&syncrv1.CreateAPITokenRequest{Name: "tests"}))
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if !strings.HasPrefix(tok.Msg.GetSecret(), "syncr_pat_") {
		t.Fatalf("token secret = %q", tok.Msg.GetSecret())
	}
	return tok.Msg.GetSecret()
}

func waitLog(t *testing.T, logs *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("log %q did not appear:\n%s", want, logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRunWiresAPIAndLink drives the wired controller over direct TLS: the
// first-run setup with the preset code (the cookie is the Secure one), an
// API token, the admin API creates an enrollment token, the enrollment
// service redeems it, and registering or removing a passive worker wakes
// the passive supervisor at once (its periodic resync is 30 s).
func TestRunWiresAPIAndLink(t *testing.T) {
	t.Parallel()
	pair, err := tlscert.Generate(tlscert.Options{Hostname: "localhost", IPs: []net.IP{net.IPv4(127, 0, 0, 1)}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if _, err := tlscert.Write(certFile, keyFile, pair, true); err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	env := devEnv(map[string]string{
		"SYNCR_SETUP_CODE":            testSetupCode,
		"SYNCR_HTTP_TLS_CERT_FILE":    certFile,
		"SYNCR_HTTP_TLS_KEY_FILE":     keyFile,
		"SYNCR_LOG_LEVEL":             "debug",
		"SYNCR_HTTP_CLIENT_IP_HEADER": "X-Forwarded-For",
		"SYNCR_HTTP_TRUSTED_PROXIES":  "127.0.0.1",
	})
	addr := startController(t, env, &logs)

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pair.CertPEM)
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	base := "https://" + addr.String()
	ctx := t.Context()
	token := bootstrap(t, tr, base, testSetupCode)
	admin := syncrv1connect.NewWorkerAdminServiceClient(bearerClient(tr, token), base)

	// Unauthenticated admin calls are refused.
	anon := syncrv1connect.NewWorkerAdminServiceClient(&http.Client{Transport: tr}, base)
	if _, err := anon.ListWorkers(ctx, connect.NewRequest(&syncrv1.ListWorkersRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("anonymous ListWorkers = %v", err)
	}

	tok, err := admin.CreateEnrollmentToken(ctx, connect.NewRequest(&syncrv1.CreateEnrollmentTokenRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	enroll := syncrv1connect.NewEnrollmentServiceClient(&http.Client{Transport: tr}, base)
	resp, err := enroll.Enroll(ctx, connect.NewRequest(&syncrv1.EnrollRequest{
		EnrollmentToken: tok.Msg.GetToken(), Name: "laptop", Os: "linux", Arch: "amd64",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.GetWorkerId() == "" || resp.Msg.GetWorkerSecret() == "" {
		t.Fatalf("enroll = %v", resp.Msg)
	}

	// A passive worker starts its link at once, and stops it on removal.
	reg, err := admin.RegisterPassiveWorker(ctx, connect.NewRequest(&syncrv1.RegisterPassiveWorkerRequest{
		Name: "nas", Url: "https://127.0.0.1:1", CertFingerprint: strings.Repeat("ab", 32),
		PairingSecret: "pairing-secret-0123456789-abcdefghij",
	}))
	if err != nil {
		t.Fatal(err)
	}
	waitLog(t, &logs, `"msg":"passive worker link started"`)
	if _, err := admin.RemoveWorker(ctx, connect.NewRequest(&syncrv1.RemoveWorkerRequest{WorkerId: reg.Msg.GetWorker().GetId()})); err != nil {
		t.Fatal(err)
	}
	waitLog(t, &logs, `"msg":"passive worker removed; stopping its link"`)

	list, err := admin.ListWorkers(ctx, connect.NewRequest(&syncrv1.ListWorkersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(list.Msg.GetWorkers()); n != 1 || list.Msg.GetWorkers()[0].GetName() != "laptop" {
		t.Errorf("workers = %v", list.Msg.GetWorkers())
	}

	// The container health check finds the TLS listener.
	var out bytes.Buffer
	hcEnv := devEnv(map[string]string{"SYNCR_HTTP_ADDR": addr.String(), "SYNCR_HTTP_TLS_CERT_FILE": certFile, "SYNCR_HTTP_TLS_KEY_FILE": keyFile})
	if err := run(ctx, []string{"-healthcheck"}, &out, io.Discard, envOf(hcEnv), nil); err != nil || strings.TrimSpace(out.String()) != "ok" {
		t.Errorf("-healthcheck = %v, output %q", err, out.String())
	}
}

func TestHealthcheckFailsWhenNothingListens(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	if err := run(t.Context(), []string{"-healthcheck"}, io.Discard, io.Discard, envOf(map[string]string{"SYNCR_HTTP_ADDR": addr}), nil); err == nil {
		t.Error("-healthcheck succeeded without a controller")
	}
}

func TestEnrollClientKey(t *testing.T) {
	t.Parallel()
	if key := enrollClientKey(controllerconfig.HTTP{}, clientip.New("", nil)); key != nil {
		t.Fatal("no header: want the default PeerIP")
	}
	cfg := controllerconfig.Default()
	cfg.HTTP.ClientIPHeader, cfg.HTTP.TrustedProxies = "X-Real-IP", []string{"10.0.0.0/8"}
	ac, err := newAPIConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	key := enrollClientKey(cfg.HTTP, ac.clientIP)
	h := http.Header{"X-Real-Ip": []string{"198.51.100.4"}}
	if got := key(connect.Peer{Addr: "10.1.2.3:4444"}, h); got != "198.51.100.4" {
		t.Errorf("from a trusted proxy: %q", got)
	}
	if got := key(connect.Peer{Addr: "192.0.2.9:4444"}, h); got != "192.0.2.9" {
		t.Errorf("from an untrusted peer: %q", got)
	}
}

// TestRunNotifiesAllowedNetworks: with the loopback address in
// notify.allowed_networks, a webhook on it receives the notification of a
// schedule's failed run at once (the scheduler wakes the outbox), linking
// to http.public_url.
func TestRunNotifiesAllowedNetworks(t *testing.T) {
	t.Parallel()
	got := make(chan []byte, 4)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Header.Get("Webhook-Id") == "" || r.Header.Get("Webhook-Signature") == "" {
			http.Error(w, "unsigned", http.StatusBadRequest)
			return
		}
		got <- b
	}))
	defer hook.Close()

	var logs syncBuffer
	addr := startController(t, devEnv(map[string]string{
		"SYNCR_SETUP_CODE": testSetupCode, "SYNCR_NOTIFY_ALLOWED_NETWORKS": "127.0.0.1",
		"SYNCR_PUBLIC_URL": "http://127.0.0.1:8080/", "SYNCR_SCHEDULES_TICK": "5m",
	}), &logs)
	base := "http://" + addr.String()
	ctx := t.Context()
	c := newM4Client(bearerClient(http.DefaultTransport, bootstrap(t, http.DefaultTransport, base, testSetupCode)), base)
	contact, err := c.contacts.CreateContact(ctx, connect.NewRequest(&syncrv1.CreateContactRequest{Input: &syncrv1.ContactInput{
		Name: "Local hook", ChannelType: "webhook", Enabled: true,
		Secrets: map[string]string{"url": hook.URL + "/hook", "signing_secret": "whsec_c3luY3ItdGVzdC1zaWduaW5nLWtleS0wMTIzNDU2Nzg5"},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	remote, err := c.remotes.CreateRemote(ctx, connect.NewRequest(&syncrv1.CreateRemoteRequest{Input: &syncrv1.RemoteInput{
		Name: "Data", BackendType: "local", Options: map[string]string{"path": "/srv/data"},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	sched, err := c.schedules.CreateSchedule(ctx, connect.NewRequest(&syncrv1.CreateScheduleRequest{
		Input: labelSchedule("Nightly", remote.Msg.GetRemote().GetId(), "nowhere", contact.Msg.GetContact().GetId()),
	}))
	if err != nil {
		t.Fatal(err)
	}
	id := sched.Msg.GetSchedule().GetId()
	if _, err := c.schedules.RunScheduleNow(ctx, connect.NewRequest(&syncrv1.RunScheduleNowRequest{ScheduleId: id})); err != nil {
		t.Fatal(err)
	}
	// The scheduler and outbox ticks are 5 minutes and 30 s: only their
	// wake-ups can deliver this in time.
	select {
	case b := <-got:
		var msg struct {
			Event string `json:"event"`
			URL   string `json:"url"`
			Run   struct {
				Reason string `json:"reason"`
			} `json:"run"`
		}
		if err := json.Unmarshal(b, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Event != "failed" || msg.Run.Reason != "no_worker_with_label" || msg.URL != "http://127.0.0.1:8080/schedules/"+id {
			t.Errorf("notification = %s", b)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("no notification:\n%s", logs.String())
	}
}
