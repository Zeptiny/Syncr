package notify

import (
	"context"

	"github.com/Zeptiny/Syncr/internal/descriptor"
)

// Channel is a notification channel type. Implementations are stateless
// values (apart from the clients they are built with) and safe for
// concurrent use.
type Channel interface {
	// ID is the stable identifier stored with contacts:
	// ^[a-z][a-z0-9]{0,31}$, e.g. "discord".
	ID() string
	// Label and Description are shown in the type picker.
	Label() string
	Description() string
	// Fields lists the configuration fields in form order. The returned
	// slice is the caller's to keep.
	Fields() []descriptor.Field
	// Validate checks a configuration. It returns nil or a
	// *domain.ValidationError whose field paths are "options.<name>" or
	// "secrets.<name>"; messages never contain values.
	Validate(cfg descriptor.Values) error
	// Summary returns a non-secret one-line description of a valid
	// configuration for lists, e.g. "discord.com webhook" or
	// "smtp.example.com → 2 recipients". It never contains a secret value
	// and must not fail on an invalid configuration.
	Summary(cfg descriptor.Values) string
	// Send delivers msg with a valid configuration. It returns nil once the
	// target accepted the message, or a *SendError. It honors ctx: a
	// cancelled or expired context ends the attempt with an error.
	Send(ctx context.Context, cfg descriptor.Values, msg Message) error
}

// Exampler is implemented by channels that provide example configurations,
// which the contract suite (internal/notify/notifytest) validates and
// mutates. Examples should set every field, secrets with values that are
// not substrings of anything else.
type Exampler interface {
	Examples() []descriptor.Values
}
