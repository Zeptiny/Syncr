package notify_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/descriptor"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/notify"
)

type stubChannel struct {
	id, label string
	fields    []descriptor.Field
	sent      *int
}

func (s stubChannel) ID() string                       { return s.id }
func (s stubChannel) Label() string                    { return s.label }
func (s stubChannel) Description() string              { return "stub" }
func (s stubChannel) Fields() []descriptor.Field       { return s.fields }
func (s stubChannel) Validate(descriptor.Values) error { return nil }
func (s stubChannel) Summary(descriptor.Values) string { return "stub" }
func (s stubChannel) Examples() []descriptor.Values    { return []descriptor.Values{{}} }
func (s stubChannel) Send(context.Context, descriptor.Values, notify.Message) error {
	*s.sent++
	return nil
}

func stub(id, label string) stubChannel { return stubChannel{id: id, label: label, sent: new(int)} }

func TestRegistry(t *testing.T) {
	t.Parallel()
	r, err := notify.NewRegistry(stub("webhook", "Webhook"), stub("discord", "Discord"), stub("email", "Email"), stub("aaa", "Email"))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range r.Channels() {
		ids = append(ids, c.ID())
	}
	if strings.Join(ids, ",") != "discord,aaa,email,webhook" {
		t.Errorf("Channels() = %v, want by label then ID", ids)
	}
	if c, ok := r.Get("email"); !ok || c.ID() != "email" {
		t.Error("Get(email) failed")
	}
	if _, ok := r.Get("nope"); ok {
		t.Error("Get(nope) found something")
	}

	bad := map[string][]notify.Channel{
		"nil":         {nil},
		"duplicate":   {stub("a", "A"), stub("a", "B")},
		"bad id":      {stub("Discord", "D")},
		"long id":     {stub(strings.Repeat("a", 33), "A")},
		"underscore":  {stub("a_b", "A")},
		"empty label": {stub("a", "")},
		"bad field":   {stubChannel{id: "a", label: "A", fields: []descriptor.Field{{Name: "X", Label: "X", Kind: descriptor.KindString}}}},
	}
	for name, chs := range bad {
		if _, err := notify.NewRegistry(chs...); err == nil {
			t.Errorf("%s: NewRegistry accepted it", name)
		}
	}
}

func TestSendErrors(t *testing.T) {
	t.Parallel()
	perm := notify.Permanent(404, "unknown webhook")
	var serr *notify.SendError
	if !errors.As(perm, &serr) || !serr.Permanent || serr.Status != 404 || serr.Reason != "unknown webhook" {
		t.Errorf("Permanent = %#v", perm)
	}
	if !strings.Contains(perm.Error(), "404") || !strings.Contains(perm.Error(), "permanent") {
		t.Errorf("Error() = %q", perm.Error())
	}
	temp := notify.Temporary(0, "connection refused", -time.Second)
	if !errors.As(temp, &serr) || serr.Permanent || serr.RetryAfter != 0 || strings.Contains(temp.Error(), "status") {
		t.Errorf("Temporary = %#v (%q)", temp, temp.Error())
	}
	if errors.As(notify.Temporary(429, "slow down", time.Minute), &serr); serr.RetryAfter != time.Minute {
		t.Errorf("RetryAfter = %v", serr.RetryAfter)
	}
}

