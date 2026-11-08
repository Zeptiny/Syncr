package notify

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"

	"github.com/Zeptiny/Syncr/internal/descriptor"
)

// idRe matches channel type IDs.
var idRe = regexp.MustCompile(`^[a-z][a-z0-9]{0,31}$`)

// Registry is an immutable set of channel types, keyed by ID. It is safe
// for concurrent use.
type Registry struct {
	byID map[string]Channel
	// sorted is every channel by Label, then ID.
	sorted []Channel
}

// NewRegistry returns a registry of channels. It rejects nil channels,
// duplicate IDs and descriptors that fail CheckChannel.
func NewRegistry(ch ...Channel) (*Registry, error) {
	r := &Registry{byID: make(map[string]Channel, len(ch))}
	for i, c := range ch {
		if c == nil {
			return nil, fmt.Errorf("channel registry: channel %d is nil", i)
		}
		if err := CheckChannel(c); err != nil {
			return nil, fmt.Errorf("channel registry: %w", err)
		}
		if _, dup := r.byID[c.ID()]; dup {
			return nil, fmt.Errorf("channel registry: channel %q is registered twice", c.ID())
		}
		r.byID[c.ID()] = c
		r.sorted = append(r.sorted, c)
	}
	slices.SortFunc(r.sorted, func(a, b Channel) int {
		return cmp.Or(cmp.Compare(a.Label(), b.Label()), cmp.Compare(a.ID(), b.ID()))
	})
	return r, nil
}

// Get returns the channel with id.
func (r *Registry) Get(id string) (Channel, bool) {
	c, ok := r.byID[id]
	return c, ok
}

// Channels returns every channel, by Label (then ID). The slice is the
// caller's.
func (r *Registry) Channels() []Channel { return slices.Clone(r.sorted) }

// CheckChannel checks the static shape of a channel descriptor: the ID
// matches ^[a-z][a-z0-9]{0,31}$, the label is not empty and the fields pass
// descriptor.CheckShape. The behavior of Validate, Summary and Send is
// checked by the contract suite (internal/notify/notifytest).
func CheckChannel(c Channel) error {
	id := c.ID()
	if !idRe.MatchString(id) {
		return fmt.Errorf("channel ID %q must match %s", id, idRe)
	}
	if c.Label() == "" {
		return fmt.Errorf("channel %q: label is empty", id)
	}
	if err := descriptor.CheckShape(c.Fields()); err != nil {
		return fmt.Errorf("channel %q: %w", id, err)
	}
	return nil
}
