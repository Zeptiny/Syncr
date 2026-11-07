// Package backendtest is the contract suite of backend type descriptors
// (backend.Type). Every descriptor runs it from its tests:
//
//	func TestContract(t *testing.T) { backendtest.Run(t, mytype.Type{}) }
//
// It checks, beyond backend.CheckDescriptor's static shape:
//
//  1. the ID and label;
//  2. the fields: names, kinds, enum choices and defaults, secret fields
//     without defaults, Obscure only on secrets, members only on unions and
//     never a required field that is advanced;
//  3. Validate: it accepts every example configuration (the type's
//     backend.Exampler, or one synthesized from the fields), which together
//     must set every field, and rejects unknown keys, values in the wrong
//     map (a secret in Options, a non-secret in Secrets) and missing
//     required fields, as a *domain.ValidationError whose messages contain
//     no values;
//  4. ToRclone: it only emits declared rclone option names, secret values
//     only in Rclone.Secrets (never in Options, Root or Backend), obscured
//     keys that are secret fields marked Obscure, Backend equal to the ID
//     ("" exactly for a Local type), the same output for the same input, and
//     leaves its input unchanged.
//
// WithAccept adds a check of every mapped example, e.g. that the worker
// accepts it. Secret values never echo through the API is checked by the
// API's tests.
//
// It is test support: it imports only the standard library, internal/domain
// and internal/backend.
package backendtest

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// reporter is the part of *testing.T the checks use, so that the suite's
// own tests can run them on broken types.
type reporter interface {
	Helper()
	Errorf(format string, args ...any)
}

// Option configures Run.
type Option func(*options)

type options struct {
	accept []func(backend.Rclone) error
}

// WithAccept makes Run call fn with the rclone remote of every example
// configuration; an error fails the test.
func WithAccept(fn func(backend.Rclone) error) Option {
	return func(o *options) { o.accept = append(o.accept, fn) }
}

// RunRegistry runs Run for every type of reg, each in a subtest named by
// its ID.
func RunRegistry(t *testing.T, reg *backend.Registry, opts ...Option) {
	t.Helper()
	types := reg.Types()
	if len(types) == 0 {
		t.Fatal("the registry has no types")
	}
	for _, typ := range types {
		t.Run(typ.ID(), func(t *testing.T) { Run(t, typ, opts...) })
	}
}

// Run runs the contract suite on typ.
func Run(t *testing.T, typ backend.Type, opts ...Option) {
	t.Helper()
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	t.Run("Descriptor", func(t *testing.T) { checkDescriptor(t, typ) })
	t.Run("Validate", func(t *testing.T) { checkValidate(t, typ) })
	t.Run("ToRclone", func(t *testing.T) { checkToRclone(t, typ, o) })
}

