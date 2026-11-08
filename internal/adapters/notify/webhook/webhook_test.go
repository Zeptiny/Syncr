package webhook_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/notify/webhook"
	"github.com/Zeptiny/Syncr/internal/descriptor"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/notify"
	"github.com/Zeptiny/Syncr/internal/notify/notifytest"
	"github.com/Zeptiny/Syncr/internal/platform/netguard"
)

// The test vector of the Standard Webhooks specification.
const (
	specSecret    = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
	specID        = "msg_p5jXN8AQM9LWM0D4loKWxJek"
	specTimestamp = 1614265330
	specPayload   = `{"test": 2432232314}`
	specSignature = "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="
)

var fixedNow = time.Unix(specTimestamp, 0)

func TestSignMatchesTheSpecification(t *testing.T) {
	t.Parallel()
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(specSecret, webhook.SecretPrefix))
	if err != nil {
		t.Fatal(err)
	}
	if got := webhook.Sign(key, specID, specTimestamp, []byte(specPayload)); got != specSignature {
		t.Errorf("Sign = %q, want %q", got, specSignature)
	}
}

// received is one request the test server got.
type received struct {
	header http.Header
	body   []byte
}

// server is an httptest TLS server answering with the queued statuses
// (then 204), recording what it receives.
type server struct {
	*httptest.Server
	mu       sync.Mutex
	got      []received
	statuses []int
	headers  []http.Header
}