func TestSanitizeReason(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{`Post "https://discord.com/api/webhooks/1/TOKEN": dial tcp: refused`, `Post "<url> dial tcp: refused`},
		{"line1\nline2\r\n\tline3\x00", "line1 line2 line3"},
		{"smtp://user:pass@host failed", "<url> failed"},
		{"see (http://x/y) now", "see (<url> now"},
		{"no scheme ://here", "no scheme ://here"},
		{"bad \xff utf8", "bad utf8"},
	}
	for _, tt := range tests {
		if got := notify.SanitizeReason(tt.in); got != tt.want {
			t.Errorf("SanitizeReason(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	long := notify.SanitizeReason(strings.Repeat("é", 500))
	if n := utf8.RuneCountInString(long); n != notify.MaxReasonRunes || !strings.HasSuffix(long, "…") {
		t.Errorf("long reason has %d runes: %q", n, long[len(long)-8:])
	}
}

func TestMessageJSONIsStable(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 4, 2, 30, 0, 0, time.UTC)
	m := notify.Message{
		Version: notify.MessageVersion, ID: "d1", Event: domain.EventMissed, OccurredAt: at, Title: "t",
		Schedule: &notify.ScheduleInfo{ID: "s1", Name: "Nightly"},
		Run:      &notify.RunInfo{ID: "r1", ScheduledFor: at, Reason: "worker_offline", Coalesced: 2},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"version":1,"id":"d1","event":"missed","occurred_at":"2026-11-04T02:30:00Z","title":"t",` +
		`"schedule":{"id":"s1","name":"Nightly"},"run":{"id":"r1","scheduled_for":"2026-11-04T02:30:00Z","reason":"worker_offline","coalesced":2}}`
	if string(b) != want {
		t.Errorf("JSON = %s\nwant  %s", b, want)
	}
	job, err := json.Marshal(notify.JobInfo{ID: "j", Kind: "k", Status: domain.JobInterrupted})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(job), "started_at") || strings.Contains(string(job), "error_code") {
		t.Errorf("zero optional job fields are not omitted: %s", job)
	}
	var back notify.Message
	if err := json.Unmarshal(b, &back); err != nil || back.Run.Coalesced != 2 || back.Event != domain.EventMissed {
		t.Errorf("round trip = (%+v, %v)", back, err)
	}
}

func TestTitle(t *testing.T) {
	t.Parallel()
	sched := &notify.ScheduleInfo{Name: "Nightly photos"}
	tests := []struct {
		m    notify.Message
		want string
	}{
		{notify.Message{Event: domain.EventFailed, Schedule: sched, Job: &notify.JobInfo{Kind: "rclone_transfer"}}, "Job failed: Nightly photos"},
		{notify.Message{Event: domain.EventSucceeded, Job: &notify.JobInfo{Kind: "rclone_transfer"}}, "Job succeeded: rclone_transfer"},
		{notify.Message{Event: domain.EventMissed, Schedule: sched, Run: &notify.RunInfo{}}, "Scheduled run missed: Nightly photos"},
		{notify.Message{Event: domain.EventFailed, Schedule: sched, Run: &notify.RunInfo{}}, "Scheduled run failed to start: Nightly photos"},
		{notify.Message{Event: domain.EventTest, Test: true}, "Syncr test notification"},
		{notify.Message{Event: domain.EventFailed, Schedule: &notify.ScheduleInfo{Name: "a\nb"}}, "Job failed: a b"},
	}
	for _, tt := range tests {
		if got := notify.Title(tt.m); got != tt.want {
			t.Errorf("Title = %q, want %q", got, tt.want)
		}
	}
	long := notify.Title(notify.Message{Event: domain.EventFailed, Schedule: &notify.ScheduleInfo{Name: strings.Repeat("x", 300)}})
	if utf8.RuneCountInString(long) != notify.MaxTitleRunes {
		t.Errorf("long title has %d runes", utf8.RuneCountInString(long))
	}
}

func TestDryRun(t *testing.T) {
	t.Parallel()
	wrapped := stub("discord", "Discord")
	r, err := notify.NewRegistry(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	dry := notify.DryRun(r, slog.New(slog.NewTextHandler(&logs, nil)))
	c, ok := dry.Get("discord")
	if !ok || c.Label() != "Discord" || len(dry.Channels()) != 1 {
		t.Fatal("DryRun lost the descriptor")
	}
	if err := c.Send(context.Background(), descriptor.Values{}, notify.Message{ID: "d1", Event: domain.EventFailed, Title: "hello"}); err != nil {
		t.Fatal(err)
	}
	if *wrapped.sent != 0 {
		t.Error("DryRun called the real Send")
	}
	if !strings.Contains(logs.String(), "dry run") || !strings.Contains(logs.String(), "hello") {
		t.Errorf("logs = %q", logs.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Send(ctx, descriptor.Values{}, notify.Message{}); err == nil {
		t.Error("DryRun Send ignores a cancelled context")
	}
	if ex, ok := c.(notify.Exampler); !ok || len(ex.Examples()) != 1 {
		t.Error("DryRun hides the examples")
	}
	// The original registry is untouched.
	if orig, _ := r.Get("discord"); orig.Send(context.Background(), descriptor.Values{}, notify.Message{}) != nil || *wrapped.sent != 1 {
		t.Error("the original registry changed")
	}
}