var (
	idRe    = regexp.MustCompile(`^[a-z][a-z0-9]{0,63}$`)
	fieldRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// checkDescriptor checks the ID, label and fields (checks 1 and 2).
func checkDescriptor(t reporter, typ backend.Type) {
	if err := backend.CheckDescriptor(typ); err != nil {
		t.Errorf("CheckDescriptor: %v", err)
	}
	if !idRe.MatchString(typ.ID()) {
		t.Errorf("ID %q does not match %s", typ.ID(), idRe)
	}
	if typ.Label() == "" {
		t.Errorf("Label is empty")
	}
	caps := typ.Capabilities()
	seen := map[string]bool{}
	members := 0
	for _, f := range typ.Fields() {
		where := fmt.Sprintf("field %q", f.Name)
		if !fieldRe.MatchString(f.Name) || seen[f.Name] {
			t.Errorf("%s: name is malformed or used twice", where)
		}
		seen[f.Name] = true
		if !f.Kind.Valid() {
			t.Errorf("%s: unknown kind %q", where, f.Kind)
		}
		if f.Kind == backend.KindEnum {
			if len(f.Choices) == 0 {
				t.Errorf("%s: an enum without choices", where)
			}
			if f.Default != "" && !slices.ContainsFunc(f.Choices, func(c backend.Choice) bool { return c.Value == f.Default }) {
				t.Errorf("%s: default %q is not a choice", where, f.Default)
			}
		}
		if f.Default != "" {
			cfg := backend.Config{Options: map[string]string{f.Name: f.Default}}
			for _, fe := range backend.CheckFields([]backend.Field{f}, cfg) {
				t.Errorf("%s: default %q does not parse: %s", where, f.Default, fe.Message)
			}
		}
		if f.Secret && f.Default != "" {
			t.Errorf("%s: a secret field has a default", where)
		}
		if f.Obscure && !f.Secret {
			t.Errorf("%s: Obscure without Secret", where)
		}
		if f.Required && f.Advanced {
			t.Errorf("%s: required and advanced", where)
		}
		if f.Kind == backend.KindMembers {
			members++
		}
	}
	if members > 1 || (members == 1) != caps.Union {
		t.Errorf("%d members fields with Capabilities.Union = %v; a union has exactly one, other types none", members, caps.Union)
	}
	if fs := typ.Fields(); len(fs) > 0 {
		fs[0].Name = "mutated"
		if typ.Fields()[0].Name == "mutated" {
			t.Errorf("Fields returns a slice that aliases the descriptor")
		}
	}
}

// configFields are the fields that are configuration values (not members).
func configFields(typ backend.Type) []backend.Field {
	return slices.DeleteFunc(typ.Fields(), func(f backend.Field) bool { return f.Kind == backend.KindMembers })
}

// examples returns the type's example configurations, or one synthesized
// from its fields, with every secret value replaced by a unique sentinel
// (so that leaks are detectable).
func examples(t reporter, typ backend.Type) []backend.Config {
	t.Helper()
	var out []backend.Config
	if ex, ok := typ.(backend.Exampler); ok {
		out = ex.Examples()
		if len(out) == 0 {
			t.Errorf("Examples returned no configurations")
			return nil
		}
	} else {
		out = []backend.Config{synthesize(typ)}
	}
	for i := range out {
		out[i] = out[i].Clone()
		for k := range out[i].Secrets {
			out[i].Secrets[k] = sentinel(i, k)
		}
	}
	return out
}

func sentinel(i int, key string) string { return fmt.Sprintf("SeNtInEl-%d-%s-7f3a", i, key) }

// synthesize returns a configuration that sets every field to a value of
// its kind.
func synthesize(typ backend.Type) backend.Config {
	cfg := backend.Config{Options: map[string]string{}, Secrets: map[string]string{}}
	for _, f := range configFields(typ) {
		var v string
		switch f.Kind {
		case backend.KindInt:
			v = "1"
		case backend.KindBool:
			v = "true"
		case backend.KindEnum:
			v = f.Choices[0].Value
		default:
			v = "value-" + f.Name
		}
		if f.Default != "" {
			v = f.Default
		}
		if f.Secret {
			cfg.Secrets[f.Name] = v
		} else {
			cfg.Options[f.Name] = v
		}
	}
	return cfg
}

// validationFields returns the field paths of err, failing the test if it
// is not a *domain.ValidationError.
func validationFields(t reporter, what string, err error) []string {
	t.Helper()
	var verr *domain.ValidationError
	if !errors.As(err, &verr) || len(verr.Fields) == 0 {
		t.Errorf("%s: Validate = %v, want a *domain.ValidationError with fields", what, err)
		return nil
	}
	if !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("%s: the error does not match domain.ErrInvalidArgument", what)
	}
	paths := make([]string, len(verr.Fields))
	for i, f := range verr.Fields {
		paths[i] = f.Field
	}
	return paths
}

// noValues fails the test if err's text contains a value of cfg.
func noValues(t reporter, what string, err error, cfg backend.Config) {
	t.Helper()
	if err == nil {
		return
	}
	msg := err.Error()
	for _, v := range slices.Concat(slices.Collect(maps.Values(cfg.Secrets)), slices.Collect(maps.Values(cfg.Options))) {
		if len(v) >= 6 && strings.Contains(msg, v) {
			t.Errorf("%s: the validation error contains the value %q", what, v)
		}
	}
}

// checkValidate is check 3.
func checkValidate(t reporter, typ backend.Type) {
	exs := examples(t, typ)
	if len(exs) == 0 {
		return
	}
	set := map[string]bool{}
	for i, cfg := range exs {
		if err := typ.Validate(cfg.Clone()); err != nil {
			t.Errorf("example %d: Validate = %v, want nil", i, err)
		}
		for k, v := range cfg.Options {
			set[k] = set[k] || v != ""
		}
		for k, v := range cfg.Secrets {
			set[k] = set[k] || v != ""
		}
	}
	for _, f := range configFields(typ) {
		if !set[f.Name] {
			t.Errorf("no example sets field %q", f.Name)
		}
	}

	base := exs[0]
	has := slices.Contains[[]string]

	unknown := base.Clone()
	if unknown.Options == nil {
		unknown.Options = map[string]string{}
	}
	unknown.Options["zz_not_a_field"] = "unknown-value-1234"
	if paths := validationFields(t, "unknown option", typ.Validate(unknown)); paths != nil && !has(paths, "options.zz_not_a_field") {
		t.Errorf("unknown option: fields %v do not name options.zz_not_a_field", paths)
	}
	unknown = base.Clone()
	if unknown.Secrets == nil {
		unknown.Secrets = map[string]string{}
	}
	unknown.Secrets["zz_not_a_field"] = "unknown-secret-1234"
	err := typ.Validate(unknown)
	if paths := validationFields(t, "unknown secret", err); paths != nil && !has(paths, "secrets.zz_not_a_field") {
		t.Errorf("unknown secret: fields %v do not name secrets.zz_not_a_field", paths)
	}
	noValues(t, "unknown secret", err, unknown)

	for _, f := range configFields(typ) {
		misplaced := base.Clone()
		if misplaced.Options == nil {
			misplaced.Options = map[string]string{}
		}
		if misplaced.Secrets == nil {
			misplaced.Secrets = map[string]string{}
		}
		value := valueFor(f)
		var want string
		if f.Secret {
			delete(misplaced.Secrets, f.Name)
			misplaced.Options[f.Name] = value
			want = "options." + f.Name
		} else {
			delete(misplaced.Options, f.Name)
			misplaced.Secrets[f.Name] = value
			want = "secrets." + f.Name
		}
		err := typ.Validate(misplaced)
		what := fmt.Sprintf("field %q in the wrong map", f.Name)
		if paths := validationFields(t, what, err); paths != nil && !has(paths, want) {
			t.Errorf("%s: fields %v do not name %s", what, paths, want)
		}
		noValues(t, what, err, misplaced)

		if !f.Required {
			continue
		}
		missing := base.Clone()
		path := "options." + f.Name
		if f.Secret {
			delete(missing.Secrets, f.Name)
			path = "secrets." + f.Name
		} else {
			delete(missing.Options, f.Name)
		}
		what = fmt.Sprintf("required field %q missing", f.Name)
		if paths := validationFields(t, what, typ.Validate(missing)); paths != nil && !has(paths, path) {
			t.Errorf("%s: fields %v do not name %s", what, paths, path)
		}
	}
}

