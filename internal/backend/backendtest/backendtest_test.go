package backendtest_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/backendtest"
	"github.com/Zeptiny/Syncr/internal/backend/builtin"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// TestBuiltins runs the suite over every built-in type, with an accept
// function that sees every mapped example.
func TestBuiltins(t *testing.T) {
	t.Parallel()
	reg, err := backend.NewRegistry(builtin.Types()...)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	backendtest.RunRegistry(t, reg, backendtest.WithAccept(func(r backend.Rclone) error {
		seen[r.Backend]++
		return nil
	}))
	for _, typ := range reg.Types() {
		backendName := typ.ID()
		if typ.Capabilities().Local {
			backendName = ""
		}
		if seen[backendName] == 0 {
			t.Errorf("WithAccept never saw a %s remote", typ.ID())
		}
	}
}

// recorder collects the failures of the checks.
type recorder struct{ errs []string }

func (*recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

// goodType is a minimal valid descriptor; the broken variants change one
// behavior each.
type goodType struct {
	local, leak, wrongBackend, extraOption, lenient, noObscure, randomRoot bool
	calls                                                                  *int
}

func (goodType) ID() string          { return "good" }
func (goodType) Label() string       { return "Good" }
func (goodType) Description() string { return "a test descriptor" }

func (g goodType) Fields() []backend.Field {
	return []backend.Field{
		{Name: "host", Label: "Host", Kind: backend.KindString, Required: true},
		{Name: "pass", Label: "Password", Kind: backend.KindString, Secret: true, Obscure: true},
		{Name: "path", Label: "Path", Kind: backend.KindString, Rclone: backend.NotRclone},
	}
}

func (g goodType) Capabilities() backend.Capabilities {
	return backend.Capabilities{Local: g.local, UnionMember: true}
}

func (g goodType) Validate(cfg backend.Config) error {
	if g.lenient {
		return nil
	}
	var v domain.Validation
	v.AddAll("", backend.CheckFields(g.Fields(), cfg))
	return v.Err()
}

func (g goodType) ToRclone(cfg backend.Config) (backend.Rclone, error) {
	r := backend.MapFields(g.Fields(), cfg)
	r.Backend, r.Root = "good", cfg.Options["path"]
	switch {
	case g.local:
		r.Backend = "good" // should be ""
	case g.leak:
		r.Options["host"] = cfg.Options["host"] + cfg.Secrets["pass"]
	case g.wrongBackend:
		r.Backend = "other"
	case g.extraOption:
		r.Options["undeclared"] = "x"
	case g.noObscure:
		r.ObscureKeys = nil
	case g.randomRoot:
		*g.calls++
		r.Root = strings.Repeat("x", *g.calls)
	}
	return r, nil
}

func TestSuiteCatchesBrokenTypes(t *testing.T) {
	t.Parallel()
	var good recorder
	backendtest.Check(&good, goodType{})
	if len(good.errs) != 0 {
		t.Fatalf("the valid type fails: %v", good.errs)
	}
	calls := 0
	for _, tt := range []struct {
		name string
		typ  goodType
		want string
	}{
		{"local with a backend", goodType{local: true}, "local type's Backend"},
		{"secret leaked into an option", goodType{leak: true}, `appears in option "host"`},
		{"backend is not the ID", goodType{wrongBackend: true}, `want the ID "good"`},
		{"undeclared option", goodType{extraOption: true}, `option "undeclared"`},
		{"accepts anything", goodType{lenient: true}, "want a *domain.ValidationError"},
		{"obscure key missing", goodType{noObscure: true}, "not listed in ObscureKeys"},
		{"not deterministic", goodType{randomRoot: true, calls: &calls}, "not deterministic"},
	} {
		var r recorder
		backendtest.Check(&r, tt.typ)
		if !slices.ContainsFunc(r.errs, func(e string) bool { return strings.Contains(e, tt.want) }) {
			t.Errorf("%s: failures %q do not mention %q", tt.name, r.errs, tt.want)
		}
	}

	for _, tt := range []struct {
		name string
		ex   backend.InvalidExample
		want string
	}{
		{"accepted", backend.InvalidExample{Name: "fine", Config: backend.Config{Options: map[string]string{"host": "h"}}, Field: "options.host"},
			`invalid example "fine": Validate = <nil>`},
		{"another field", backend.InvalidExample{Name: "unknown", Config: backend.Config{Options: map[string]string{"host": "h", "zz": "v"}}, Field: "options.host"},
			"do not name options.host"},
		{"no field", backend.InvalidExample{Name: "unnamed", Config: backend.Config{}}, "names no field"},
		{"echoed value", backend.InvalidExample{Name: "echo", Config: backend.Config{Options: map[string]string{"host": "echo-me-please"}}, Field: "options.host"},
			`contains the value "echo-me-please"`},
	} {
		var r recorder
		backendtest.Check(&r, invalidType{ex: tt.ex})
		if !slices.ContainsFunc(r.errs, func(e string) bool { return strings.Contains(e, tt.want) }) {
			t.Errorf("%s: failures %q do not mention %q", tt.name, r.errs, tt.want)
		}
	}
	var ok recorder
	backendtest.Check(&ok, invalidType{ex: backend.InvalidExample{Name: "missing host", Config: backend.Config{}, Field: "options.host"}})
	if len(ok.errs) != 0 {
		t.Errorf("a rejected invalid example fails: %v", ok.errs)
	}
	var objectLocal recorder
	backendtest.Check(&objectLocal, objectStoreType{goodType{local: true}})
	if !slices.ContainsFunc(objectLocal.errs, func(e string) bool { return strings.Contains(e, "ObjectStore on a local") }) {
		t.Errorf("an object store that is local passes: %q", objectLocal.errs)
	}

	var r recorder
	backendtest.Check(&r, goodType{}, backendtest.WithAccept(func(backend.Rclone) error { return errors.New("rejected") }))
	if !slices.ContainsFunc(r.errs, func(e string) bool { return strings.Contains(e, "not accepted: rejected") }) {
		t.Errorf("WithAccept errors are not reported: %q", r.errs)
	}
}

// invalidType is goodType with one invalid example; its Validate echoes
// the host when it is "echo-me-please".
type invalidType struct {
	goodType
	ex backend.InvalidExample
}

func (i invalidType) InvalidExamples() []backend.InvalidExample {
	return []backend.InvalidExample{i.ex}
}

func (i invalidType) Validate(cfg backend.Config) error {
	if h := cfg.Options["host"]; h == "echo-me-please" {
		return &domain.ValidationError{Fields: []domain.FieldError{{Field: "options.host", Message: "bad host " + h}}}
	}
	return i.goodType.Validate(cfg)
}

// objectStoreType is goodType claiming to be an object store.
type objectStoreType struct{ goodType }

func (o objectStoreType) Capabilities() backend.Capabilities {
	c := o.goodType.Capabilities()
	c.ObjectStore = true
	return c
}
