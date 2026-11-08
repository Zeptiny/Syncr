package builtin_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/notify/builtin"
	"github.com/Zeptiny/Syncr/internal/descriptor"
	"github.com/Zeptiny/Syncr/internal/notify"
	"github.com/Zeptiny/Syncr/internal/notify/notifytest"
	"github.com/Zeptiny/Syncr/internal/platform/netguard"
)

// refuse is a transport and a dialer that fail every connection, so the
// suite's sends never leave the process.
type refuse struct{}

func (refuse) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused by the test")
}

func dialRefused(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("connection refused by the test")
}

func TestRegistry(t *testing.T) {
	t.Parallel()
	chs := builtin.Channels(builtin.Deps{HTTP: &http.Client{Transport: refuse{}}, Dial: dialRefused, Hostname: "controller.example"})
	reg, err := notify.NewRegistry(chs...)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	opts := []notifytest.Option{notifytest.WithSendTimeout(5 * time.Second)}
	for _, ch := range reg.Channels() {
		ids = append(ids, ch.ID())
		ex, ok := ch.(notify.Exampler)
		if !ok {
			t.Fatalf("%s has no examples", ch.ID())
		}
		// Every example fails to send (the connections are refused).
		opts = append(opts, notifytest.For(ch.ID(), notifytest.WithFailingTarget(ex.Examples()[0])))
	}
	if got := len(ids); got != 3 {
		t.Fatalf("channels = %v", ids)
	}
	for _, id := range []string{"discord", "email", "webhook"} {
		if _, ok := reg.Get(id); !ok {
			t.Errorf("channel %q is missing", id)
		}
	}
	notifytest.RunRegistry(t, reg, opts...)
}

// TestDefaultPolicyRefusesMetadata: with production clients, every
// channel refuses the cloud metadata address before sending anything.
func TestDefaultPolicyRefusesMetadata(t *testing.T) {
	t.Parallel()
	p := netguard.Policy{}
	reg, err := notify.NewRegistry(builtin.Channels(builtin.Deps{HTTP: p.HTTPClient(5 * time.Second), Dial: p.Dialer(5 * time.Second)})...)
	if err != nil {
		t.Fatal(err)
	}
	targets := map[string]descriptor.Values{
		"webhook": {Secrets: map[string]string{"url": "http://169.254.169.254/latest/meta-data/"}},
		"email":   {Options: map[string]string{"host": "169.254.169.254", "port": "25", "security": "none", "from": "a@b.c", "to": "d@e.f"}},
	}
	for id, cfg := range targets {
		ch, _ := reg.Get(id)
		if err := ch.Validate(cfg); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		err := ch.Send(context.Background(), cfg, notifytest.SampleMessage())
		var serr *notify.SendError
		if !errors.As(err, &serr) || !serr.Permanent {
			t.Errorf("%s: send to the metadata address = %v, want a permanent policy error", id, err)
		}
	}
}
