// Package discord is the Discord notification channel: it posts an embed
// to a channel webhook (https://discord.com/developers/docs/resources/webhook).
//
// Names in a notification are users' text, so they are markdown-escaped
// and the request sets allowed_mentions {"parse": []}: a schedule named
// "@everyone" cannot ping anyone.
//
// Replies: 2xx is success; 429 is temporary after Retry-After (or the
// body's retry_after); 400, 401, 403 and 404 (a deleted webhook, a bad
// token) and other 4xx are permanent; 5xx is temporary.
package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/adapters/notify/internal/sendutil"
	"github.com/Zeptiny/Syncr/internal/descriptor"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/notify"
)

// Field names.
const (
	FieldWebhookURL = "webhook_url"
	FieldUsername   = "username"
)

// Discord's limits (embeds and webhook usernames).
const (
	maxTitle       = 256
	maxDescription = 4096
	maxFieldValue  = 1024
	maxFields      = 25
	maxUsername    = 80
)

// what names the target in reasons.
const what = "Discord"

// pathRe matches a webhook path: /api/webhooks/<id>/<token>.
var pathRe = regexp.MustCompile(`^/api/webhooks/\d+/[\w-]+$`)

// digitsRe matches a snowflake ID.
var digitsRe = regexp.MustCompile(`^\d+$`)

// hosts are Discord's webhook hosts.
func hosts() []string {
	return []string{"discord.com", "discordapp.com", "ptb.discord.com", "canary.discord.com"}
}

// Channel is the Discord channel. It is safe for concurrent use.
type Channel struct {
	client *http.Client
	base   *url.URL
	now    func() time.Time
}

// Option configures New.
type Option func(*Channel)

// WithBaseURL sends to base (scheme and host) instead of the webhook URL's
// Discord host, keeping its path. Tests only: it points the channel at a
// test server.
func WithBaseURL(base *url.URL) Option { return func(c *Channel) { c.base = base } }

// New returns the channel, sending with client (netguard's HTTPClient in
// production).
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
func (*Channel) ID() string { return "discord" }

// Label implements notify.Channel.
func (*Channel) Label() string { return "Discord" }

// Description implements notify.Channel.
func (*Channel) Description() string {
	return "Post to a Discord channel through a webhook (Channel settings → Integrations → Webhooks)."
}

// Fields implements notify.Channel.
func (*Channel) Fields() []descriptor.Field {
	return []descriptor.Field{
		{Name: FieldWebhookURL, Label: "Webhook URL", Kind: descriptor.KindString, Required: true, Secret: true,
			Placeholder: "https://discord.com/api/webhooks/…",
			Help:        "Copy it from the channel's Integrations settings. It is a credential: anyone with it can post."},
		{Name: FieldUsername, Label: "Bot name", Kind: descriptor.KindString, Advanced: true, Placeholder: "Syncr",
			Help: "Overrides the webhook's name for these messages."},
	}
}

// Examples implements notify.Exampler.
func (*Channel) Examples() []descriptor.Values {
	return []descriptor.Values{
		{
			Options: map[string]string{FieldUsername: "Backups"},
			Secrets: map[string]string{FieldWebhookURL: "https://discord.com/api/webhooks/123456789012345678/example-Webhook_token-abc"},
		},
		{Secrets: map[string]string{FieldWebhookURL: "https://canary.discord.com/api/webhooks/42/another-token"}},
	}
}

// Validate implements notify.Channel.
func (c *Channel) Validate(cfg descriptor.Values) error {
	var v domain.Validation
	for _, fe := range descriptor.CheckFields(c.Fields(), cfg) {
		v.Add(fe.Field, fe.Message)
	}
	if raw := cfg.Secrets[FieldWebhookURL]; raw != "" {
		if _, msg := parseWebhook(raw); msg != "" {
			v.Add("secrets."+FieldWebhookURL, msg)
		}
	}
	if name := cfg.Options[FieldUsername]; name != "" {
		switch lower := strings.ToLower(name); {
		case utf8.RuneCountInString(name) > maxUsername:
			v.Add("options."+FieldUsername, "must be at most 80 characters")
		case strings.Contains(lower, "discord") || strings.Contains(lower, "clyde"):
			v.Add("options."+FieldUsername, `must not contain "discord" or "clyde" (Discord rejects it)`)
		}
	}
	return v.Err()
}

// parseWebhook checks a Discord webhook URL: https on a Discord host with
// the path /api/webhooks/<id>/<token>, optionally ?thread_id=<id>.
func parseWebhook(raw string) (*url.URL, string) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Fragment != "" {
		return nil, "must be a Discord webhook URL (https://discord.com/api/webhooks/…)"
	}
	host := strings.ToLower(u.Hostname())
	ok := false
	for _, h := range hosts() {
		ok = ok || host == h
	}
	if !ok {
		return nil, "must be on discord.com"
	}
	if !pathRe.MatchString(u.EscapedPath()) {
		return nil, "must have the path /api/webhooks/<id>/<token>"
	}
	q := u.Query()
	for k, vs := range q {
		if k != "thread_id" || len(vs) != 1 || !digitsRe.MatchString(vs[0]) {
			return nil, "may only have a thread_id query parameter"
		}
	}
	return u, ""
}

// Summary implements notify.Channel: "<host> webhook".
func (*Channel) Summary(cfg descriptor.Values) string {
	if u, msg := parseWebhook(cfg.Secrets[FieldWebhookURL]); msg == "" {
		return strings.ToLower(u.Hostname()) + " webhook"
	}
	return "Discord webhook"
}

