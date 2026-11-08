package backend

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/Zeptiny/Syncr/internal/descriptor"
)

var (
	// idRe matches type IDs (a valid rclone backend name, no "_" so that it
	// can never look like an rclone alias suffix).
	idRe = regexp.MustCompile(`^[a-z][a-z0-9]{0,63}$`)
	// rcloneRe matches rclone option names (as the worker accepts them).
	rcloneRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
)

// Registry is an immutable set of backend types, keyed by ID. It is safe for
// concurrent use.
type Registry struct {
	byID map[string]Type
	// sorted is every type by Label, then ID.
	sorted []Type
}

// NewRegistry returns a registry of types. It rejects nil types, duplicate
// IDs and descriptors that fail CheckDescriptor.
func NewRegistry(types ...Type) (*Registry, error) {
	r := &Registry{byID: make(map[string]Type, len(types))}
	for i, t := range types {
		if t == nil {
			return nil, fmt.Errorf("backend registry: type %d is nil", i)
		}
		if err := CheckDescriptor(t); err != nil {
			return nil, fmt.Errorf("backend registry: %w", err)
		}
		if _, dup := r.byID[t.ID()]; dup {
			return nil, fmt.Errorf("backend registry: type %q is registered twice", t.ID())
		}
		r.byID[t.ID()] = t
		r.sorted = append(r.sorted, t)
	}
	slices.SortFunc(r.sorted, func(a, b Type) int {
		return cmp.Or(cmp.Compare(a.Label(), b.Label()), cmp.Compare(a.ID(), b.ID()))
	})
	return r, nil
}

// Get returns the type with id.
func (r *Registry) Get(id string) (Type, bool) {
	t, ok := r.byID[id]
	return t, ok
}

// Types returns every type, by Label (then ID). The slice is the caller's.
func (r *Registry) Types() []Type { return slices.Clone(r.sorted) }

// CheckDescriptor checks the static shape of a type descriptor:
//
//   - the ID matches ^[a-z][a-z0-9]{0,63}$ and the label is not empty;
//   - capabilities are consistent: a union is neither local nor a union
//     member;
//   - the fields pass descriptor.CheckShape (names, labels, kinds, enum
//     choices, defaults, no secret default, no required advanced field),
//     with KindMembers as an extra kind;
//   - rclone names are valid rclone option names (or NotRclone) and no two
//     fields map to the same one;
//   - Obscure implies Secret;
//   - a members field appears only in union types, at most once, and has
//     no secret flag, default or choices; a union type has exactly one.
//
// The behavior of Validate and ToRclone is checked by the contract suite
// (internal/backend/backendtest).
func CheckDescriptor(t Type) error {
	id := t.ID()
	if !idRe.MatchString(id) {
		return fmt.Errorf("type ID %q must match %s", id, idRe)
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("type %q: %s", id, fmt.Sprintf(format, args...))
	}
	if t.Label() == "" {
		return fail("label is empty")
	}
	caps := t.Capabilities()
	if caps.Union && (caps.Local || caps.UnionMember) {
		return fail("a union type cannot be local or a union member")
	}

	fields := t.Fields()
	shape := make([]descriptor.Field, len(fields))
	for i, f := range fields {
		shape[i] = f.Descriptor()
	}
	var errs []error
	if err := descriptor.CheckShape(shape, descriptor.Kind(KindMembers)); err != nil {
		errs = append(errs, fmt.Errorf("type %q: %w", id, err))
	}
	rcloneNames := map[string]string{}
	members := 0
	for i, f := range fields {
		ferr := func(format string, args ...any) {
			errs = append(errs, fail("field %d (%q): %s", i, f.Name, fmt.Sprintf(format, args...)))
		}
		if f.Obscure && !f.Secret {
			ferr("only secret fields can be obscured")
		}
		if f.Kind == KindMembers {
			members++
			if f.Secret || f.Default != "" || len(f.Choices) > 0 {
				ferr("a members field has no secret flag, default or choices")
			}
			continue
		}
		if f.Rclone != "" && f.Rclone != NotRclone && !rcloneRe.MatchString(f.Rclone) {
			ferr("rclone name %q must match %s", f.Rclone, rcloneRe)
		}
		if name := f.RcloneName(); name != "" {
			if other, dup := rcloneNames[name]; dup {
				ferr("maps to rclone option %q like field %q", name, other)
			}
			rcloneNames[name] = f.Name
		}
	}
	switch {
	case members > 1:
		errs = append(errs, fail("has %d members fields; at most one is allowed", members))
	case members == 1 && !caps.Union:
		errs = append(errs, fail("has a members field but is not a union"))
	case members == 0 && caps.Union:
		errs = append(errs, fail("is a union but has no members field"))
	}
	return errors.Join(errs...)
}
