// Package webhook is the generic webhook notification channel. It POSTs
// each notification as JSON, signed per the Standard Webhooks
// specification (https://www.standardwebhooks.com), so receivers can verify
// it with an existing library:
//
//	webhook-id:        the delivery ID, the same on every retry (dedupe on it)
//	webhook-timestamp: Unix seconds of the attempt
//	webhook-signature: v1,<base64 HMAC-SHA256(key, id + "." + timestamp + "." + body)>
//
// The key is the signing secret: "whsec_<base64>" is decoded (the
// specification's format), any other value of at least 24 bytes is used as
// is. Without a signing secret the signature header is omitted.
//
// Format "syncr" sends the notify.Message JSON (see docs/notifications.md);
// format "slack" sends {"text": ...}, which Slack, Mattermost and
// Rocket.Chat incoming webhooks accept.
//
// Replies: 2xx is success; 408, 425, 429 and 5xx are temporary (with
// Retry-After honored); redirects and other 4xx are permanent.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/notify/internal/sendutil"
	"github.com/Zeptiny/Syncr/internal/descriptor"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/notify"
)

// Field names.
const (
	FieldURL           = "url"
	FieldSigningSecret = "signing_secret"
	FieldAuthorization = "authorization"
	FieldFormat        = "format"
)

// Formats.
const (
	FormatSyncr = "syncr"
	FormatSlack = "slack"
)

// SecretPrefix starts a Standard Webhooks secret.
const SecretPrefix = "whsec_"

// Signing secret lengths, in bytes (of the decoded key for "whsec_").
const (
	minSecretBytes = 24
	maxSecretBytes = 512
)

// what names the target in reasons.
const what = "the webhook"

// Channel is the webhook channel. It is safe for concurrent use.
type Channel struct {
	client *http.Client
	now    func() time.Time
}

// Option configures New.
type Option func(*Channel)

// WithClock sets the clock of the webhook-timestamp header (tests).
func WithClock(now func() time.Time) Option { return func(c *Channel) { c.now = now } }

// New returns the channel, sending with client (netguard's HTTPClient in
// production: no redirects, no proxy, checked addresses).
func New(client *http.Client, opts ...Option) *Channel {
	c := &Channel{client: client, now: time.Now}
	for _, o := range opts {
		o(c)
	}
	return c
}

var (
	_ notify.Channel  = (*Channel)(nil)
	_ notify.Exampler = (*Channel)(nil)
)

// ID implements notify.Channel.
func (*Channel) ID() string { return "webhook" }

// Label implements notify.Channel.
func (*Channel) Label() string { return "Webhook" }

// Description implements notify.Channel.
func (*Channel) Description() string {
	return "POST a signed JSON message (Standard Webhooks) to any URL, or a Slack-compatible message to Slack, Mattermost or Rocket.Chat."
}

// Fields implements notify.Channel.
func (*Channel) Fields() []descriptor.Field {
	return []descriptor.Field{
		{Name: FieldURL, Label: "URL", Kind: descriptor.KindString, Required: true, Secret: true,
			Placeholder: "https://hooks.example.com/syncr",
			Help:        "Where to POST. Kept secret because webhook URLs often contain a token."},
		{Name: FieldFormat, Label: "Format", Kind: descriptor.KindEnum, Default: FormatSyncr,
			Choices: []descriptor.Choice{
				{Value: FormatSyncr, Label: "Syncr JSON (signed, documented payload)"},
				{Value: FormatSlack, Label: "Slack-compatible text (Slack, Mattermost, Rocket.Chat)"},
			}},
		{Name: FieldSigningSecret, Label: "Signing secret", Kind: descriptor.KindString, Secret: true,
			Placeholder: "whsec_…",
			Help:        "Signs each request (Standard Webhooks webhook-signature header): whsec_ followed by base64, or any string of at least 24 characters."},
		{Name: FieldAuthorization, Label: "Authorization header", Kind: descriptor.KindString, Secret: true, Advanced: true,
			Placeholder: "Bearer …", Help: "Sent as the Authorization header, e.g. a bearer token."},
	}
}

// Examples implements notify.Exampler.
func (*Channel) Examples() []descriptor.Values {
	return []descriptor.Values{
		{
			Options: map[string]string{FieldFormat: FormatSyncr},
			Secrets: map[string]string{ //nolint:gosec // examples for the contract suite, not credentials
				FieldURL:           "https://ha.example.lan:8123/api/webhook/syncr-token-1234",
				FieldSigningSecret: "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw",
				FieldAuthorization: "Bearer example-bearer-token",
			},
		},
		{
			Options: map[string]string{FieldFormat: FormatSlack},
			Secrets: map[string]string{FieldURL: "https://hooks.slack.example/services/T000/B000/slack-secret-path"},
		},
		{Secrets: map[string]string{FieldURL: "http://192.168.1.20:5678/webhook/plain-path"}},
	}
}

// Validate implements notify.Channel.
func (c *Channel) Validate(cfg descriptor.Values) error {
	var v domain.Validation
	for _, fe := range descriptor.CheckFields(c.Fields(), cfg) {
		v.Add(fe.Field, fe.Message)
	}
	if raw := cfg.Secrets[FieldURL]; raw != "" {
		if _, msg := parseURL(raw); msg != "" {
			v.Add("secrets."+FieldURL, msg)
		}
	}
	if s := cfg.Secrets[FieldSigningSecret]; s != "" {
		if _, msg := signingKey(s); msg != "" {
			v.Add("secrets."+FieldSigningSecret, msg)
		}
	}
	return v.Err()
}