// Payload is the webhook request body.
type Payload struct {
	Username        string          `json:"username,omitempty"`
	Embeds          []Embed         `json:"embeds"`
	AllowedMentions AllowedMentions `json:"allowed_mentions"`
}

// AllowedMentions is Discord's mention filter; an empty Parse pings no one.
type AllowedMentions struct {
	Parse []string `json:"parse"`
}

// Embed is one rich embed.
type Embed struct {
	Title       string       `json:"title"`
	Description string       `json:"description,omitempty"`
	URL         string       `json:"url,omitempty"`
	Color       int          `json:"color"`
	Fields      []EmbedField `json:"fields,omitempty"`
	Timestamp   string       `json:"timestamp,omitempty"`
	Footer      *EmbedFooter `json:"footer,omitempty"`
}

// EmbedField is one name/value pair of an embed.
type EmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

// EmbedFooter is the small text under an embed.
type EmbedFooter struct {
	Text string `json:"text"`
}

// Colors by event (GitHub Primer palette).
const (
	colorSucceeded   = 0x1A7F37
	colorFailed      = 0xCF222E
	colorInterrupted = 0xBF8700
	colorMissed      = 0x8250DF
	colorInfo        = 0x0969DA
)

func color(e domain.NotificationEvent) int {
	switch e {
	case domain.EventSucceeded:
		return colorSucceeded
	case domain.EventFailed:
		return colorFailed
	case domain.EventInterrupted:
		return colorInterrupted
	case domain.EventMissed:
		return colorMissed
	default:
		return colorInfo
	}
}

// inline reports whether a fact is short enough to sit beside others.
func inline(label string) bool {
	switch label {
	case "Worker", "Kind", "Status", "Transferred", "Errors", "Duration":
		return true
	}
	return false
}

// BuildPayload returns the request body of msg.
func BuildPayload(msg notify.Message, username string) Payload {
	title := msg.Title
	if title == "" {
		title = notify.Title(msg)
	}
	e := Embed{
		Title:  cut(notify.EscapeMarkdown(title), maxTitle),
		Color:  color(msg.Event),
		Footer: &EmbedFooter{Text: "Syncr"},
	}
	if msg.Test {
		e.Description = cut(notify.TestText, maxDescription)
	}
	if u, err := url.Parse(msg.URL); err == nil && (u.Scheme == "https" || u.Scheme == "http") {
		e.URL = msg.URL
	}
	if !msg.OccurredAt.IsZero() {
		e.Timestamp = msg.OccurredAt.UTC().Format(time.RFC3339)
	}
	for _, f := range notify.Facts(msg) {
		if len(e.Fields) == maxFields {
			break
		}
		e.Fields = append(e.Fields, EmbedField{Name: f.Label, Value: cut(notify.EscapeMarkdown(f.Value), maxFieldValue), Inline: inline(f.Label)})
	}
	return Payload{Username: username, Embeds: []Embed{e}, AllowedMentions: AllowedMentions{Parse: []string{}}}
}

// cut truncates s to n runes, ending in "…" when cut.
func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// Send implements notify.Channel.
func (c *Channel) Send(ctx context.Context, cfg descriptor.Values, msg notify.Message) error {
	if err := ctx.Err(); err != nil {
		return sendutil.TransportError(err, what)
	}
	u, bad := parseWebhook(cfg.Secrets[FieldWebhookURL])
	if bad != "" {
		return notify.Permanent(0, "the webhook URL "+bad)
	}
	target := *u
	if c.base != nil {
		target.Scheme, target.Host = c.base.Scheme, c.base.Host
	}
	q := target.Query()
	q.Set("wait", "true")
	target.RawQuery = q.Encode()
	body, err := json.Marshal(BuildPayload(msg, cfg.Options[FieldUsername]))
	if err != nil {
		return notify.Permanent(0, "the message cannot be encoded")
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "Syncr (https://github.com/Zeptiny/Syncr, 1)")
	resp, err := sendutil.Post(ctx, c.client, target.String(), h, body, what)
	if err != nil {
		return err
	}
	return classify(resp, c.now(), notify.NewRedactor(cfg))
}

// errorBody is Discord's JSON error reply.
type errorBody struct {
	Message    string  `json:"message"`
	Code       int     `json:"code"`
	RetryAfter float64 `json:"retry_after"`
}

// classify maps Discord's reply to nil or a *notify.SendError. Discord's
// error message is redacted with r (the webhook URL carries the token in
// its path) before it is quoted.
func classify(resp sendutil.Response, now time.Time, r *notify.Redactor) error {
	st := resp.Status
	if st >= 200 && st < 300 {
		return nil
	}
	var eb errorBody
	_ = json.Unmarshal(resp.Body, &eb)
	detail := sendutil.Snippet([]byte(eb.Message), r)
	switch {
	case st == http.StatusTooManyRequests:
		retry := sendutil.RetryAfter(resp.Header, now)
		if retry == 0 {
			retry = sendutil.Seconds(eb.RetryAfter)
		}
		return notify.Temporary(st, sendutil.StatusReason(what, st, "rate limited"), retry)
	case st >= 500:
		return notify.Temporary(st, sendutil.StatusReason(what, st, detail), 0)
	case st == http.StatusNotFound:
		return notify.Permanent(st, sendutil.StatusReason(what, st, "the webhook does not exist (deleted?)"))
	case st == http.StatusUnauthorized || st == http.StatusForbidden:
		return notify.Permanent(st, sendutil.StatusReason(what, st, "the webhook token is not valid"))
	default:
		return notify.Permanent(st, sendutil.StatusReason(what, st, detail))
	}
}
