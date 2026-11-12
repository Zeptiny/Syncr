// Package builtin lists the built-in notification channels: Discord,
// email and webhook. The composition root builds them with clients from
// internal/platform/netguard, so every target address is checked against
// the notification network policy:
//
//	policy := netguard.Policy{Mode: mode, Allow: allowed}
//	reg, err := notify.NewRegistry(builtin.Channels(builtin.Deps{
//		HTTP:     policy.HTTPClient(sendTimeout),
//		Dial:     policy.Dialer(sendTimeout),
//		Hostname: hostname,
//	})...)
//
// Adding a channel is one package under internal/adapters/notify and one
// line here.
package builtin

import (
	"net/http"

	"github.com/Zeptiny/Syncr/internal/adapters/notify/discord"
	"github.com/Zeptiny/Syncr/internal/adapters/notify/email"
	"github.com/Zeptiny/Syncr/internal/adapters/notify/webhook"
	"github.com/Zeptiny/Syncr/internal/notify"
	"github.com/Zeptiny/Syncr/internal/platform/netguard"
)

// Deps are what the built-in channels send with.
type Deps struct {
	// HTTP sends the Discord and webhook requests: netguard's HTTPClient
	// (no redirects, no proxy, checked addresses, the send timeout).
	HTTP *http.Client
	// Dial connects to SMTP servers: netguard's Dialer.
	Dial netguard.DialFunc
	// Hostname is the default EHLO name of SMTP sessions (the
	// controller's host name); empty means "localhost".
	Hostname string
}

// Channels returns the built-in channels, for notify.NewRegistry.
func Channels(d Deps) []notify.Channel {
	return []notify.Channel{
		discord.New(d.HTTP),
		email.New(d.Dial, email.WithHelloName(d.Hostname)),
		webhook.New(d.HTTP),
	}
}
