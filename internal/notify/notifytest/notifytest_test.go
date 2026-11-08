package notifytest_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/descriptor"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/notify"
	"github.com/Zeptiny/Syncr/internal/notify/notifytest"
)

// fakeChannel is a well-behaved channel: it "posts" to the URL in its
// secret and fails when the URL's host is "fail.invalid".
type fakeChannel struct{}

func (fakeChannel) ID() string          { return "fake" }
func (fakeChannel) Label() string       { return "Fake" }
func (fakeChannel) Description() string { return "A channel for tests." }

func (fakeChannel) Fields() []descriptor.Field {
	return []descriptor.Field{
		{Name: "url", Label: "URL", Kind: descriptor.KindString, Secret: true, Required: true},
		{Name: "username", Label: "Username", Kind: descriptor.KindString, Advanced: true},
		{Name: "format", Label: "Format", Kind: descriptor.KindEnum, Default: "json",
			Choices: []descriptor.Choice{{Value: "json", Label: "JSON"}, {Value: "text", Label: "Text"}}},
	}
}

func (c fakeChannel) Validate(cfg descriptor.Values) error {
	var v domain.Validation
	v.AddAll("", descriptor.CheckFields(c.Fields(), cfg))
	if u := cfg.Secrets["url"]; u != "" && !strings.HasPrefix(u, "https://") {
		v.Add("secrets.url", "must be an https URL")
	}
	return v.Err()
}

func (fakeChannel) Summary(cfg descriptor.Values) string {
	host, _, _ := strings.Cut(strings.TrimPrefix(cfg.Secrets["url"], "https://"), "/")
	if host == "" || strings.Contains(host, "notifytest") {
		return "fake target"
	}
	return host + " target"
}

func (fakeChannel) Send(ctx context.Context, cfg descriptor.Values, _ notify.Message) error {
	if ctx.Err() != nil {
		return notify.Temporary(0, "cancelled", 0)
	}
	if strings.HasPrefix(cfg.Secrets["url"], "https://fail.invalid/") {
		return notify.Temporary(503, `Post "`+cfg.Secrets["url"]+`": service unavailable`, 0)
	}
	return nil
}

func (fakeChannel) Examples() []descriptor.Values {
	return []descriptor.Values{
		{Options: map[string]string{"username": "syncr", "format": "text"}, Secrets: map[string]string{"url": "https://hooks.example/abc/s3cr3t-token"}},
		{Secrets: map[string]string{"url": "https://hooks.example/def/other-token"}},
	}
}

func TestFakeChannelPassesTheSuite(t *testing.T) {
	t.Parallel()
	failing := descriptor.Values{Secrets: map[string]string{"url": "https://fail.invalid/hook/sentinel-token-123"}}
	notifytest.Run(t, fakeChannel{}, notifytest.WithFailingTarget(failing))
}

func TestRunRegistryWithScopedOptions(t *testing.T) {
	t.Parallel()
	reg, err := notify.NewRegistry(fakeChannel{})
	if err != nil {
		t.Fatal(err)
	}
	failing := descriptor.Values{Secrets: map[string]string{"url": "https://fail.invalid/hook/sentinel-token-456"}}
	notifytest.RunRegistry(t, reg, notifytest.For("fake", notifytest.WithFailingTarget(failing)))
	// The dry-run registry has the same descriptors and passes too.
	notifytest.RunRegistry(t, notify.DryRun(reg, nil))
}

func TestSampleMessage(t *testing.T) {
	t.Parallel()
	m := notifytest.SampleMessage()
	if m.Version != notify.MessageVersion || m.Job == nil || m.Schedule == nil || m.Title == "" {
		t.Errorf("SampleMessage = %+v", m)
	}
}
