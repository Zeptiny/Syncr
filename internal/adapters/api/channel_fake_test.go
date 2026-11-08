package api

import (
	"context"
	"slices"
	"sync"
	"testing"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/descriptor"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/notify"
	"github.com/Zeptiny/Syncr/internal/notify/notifytest"
)

// fakeChannel is a channel type that exists only in this test. It proves
// that adding a channel type is one value: registered next to the others,
// it passes the contract suite, is served by ListChannels and is usable for
// contacts, test sends and planned deliveries with no other code change.
// It records what it sends; the room "nowhere" fails for good and "busy"
// for now.
type fakeChannel struct {
	mu   sync.Mutex
	sent []notify.Message
}

func (*fakeChannel) ID() string          { return "fake" }
func (*fakeChannel) Label() string       { return "Fake chat" }
func (*fakeChannel) Description() string { return "A channel type for tests." }

func (*fakeChannel) Fields() []descriptor.Field {
	return []descriptor.Field{
		{Name: "webhook_url", Label: "Webhook URL", Kind: descriptor.KindString, Required: true, Secret: true},
		{Name: "room", Label: "Room", Kind: descriptor.KindString, Required: true},
		{Name: "mention", Label: "Mention", Kind: descriptor.KindBool, Default: "false", Advanced: true},
	}
}

func (c *fakeChannel) Validate(cfg descriptor.Values) error {
	var v domain.Validation
	v.AddAll("", descriptor.CheckFields(c.Fields(), cfg))
	return v.Err()
}

func (*fakeChannel) Summary(cfg descriptor.Values) string { return "room " + cfg.Options["room"] }

func (*fakeChannel) Examples() []descriptor.Values {
	return []descriptor.Values{{
		Options: map[string]string{"room": "ops", "mention": "true"},
		Secrets: map[string]string{"webhook_url": "https://chat.example/hooks/fake-example-token"},
	}}
}

func (c *fakeChannel) Send(ctx context.Context, cfg descriptor.Values, msg notify.Message) error {
	if ctx.Err() != nil {
		return notify.Temporary(0, "the send was cancelled", 0)
	}
	c.mu.Lock()
	c.sent = append(c.sent, msg)
	c.mu.Unlock()
	switch cfg.Options["room"] {
	case "nowhere":
		return notify.Permanent(404, "no such room")
	case "busy":
		return notify.Temporary(503, "the room is busy", 0)
	}
	return nil
}

func (c *fakeChannel) messages() []notify.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.sent)
}

// otherChannel is a second registered type, so the fake one is listed
// next to others.
type otherChannel struct{ fakeChannel }

func (*otherChannel) ID() string    { return "other" }
func (*otherChannel) Label() string { return "Another chat" }

func TestFakeChannelIsOneValue(t *testing.T) {
	t.Parallel()
	failing := (&fakeChannel{}).Examples()[0]
	failing.Options["room"] = "nowhere"
	notifytest.Run(t, &fakeChannel{}, notifytest.WithFailingTarget(failing))

	e := newNotifyEnv(t)
	// Listed with its descriptor, by label.
	resp, err := e.channelsFor(testUser).ListChannels(t.Context(), connect.NewRequest(&syncrv1.ListChannelsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, ch := range resp.Msg.GetChannels() {
		ids = append(ids, ch.GetId())
	}
	if !slices.Equal(ids, []string{"other", "fake"}) {
		t.Fatalf("ListChannels = %v, want by label", ids)
	}
	fake := resp.Msg.GetChannels()[1]
	var names []string
	for _, f := range fake.GetFields() {
		names = append(names, f.GetName())
	}
	if fake.GetLabel() != "Fake chat" || fake.GetDescription() == "" || !slices.Equal(names, []string{"webhook_url", "room", "mention"}) ||
		!fake.GetFields()[0].GetSecret() || fake.GetFields()[2].GetKind() != syncrv1.FieldKind_FIELD_KIND_BOOL ||
		fake.GetFields()[2].GetDefaultValue() != "false" || !fake.GetFields()[2].GetAdvanced() {
		t.Errorf("fake channel = %v", fake)
	}

	// A contact of the type.
	c := e.createContact(testUser, fakeContactInput("Team chat", "ops"))
	if c.GetChannelType() != "fake" || c.GetSummary() != "room ops" || !slices.Equal(c.GetSecretsSet(), []string{"webhook_url"}) {
		t.Errorf("contact = %v", c)
	}
	// A test notification.
	test, err := e.contactsFor(testUser).SendTestNotification(t.Context(), connect.NewRequest(&syncrv1.SendTestNotificationRequest{ContactId: c.GetId()}))
	if err != nil || !test.Msg.GetOk() {
		t.Fatalf("SendTestNotification = (%v, %v)", test, err)
	}
	if sent := e.channel.messages(); len(sent) != 1 || !sent[0].Test || sent[0].Event != domain.EventTest {
		t.Errorf("test message = %+v", sent)
	}

	// A planned delivery for a scheduled job's outcome.
	sc := e.addSchedule(testUser, "Nightly photos", schedules.ContactLink{
		ContactID: e.contactID(c), Events: domain.EventSet{domain.EventFailed, domain.EventMissed},
	})
	job := e.finishJob(testUser, domain.JobFailed, sc.ID)
	e.planAndDeliver()
	sent := e.channel.messages()
	if len(sent) != 2 {
		t.Fatalf("sends = %+v", sent)
	}
	m := sent[1]
	if m.Event != domain.EventFailed || m.Job == nil || m.Job.ID != job.ID.String() || m.Schedule == nil ||
		m.Schedule.Name != "Nightly photos" || m.ID == "" || m.Title != "Job failed: Nightly photos" {
		t.Errorf("delivered message = %+v", m)
	}
	ds := e.listDeliveries(testUser, &syncrv1.ListDeliveriesRequest{ContactId: c.GetId()})
	if len(ds.GetDeliveries()) != 1 || ds.GetDeliveries()[0].GetId() != m.ID ||
		ds.GetDeliveries()[0].GetStatus() != syncrv1.DeliveryStatus_DELIVERY_STATUS_DELIVERED {
		t.Errorf("ListDeliveries = %v", ds)
	}
}