func newServer(t *testing.T, statuses ...int) *server {
	t.Helper()
	s := &server{statuses: statuses}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.got = append(s.got, received{header: r.Header.Clone(), body: b})
		st, h := http.StatusNoContent, http.Header(nil)
		if len(s.statuses) > 0 {
			st, s.statuses = s.statuses[0], s.statuses[1:]
		}
		if len(s.headers) > 0 {
			h, s.headers = s.headers[0], s.headers[1:]
		}
		s.mu.Unlock()
		for k, v := range h {
			w.Header()[k] = v
		}
		w.WriteHeader(st)
		_, _ = w.Write([]byte(`{"error":"busy, see https://status.example/secret"}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) requests() []received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]received(nil), s.got...)
}

// client is a netguard client (mode any: the server is on loopback) that
// trusts the test server.
func client(t *testing.T, s *httptest.Server) *http.Client {
	t.Helper()
	c := netguard.Policy{Mode: netguard.ModeAny}.HTTPClient(5 * time.Second)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatal("not an *http.Transport")
	}
	pool := x509.NewCertPool()
	pool.AddCert(s.Certificate())
	tr.TLSClientConfig.RootCAs = pool
	return c
}

func cfg(url string, secrets map[string]string, format string) descriptor.Values {
	v := descriptor.Values{Options: map[string]string{}, Secrets: map[string]string{webhook.FieldURL: url}}
	for k, s := range secrets {
		v.Secrets[k] = s
	}
	if format != "" {
		v.Options[webhook.FieldFormat] = format
	}
	return v
}

// verify is a receiver's Standard Webhooks check, written from the
// specification.
func verify(t *testing.T, secret string, r received) {
	t.Helper()
	id, ts, sig := r.header.Get("webhook-id"), r.header.Get("webhook-timestamp"), r.header.Get("webhook-signature")
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "." + string(r.body)))
	want := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(want)) {
		t.Errorf("signature %q does not verify (want %q)", sig, want)
	}
	if n, err := strconv.ParseInt(ts, 10, 64); err != nil || n != specTimestamp {
		t.Errorf("webhook-timestamp = %q", ts)
	}
}

func TestSendSignedSyncrFormat(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	ch := webhook.New(client(t, s.Server), webhook.WithClock(func() time.Time { return fixedNow }))
	conf := cfg(s.URL+"/hook?token=abc", map[string]string{
		webhook.FieldSigningSecret: specSecret, webhook.FieldAuthorization: "Bearer tok-123",
	}, "")
	if err := ch.Validate(conf); err != nil {
		t.Fatal(err)
	}
	msg := notifytest.SampleMessage()
	if err := ch.Send(context.Background(), conf, msg); err != nil {
		t.Fatal(err)
	}
	reqs := s.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	r := reqs[0]
	verify(t, specSecret, r)
	if r.header.Get("webhook-id") != msg.ID || r.header.Get("Authorization") != "Bearer tok-123" ||
		r.header.Get("Content-Type") != "application/json" {
		t.Errorf("headers = %v", r.header)
	}
	var got notify.Message
	if err := json.Unmarshal(r.body, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != msg.ID || got.Event != domain.EventFailed || got.Job == nil || got.Job.WorkerName != "nas-01" || got.Title != msg.Title {
		t.Errorf("body = %s", r.body)
	}

	// The same secret as raw bytes (not whsec_) signs with those bytes.
	raw := "a-raw-signing-secret-of-32-bytes"
	conf.Secrets[webhook.FieldSigningSecret] = raw
	if err := ch.Send(context.Background(), conf, msg); err != nil {
		t.Fatal(err)
	}
	r = s.requests()[1]
	if want := webhook.Sign([]byte(raw), msg.ID, specTimestamp, r.body); r.header.Get("webhook-signature") != want {
		t.Errorf("raw-key signature = %q, want %q", r.header.Get("webhook-signature"), want)
	}

	// Unsigned: no signature header.
	delete(conf.Secrets, webhook.FieldSigningSecret)
	if err := ch.Send(context.Background(), conf, msg); err != nil {
		t.Fatal(err)
	}
	if r := s.requests()[2]; r.header.Get("webhook-signature") != "" || r.header.Get("webhook-id") == "" {
		t.Errorf("unsigned headers = %v", r.header)
	}
}

func TestRetryKeepsTheWebhookID(t *testing.T) {
	t.Parallel()
	s := newServer(t, http.StatusServiceUnavailable)
	ch := webhook.New(client(t, s.Server), webhook.WithClock(func() time.Time { return fixedNow }))
	conf := cfg(s.URL+"/hook", map[string]string{webhook.FieldSigningSecret: specSecret}, "")
	msg := notifytest.SampleMessage()

	err := ch.Send(context.Background(), conf, msg)
	var serr *notify.SendError
	if !errors.As(err, &serr) || serr.Permanent || serr.Status != http.StatusServiceUnavailable {
		t.Fatalf("first send = %v, want a temporary 503", err)
	}
	if strings.Contains(serr.Reason, "status.example") {
		t.Errorf("the reason quotes a URL: %q", serr.Reason)
	}
	if err := ch.Send(context.Background(), conf, msg); err != nil {
		t.Fatalf("retry = %v", err)
	}
	reqs := s.requests()
	if len(reqs) != 2 || reqs[0].header.Get("webhook-id") != msg.ID || reqs[1].header.Get("webhook-id") != msg.ID {
		t.Errorf("webhook-ids = %q, %q; want %q twice", reqs[0].header.Get("webhook-id"), reqs[1].header.Get("webhook-id"), msg.ID)
	}
	for _, r := range reqs {
		verify(t, specSecret, r)
	}
}

func TestReplies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status     int
		header     http.Header
		permanent  bool
		retryAfter time.Duration
	}{
		{http.StatusTooManyRequests, http.Header{"Retry-After": {"120"}}, false, 2 * time.Minute},
		{http.StatusTooManyRequests, http.Header{"Retry-After": {fixedNow.Add(30 * time.Second).UTC().Format(http.TimeFormat)}}, false, 30 * time.Second},
		{http.StatusTooManyRequests, http.Header{"Retry-After": {"999999"}}, false, time.Hour},
		{http.StatusServiceUnavailable, http.Header{"Retry-After": {"5"}}, false, 5 * time.Second},
		{http.StatusInternalServerError, nil, false, 0},
		{http.StatusRequestTimeout, nil, false, 0},
		{http.StatusTooEarly, nil, false, 0},
		{http.StatusBadRequest, nil, true, 0},
		{http.StatusUnauthorized, nil, true, 0},
		{http.StatusNotFound, nil, true, 0},
		{http.StatusFound, http.Header{"Location": {"https://elsewhere.example/"}}, true, 0},
	}
	for _, tt := range tests {
		s := newServer(t, tt.status)
		s.headers = []http.Header{tt.header}
		ch := webhook.New(client(t, s.Server), webhook.WithClock(func() time.Time { return fixedNow }))
		err := ch.Send(context.Background(), cfg(s.URL, nil, ""), notifytest.SampleMessage())
		var serr *notify.SendError
		if !errors.As(err, &serr) {
			t.Errorf("%d: %v is not a SendError", tt.status, err)
			continue
		}
		if serr.Permanent != tt.permanent || serr.Status != tt.status || serr.RetryAfter != tt.retryAfter {
			t.Errorf("%d: %+v, want permanent=%v retry after %v", tt.status, serr, tt.permanent, tt.retryAfter)
		}
		if n := len(s.requests()); n != 1 {
			t.Errorf("%d: %d requests (a redirect was followed?)", tt.status, n)
		}
	}
}

func TestSlackFormat(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	ch := webhook.New(client(t, s.Server))
	msg := notifytest.SampleMessage()
	msg.Schedule.Name = "<!channel> & co"
	msg.Title = notify.Title(msg)
	if err := ch.Send(context.Background(), cfg(s.URL, nil, webhook.FormatSlack), msg); err != nil {
		t.Fatal(err)
	}
	var body struct{ Text string }
	if err := json.Unmarshal(s.requests()[0].body, &body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body.Text, "<!channel>") || !strings.Contains(body.Text, "&lt;!channel&gt; &amp; co") ||
		!strings.Contains(body.Text, "\nWorker: nas-01") || !strings.HasSuffix(body.Text, "|Open in Syncr>") {
		t.Errorf("text = %q", body.Text)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	ch := webhook.New(http.DefaultClient)
	tests := []struct {
		name    string
		secrets map[string]string
		format  string
		field   string
	}{
		{"ftp", map[string]string{webhook.FieldURL: "ftp://example.com/x"}, "", "secrets.url"},
		{"no host", map[string]string{webhook.FieldURL: "https:///x"}, "", "secrets.url"},
		{"fragment", map[string]string{webhook.FieldURL: "https://example.com/x#frag"}, "", "secrets.url"},
		{"short secret", map[string]string{webhook.FieldURL: "https://example.com/x", webhook.FieldSigningSecret: "short"}, "", "secrets.signing_secret"},
		{"bad base64", map[string]string{webhook.FieldURL: "https://example.com/x", webhook.FieldSigningSecret: "whsec_***not-base64***"}, "", "secrets.signing_secret"},
		{"short whsec", map[string]string{webhook.FieldURL: "https://example.com/x", webhook.FieldSigningSecret: "whsec_c2hvcnQ="}, "", "secrets.signing_secret"},
		{"header injection", map[string]string{webhook.FieldURL: "https://example.com/x", webhook.FieldAuthorization: "Bearer x\r\nX-Evil: 1"}, "", "secrets.authorization"},
		{"format", map[string]string{webhook.FieldURL: "https://example.com/x"}, "teams", "options.format"},
	}
	for _, tt := range tests {
		err := ch.Validate(cfg("", tt.secrets, tt.format))
		var verr *domain.ValidationError
		if !errors.As(err, &verr) {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		found := false
		for _, f := range verr.Fields {
			found = found || f.Field == tt.field
		}
		if !found {
			t.Errorf("%s: fields %+v, want %s", tt.name, verr.Fields, tt.field)
		}
	}
	if got := ch.Summary(cfg("https://hooks.example:8443/p/secret-token", map[string]string{webhook.FieldSigningSecret: specSecret}, webhook.FormatSlack)); got != "hooks.example · Slack format · signed" {
		t.Errorf("Summary = %q", got)
	}
}

// TestNetworkPolicy: with the default policy a webhook to loopback fails
// permanently, without quoting the URL.
func TestNetworkPolicy(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	ch := webhook.New(netguard.Policy{}.HTTPClient(5 * time.Second))
	url := s.URL + "/hook/sentinel-token-xyz"
	err := ch.Send(context.Background(), cfg(url, nil, ""), notifytest.SampleMessage())
	var serr *notify.SendError
	if !errors.As(err, &serr) || !serr.Permanent || !strings.Contains(serr.Reason, "network policy") {
		t.Fatalf("send to loopback = %v", err)
	}
	if strings.Contains(err.Error(), "sentinel-token-xyz") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("the error quotes the target: %v", err)
	}
	if len(s.requests()) != 0 {
		t.Error("the request reached the server")
	}
}

func TestContract(t *testing.T) {
	t.Parallel()
	s := newServer(t, http.StatusInternalServerError, http.StatusInternalServerError)
	ch := webhook.New(client(t, s.Server))
	notifytest.Run(t, ch, notifytest.WithFailingTarget(cfg(s.URL+"/hook/notifytest-failing-token", map[string]string{
		webhook.FieldSigningSecret: "notifytest-failing-signing-secret",
		webhook.FieldAuthorization: "Bearer notifytest-failing-bearer",
	}, "")))
}
