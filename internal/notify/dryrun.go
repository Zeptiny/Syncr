package notify

import (
	"context"
	"log/slog"

	"github.com/Zeptiny/Syncr/internal/descriptor"
)

// DryRun returns a registry with the same channel descriptors as r whose
// Send only logs the message (at info level, without the configuration)
// and succeeds. The development seed uses it, so a seeded controller never
// posts to real endpoints. A nil log discards.
func DryRun(r *Registry, log *slog.Logger) *Registry {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	out := &Registry{byID: make(map[string]Channel, len(r.sorted)), sorted: make([]Channel, 0, len(r.sorted))}
	for _, c := range r.sorted {
		d := dryChannel{Channel: c, log: log}
		out.byID[c.ID()] = d
		out.sorted = append(out.sorted, d)
	}
	return out
}

// dryChannel is a Channel whose Send only logs.
type dryChannel struct {
	Channel
	log *slog.Logger
}

// Send logs msg and succeeds, unless ctx is done.
func (d dryChannel) Send(ctx context.Context, _ descriptor.Values, msg Message) error {
	if err := ctx.Err(); err != nil {
		return Temporary(0, "the send was cancelled", 0)
	}
	d.log.InfoContext(ctx, "notification not sent (dry run)", "channel", d.ID(), "delivery_id", msg.ID,
		"event", string(msg.Event), "title", msg.Title)
	return nil
}

// Examples forwards to the wrapped channel, if it has examples.
func (d dryChannel) Examples() []descriptor.Values {
	if ex, ok := d.Channel.(Exampler); ok {
		return ex.Examples()
	}
	return nil
}