// parseURL checks a webhook URL: http or https with a host, no fragment.
func parseURL(raw string) (*url.URL, string) {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return nil, "is not a valid URL"
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, "must be an http or https URL"
	case u.Hostname() == "":
		return nil, "has no host"
	case u.Fragment != "" || u.Opaque != "":
		return nil, "must not contain a fragment"
	}
	return u, ""
}

// signingKey returns the HMAC key of a signing secret.
func signingKey(s string) ([]byte, string) {
	if rest, ok := strings.CutPrefix(s, SecretPrefix); ok {
		key, err := base64.StdEncoding.DecodeString(rest)
		if err != nil {
			return nil, "must be whsec_ followed by base64"
		}
		if len(key) < minSecretBytes || len(key) > maxSecretBytes {
			return nil, fmt.Sprintf("must decode to %d to %d bytes", minSecretBytes, maxSecretBytes)
		}
		return key, ""
	}
	if len(s) < minSecretBytes || len(s) > maxSecretBytes {
		return nil, fmt.Sprintf("must be %d to %d characters (or whsec_ followed by base64)", minSecretBytes, maxSecretBytes)
	}
	return []byte(s), ""
}

// Summary implements notify.Channel: the host (never the path or query,
// which may hold a token), the format and whether requests are signed.
func (*Channel) Summary(cfg descriptor.Values) string {
	host := "webhook"
	if u, msg := parseURL(cfg.Secrets[FieldURL]); msg == "" {
		host = u.Hostname()
	}
	parts := []string{host}
	if cfg.Options[FieldFormat] == FormatSlack {
		parts = append(parts, "Slack format")
	}
	if cfg.Secrets[FieldSigningSecret] != "" {
		parts = append(parts, "signed")
	}
	return strings.Join(parts, " · ")
}

// Send implements notify.Channel.
func (c *Channel) Send(ctx context.Context, cfg descriptor.Values, msg notify.Message) error {
	if err := ctx.Err(); err != nil {
		return sendutil.TransportError(err, what)
	}
	if msg.ID == "" {
		msg.ID = randomID()
	}
	if msg.Title == "" {
		msg.Title = notify.Title(msg)
	}
	body, err := Body(cfg.Options[FieldFormat], msg)
	if err != nil {
		return notify.Public(notify.Permanent(0, "the message cannot be encoded"))
	}
	ts := c.now().Unix()
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "Syncr-Webhook/1")
	h.Set("webhook-id", msg.ID)
	h.Set("webhook-timestamp", strconv.FormatInt(ts, 10))
	if s := cfg.Secrets[FieldSigningSecret]; s != "" {
		key, bad := signingKey(s)
		if bad != "" {
			return notify.Public(notify.Permanent(0, "the signing secret "+bad))
		}
		h.Set("webhook-signature", Sign(key, msg.ID, ts, body))
	}
	if a := cfg.Secrets[FieldAuthorization]; a != "" {
		h.Set("Authorization", a)
	}
	resp, err := sendutil.Post(ctx, c.client, cfg.Secrets[FieldURL], h, body, what)
	if err != nil {
		return err
	}
	return Classify(resp, c.now(), notify.NewRedactor(cfg))
}

// Classify maps a webhook reply to nil or a *notify.SendError. The excerpt
// of the reply's body that goes into the reason is redacted with r (the
// destination's secrets, see notify.NewRedactor; may be nil): receivers
// often echo the request path, which holds the webhook's token.
func Classify(resp sendutil.Response, now time.Time, r *notify.Redactor) error {
	st := resp.Status
	switch {
	case st >= 200 && st < 300:
		return nil
	case st == http.StatusRequestTimeout || st == http.StatusTooEarly || st == http.StatusTooManyRequests || st >= 500:
		return notify.Temporary(st, sendutil.StatusReason(what, st, sendutil.Snippet(resp.Body, r)), sendutil.RetryAfter(resp.Header, now))
	case st >= 300 && st < 400:
		return notify.Permanent(st, sendutil.StatusReason(what, st, "redirects are not followed; use the final URL"))
	default:
		return notify.Permanent(st, sendutil.StatusReason(what, st, sendutil.Snippet(resp.Body, r)))
	}
}

// Sign returns the Standard Webhooks signature header value of a request:
// "v1," + base64(HMAC-SHA256(key, id + "." + timestamp + "." + body)).
func Sign(key []byte, id string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + strconv.FormatInt(timestamp, 10) + "."))
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// slackBody is the "slack" format.
type slackBody struct {
	Text string `json:"text"`
}

// Body encodes msg in format ("" means FormatSyncr).
func Body(format string, msg notify.Message) ([]byte, error) {
	if format != FormatSlack {
		b, err := json.Marshal(msg)
		if err != nil {
			return nil, fmt.Errorf("encode message: %w", err)
		}
		return b, nil
	}
	var b strings.Builder
	b.WriteString("*" + notify.EscapeSlack(msg.Title) + "*")
	if msg.Test {
		b.WriteString("\n" + notify.TestText)
	}
	for _, f := range notify.Facts(msg) {
		b.WriteString("\n" + f.Label + ": " + notify.EscapeSlack(f.Value))
	}
	if u, bad := parseURL(msg.URL); bad == "" && msg.URL != "" {
		// "<url|text>" is a link; the URL is ours (http.public_url).
		b.WriteString("\n<" + strings.NewReplacer("|", "%7C", ">", "%3E").Replace(u.String()) + "|Open in Syncr>")
	}
	out, err := json.Marshal(slackBody{Text: b.String()})
	if err != nil {
		return nil, fmt.Errorf("encode message: %w", err)
	}
	return out, nil
}

// randomID returns a delivery ID for a message without one.
func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "msg_" + hex.EncodeToString(b)
}