// valueFor returns a valid value of f's kind, long enough to be searched
// for in messages.
func valueFor(f backend.Field) string {
	switch f.Kind {
	case backend.KindInt:
		return "424242"
	case backend.KindBool:
		return "true"
	case backend.KindEnum:
		return f.Choices[0].Value
	}
	return "misplaced-value-" + f.Name
}

// checkToRclone is check 4, plus the WithAccept functions.
func checkToRclone(t reporter, typ backend.Type, o options) {
	rcloneNames := map[string]backend.Field{}
	for _, f := range configFields(typ) {
		if name := f.RcloneName(); name != "" {
			rcloneNames[name] = f
		}
	}
	local := typ.Capabilities().Local
	for i, cfg := range examples(t, typ) {
		where := fmt.Sprintf("example %d", i)
		in := cfg.Clone()
		r, err := typ.ToRclone(in)
		if err != nil {
			t.Errorf("%s: ToRclone: %v", where, err)
			continue
		}
		if !reflect.DeepEqual(in, cfg) {
			t.Errorf("%s: ToRclone changed its input", where)
		}
		again, err := typ.ToRclone(cfg.Clone())
		if err != nil || !reflect.DeepEqual(again, r) {
			t.Errorf("%s: ToRclone is not deterministic: %+v, then %+v (%v)", where, r, again, err)
		}
		switch {
		case local && r.Backend != "":
			t.Errorf("%s: a local type's Backend is %q, want empty", where, r.Backend)
		case !local && r.Backend != typ.ID():
			t.Errorf("%s: Backend = %q, want the ID %q", where, r.Backend, typ.ID())
		}
		for k := range r.Options {
			if f, ok := rcloneNames[k]; !ok || f.Secret {
				t.Errorf("%s: option %q is not the rclone name of a non-secret field", where, k)
			}
		}
		for k := range r.Secrets {
			if f, ok := rcloneNames[k]; !ok || !f.Secret {
				t.Errorf("%s: secret %q is not the rclone name of a secret field", where, k)
			}
		}
		for _, k := range r.ObscureKeys {
			if f, ok := rcloneNames[k]; !ok || !f.Obscure {
				t.Errorf("%s: obscure key %q is not a field marked Obscure", where, k)
			}
			if _, ok := r.Secrets[k]; !ok {
				t.Errorf("%s: obscure key %q is not in Secrets", where, k)
			}
		}
		for name, f := range rcloneNames {
			if _, ok := r.Secrets[name]; ok && f.Obscure && !slices.Contains(r.ObscureKeys, name) {
				t.Errorf("%s: secret %q is marked Obscure but not listed in ObscureKeys", where, name)
			}
		}
		for k, secret := range cfg.Secrets {
			if secret == "" {
				continue
			}
			for ok, ov := range r.Options {
				if strings.Contains(ov, secret) {
					t.Errorf("%s: the value of secret %q appears in option %q", where, k, ok)
				}
			}
			if strings.Contains(r.Root, secret) || strings.Contains(r.Backend, secret) {
				t.Errorf("%s: the value of secret %q appears in Root or Backend", where, k)
			}
			if !slices.Contains(slices.Collect(maps.Values(r.Secrets)), secret) {
				t.Errorf("%s: the value of secret %q is not passed in Secrets", where, k)
			}
		}
		for _, fn := range o.accept {
			if err := fn(r.Clone()); err != nil {
				t.Errorf("%s: not accepted: %v", where, err)
			}
		}
	}
}
