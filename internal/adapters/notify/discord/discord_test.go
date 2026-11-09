package discord_test

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/notify/discord"
	"github.com/Zeptiny/Syncr/internal/descriptor"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/notify"
	"github.com/Zeptiny/Syncr/internal/notify/notifytest"
	"github.com/Zeptiny/Syncr/internal/platform/netguard"
)

const webhookURL = "https://discord.com/api/webhooks/123456789012345678/tok-EN_secret-42"

type request struct {
	path, query string
	header      http.Header
	body        []byte
}

// fakeDiscord is a TLS test server answering with reply.
type fakeDiscord struct {
	srv   *httptest.Server
	mu    sync.Mutex
	got   []request
	reply func(w http.ResponseWriter)
}

func newFake(t *testing.T, reply func(w http.ResponseWriter)) (*fakeDiscord, *discord.Channel) {
	t.Helper()
	f := &fakeDiscord{reply: reply}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.got = append(f.got, request{path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone(), body: b})
		f.mu.Unlock()
		if f.reply == nil {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"1"}`))
			return
		}
		f.reply(w)
	}))
	t.Cleanup(f.srv.Close)
	c := netguard.Policy{Mode: netguard.ModeAny}.HTTPClient(5 * time.Second)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatal("not an *http.Transport")
	}
	pool := x509.NewCertPool()
	pool.AddCert(f.srv.Certificate())
	tr.TLSClientConfig.RootCAs = pool
	base, err := url.Parse(f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return f, discord.New(c, discord.WithBaseURL(base))
}

func (f *fakeDiscord) requests() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.got...)
}

func conf(u, username string) descriptor.Values {
	v := descriptor.Values{Secrets: map[string]string{discord.FieldWebhookURL: u}}
	if username != "" {
		v.Options = map[string]string{discord.FieldUsername: username}
	}
	return v
}

func TestRequestShape(t *testing.T) {
	t.Parallel()
	f, ch := newFake(t, nil)
	msg := notifytest.SampleMessage()
	msg.Schedule.Name = "@everyone **pwn** <@&1> `x`"
	msg.Title = notify.Title(msg)
	if err := ch.Send(context.Background(), conf(webhookURL+"?thread_id=99", "Backups"), msg); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	r := reqs[0]
	if r.path != "/api/webhooks/123456789012345678/tok-EN_secret-42" || r.query != "thread_id=99&wait=true" ||
		r.header.Get("Content-Type") != "application/json" {
		t.Errorf("request = %s?%s %v", r.path, r.query, r.header)
	}
	// The raw JSON: allowed_mentions parses nothing.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(r.body, &raw); err != nil {
		t.Fatal(err)
	}
	if got := string(raw["allowed_mentions"]); got != `{"parse":[]}` {
		t.Errorf("allowed_mentions = %s", got)
	}
	var p discord.Payload
	if err := json.Unmarshal(r.body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Username != "Backups" || len(p.Embeds) != 1 {
		t.Fatalf("payload = %+v", p)
	}
	e := p.Embeds[0]
	if want := `Job failed: \@everyone \*\*pwn\*\* \<\@&1\> \` + "`x\\`"; e.Title != want {
		t.Errorf("title = %q, want %q", e.Title, want)
	}
	if e.URL != msg.URL || e.Color != 0xCF222E || e.Timestamp != "2026-11-04T02:31:07Z" || e.Footer == nil {
		t.Errorf("embed = %+v", e)
	}
	fields := map[string]discord.EmbedField{}
	for _, fl := range e.Fields {
		fields[fl.Name] = fl
	}
	if fields["Worker"].Value != "nas-01" || !fields["Worker"].Inline || fields["Error"].Inline ||
		!strings.HasPrefix(fields["Error"].Value, `path\_not\_allowed:`) {
		t.Errorf("fields = %+v", e.Fields)
	}
	if fields["Schedule"].Value != `\@everyone \*\*pwn\*\* \<\@&1\> \`+"`x\\`" {
		t.Errorf("schedule field = %q", fields["Schedule"].Value)
	}
}

func TestEmbedLimits(t *testing.T) {
	t.Parallel()
	msg := notifytest.SampleMessage()
	msg.Title = strings.Repeat("*", 300)
	msg.Job.ErrorMessage = strings.Repeat("_", 3000)
	p := discord.BuildPayload(msg, "")
	e := p.Embeds[0]
	if n := len([]rune(e.Title)); n > 256 {
		t.Errorf("title has %d runes", n)
	}
	for _, f := range e.Fields {
		if n := len([]rune(f.Value)); n > 1024 {
			t.Errorf("field %s has %d runes", f.Name, n)
		}
	}
	test := discord.BuildPayload(notify.Message{Event: domain.EventTest, Test: true}, "")
	if test.Embeds[0].Description != notify.TestText || test.Embeds[0].Color != 0x0969DA {
		t.Errorf("test embed = %+v", test.Embeds[0])
	}
}

func TestReplies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		reply      func(w http.ResponseWriter)
		status     int
		permanent  bool
		retryAfter time.Duration
	}{
		{"429 header", func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "2.5")
			w.WriteHeader(http.StatusTooManyRequests)
		}, 429, false, 2500 * time.Millisecond},
		{"429 body", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"You are being rate limited.","retry_after":1.25,"global":false}`))
		}, 429, false, 1250 * time.Millisecond},
		{"404", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Unknown Webhook","code":10015}`))
		}, 404, true, 0},
		{"401", func(w http.ResponseWriter) { w.WriteHeader(http.StatusUnauthorized) }, 401, true, 0},
		{"403", func(w http.ResponseWriter) { w.WriteHeader(http.StatusForbidden) }, 403, true, 0},
		{"400", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"Invalid Form Body","code":50035}`))
		}, 400, true, 0},
		{"502", func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) }, 502, false, 0},
		{"redirect", func(w http.ResponseWriter) {
			w.Header().Set("Location", "https://elsewhere.example/")
			w.WriteHeader(http.StatusFound)
		}, 302, true, 0},
	}
	for _, tt := range tests {
		f, ch := newFake(t, tt.reply)
		err := ch.Send(context.Background(), conf(webhookURL, ""), notifytest.SampleMessage())
		var serr *notify.SendError
		if !errors.As(err, &serr) {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if serr.Status != tt.status || serr.Permanent != tt.permanent || serr.RetryAfter != tt.retryAfter {
			t.Errorf("%s: %+v", tt.name, serr)
		}
		if strings.Contains(err.Error(), "tok-EN_secret-42") {
			t.Errorf("%s: the error contains the token: %v", tt.name, err)
		}
		if len(f.requests()) != 1 {
			t.Errorf("%s: %d requests", tt.name, len(f.requests()))
		}
	}
}

// TestErrorMessagesEchoingTheWebhookKeepTheTokenOut: the token is the last
// segment of the webhook URL; a reply that quotes the request must not put
// it into the reason.
func TestErrorMessagesEchoingTheWebhookKeepTheTokenOut(t *testing.T) {
	t.Parallel()
	for name, echo := range map[string]func(r request) string{
		"path":      func(r request) string { return "Invalid webhook at " + r.path },
		"id/token":  func(r request) string { return "Unknown webhook " + strings.TrimPrefix(r.path, "/api/webhooks/") },
		"lowercase": func(r request) string { return "Unknown path " + strings.ToLower(r.path) },
		"escaped":   func(r request) string { return strings.ReplaceAll(r.path, "/", `\/`) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var f *fakeDiscord
			f, ch := newFake(t, func(w http.ResponseWriter) {
				got := f.requests()
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": echo(got[len(got)-1]), "code": 50027})
			})
			err := ch.Send(context.Background(), conf(webhookURL, ""), notifytest.SampleMessage())
			var serr *notify.SendError
			if !errors.As(err, &serr) || serr.Status != http.StatusBadRequest {
				t.Fatalf("Send = %v, want a 400 SendError", err)
			}
			for _, secret := range []string{"tok-EN_secret-42", "tok-en_secret-42", "123456789012345678"} {
				if strings.Contains(serr.Reason, secret) {
					t.Errorf("reason %q contains %q", serr.Reason, secret)
				}
			}
		})
	}
}

// The destination is always Discord, so its failures say nothing about the
// controller's network and are shown to every user.
func TestFailuresArePublic(t *testing.T) {
	t.Parallel()
	_, ch := newFake(t, func(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) })
	err := ch.Send(context.Background(), conf(webhookURL, ""), notifytest.SampleMessage())
	var serr *notify.SendError
	if !errors.As(err, &serr) || !serr.Public {
		t.Errorf("Send = %v, want a Public SendError", err)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	ch := discord.New(http.DefaultClient)
	for _, ok := range []string{webhookURL, "https://discordapp.com/api/webhooks/1/a", "https://ptb.discord.com/api/webhooks/1/a-b_c", webhookURL + "?thread_id=5"} {
		if err := ch.Validate(conf(ok, "")); err != nil {
			t.Errorf("Validate(%s) = %v", ok, err)
		}
	}
	for _, bad := range []string{
		"http://discord.com/api/webhooks/1/a",
		"https://discord.com.evil.example/api/webhooks/1/a",
		"https://evil.example/api/webhooks/1/a",
		"https://discord.com:8443/api/webhooks/1/a",
		"https://user@discord.com/api/webhooks/1/a",
		"https://discord.com/api/webhooks/x/a",
		"https://discord.com/api/webhooks/1/a/github",
		"https://discord.com/api/v10/channels/1/messages",
		"https://discord.com/api/webhooks/1/a?wait=false",
		"not a url",
	} {
		if err := ch.Validate(conf(bad, "")); !errors.Is(err, domain.ErrInvalidArgument) || strings.Contains(err.Error(), bad) {
			t.Errorf("Validate(%s) = %v", bad, err)
		}
	}
	for _, name := range []string{"My Discord bot", "clyde", strings.Repeat("x", 81)} {
		if err := ch.Validate(conf(webhookURL, name)); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("Validate(username %q) = %v", name, err)
		}
	}
	if got := ch.Summary(conf(webhookURL, "")); got != "discord.com webhook" {
		t.Errorf("Summary = %q", got)
	}
}

func TestNetworkPolicy(t *testing.T) {
	t.Parallel()
	f, _ := newFake(t, nil)
	base, _ := url.Parse(f.srv.URL)
	ch := discord.New(netguard.Policy{}.HTTPClient(5*time.Second), discord.WithBaseURL(base))
	err := ch.Send(context.Background(), conf(webhookURL, ""), notifytest.SampleMessage())
	var serr *notify.SendError
	if !errors.As(err, &serr) || !serr.Permanent || len(f.requests()) != 0 {
		t.Errorf("send to loopback = %v", err)
	}
}

func TestContract(t *testing.T) {
	t.Parallel()
	_, ch := newFake(t, func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) })
	notifytest.Run(t, ch, notifytest.WithFailingTarget(conf("https://discord.com/api/webhooks/987/notifytest-failing-token", "")))
}
