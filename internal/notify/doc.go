// Package notify is the notification channel extension point: a Channel
// describes one way of notifying people (Discord, email, a webhook, ...) by
// its configuration fields (internal/descriptor), validates a contact's
// configuration and sends a Message with it. The web UI renders contact
// forms from the descriptors (served by the API's ChannelService), so
// adding a channel type is one package under internal/adapters/notify plus
// one line in its builtin list, with no frontend change.
//
// # Division of work
//
//   - A Channel is stateless configuration typing plus one Send. It does
//     not store anything, retry or schedule: the notifications use case
//     (internal/app/notifications) plans deliveries durably, retries them
//     with backoff and records their outcome; the contacts use case
//     (internal/app/contacts) owns names, ownership and the encryption of
//     secrets.
//   - Send reports failures as *SendError: Permanent ones (a rejected
//     webhook URL, a 5xx SMTP reply) are not retried; temporary ones are,
//     after RetryAfter when the target asked for it. A SendError's Reason
//     is shown to users and stored: it never contains secrets or URLs
//     (which often embed tokens).
//   - Channels reach the network only through the clients they are built
//     with (internal/platform/netguard checks the addresses they connect
//     to), so the composition root decides which targets are allowed.
//
// # Messages
//
// A Message is JSON-stable: it is the stored payload of a delivery and the
// body of the generic webhook, so field names and meanings never change
// within a Version. Title, Text and HTML (render.go) render it for people.
//
// NewRegistry checks the channels' static shape; internal/notify/notifytest
// is the contract suite every channel runs.
//
// Allowed dependencies: the standard library, internal/domain and
// internal/descriptor.
package notify
