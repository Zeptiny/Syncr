package rcloneenv_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/builtin"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
)

// This file cross-checks the controller's backend descriptors with the
// worker's security boundary: every built-in type's examples, mapped to
// rclone remotes and turned into RemoteDefinitions the way the job builder
// does it, must be accepted by rcloneenv.
//
// toDefinition and upstream mirror the alias and Root mapping of
// internal/jobkind (aliases "r1", "r2", ... ; backend.JoinRemote for remote
// paths and backend.JoinLocal for local ones), so this test does not depend
// on a job kind.

// anyLocal accepts every local path, standing in for the worker's
// local_roots policy.
type anyLocal struct{}

func (anyLocal) Resolve(path string) (string, error) { return path, nil }

// toDefinition is the RemoteDefinition of r under alias.
func toDefinition(alias string, r backend.Rclone) *syncrv1.RemoteDefinition {
	return &syncrv1.RemoteDefinition{
		Alias: alias, BackendType: r.Backend,
		Options: maps.Clone(r.Options), Secrets: maps.Clone(r.Secrets), ObscureKeys: slices.Clone(r.ObscureKeys),
	}
}

// upstream is the union upstream of a member r (alias "" for a local one)
// at path.
func upstream(t *testing.T, alias string, r backend.Rclone, path string, mode syncrv1.UnionUpstreamMode) *syncrv1.UnionUpstream {
	t.Helper()
	join := backend.JoinRemote
	if r.Backend == "" {
		join, alias = backend.JoinLocal, ""
	}
	p, err := join(r.Root, path)
	if err != nil {
		t.Fatalf("join %q and %q: %v", r.Root, path, err)
	}
	return &syncrv1.UnionUpstream{Location: &syncrv1.Location{Remote: alias, Path: p}, Mode: mode}
}

type example struct {
	name   string
	typ    backend.Type
	rclone backend.Rclone
}

// examples maps every example of every built-in type.
func examples(t *testing.T) []example {
	t.Helper()
	var out []example
	for _, typ := range builtin.Types() {
		ex, ok := typ.(backend.Exampler)
		if !ok {
			t.Errorf("built-in type %q has no examples", typ.ID())
			continue
		}
		for i, cfg := range ex.Examples() {
			name := fmt.Sprintf("%s/%d", typ.ID(), i)
			if err := typ.Validate(cfg); err != nil {
				t.Fatalf("%s: Validate: %v", name, err)
			}
			r, err := typ.ToRclone(cfg)
			if err != nil {
				t.Fatalf("%s: ToRclone: %v", name, err)
			}
			out = append(out, example{name: name, typ: typ, rclone: r})
		}
	}
	return out
}

func TestBuiltinBackendsAccepted(t *testing.T) {
	t.Parallel()
	exs := examples(t)
	var localMember *example
	for i := range exs {
		if exs[i].typ.Capabilities().Local && localMember == nil {
			localMember = &exs[i]
		}
	}
	if localMember == nil {
		t.Fatal("no local example to use as a union member")
	}
	builder := rcloneenv.NewBuilder(anyLocal{})

	for _, ex := range exs {
		caps := ex.typ.Capabilities()
		switch {
		case caps.Local:
			// A local remote is no RemoteDefinition: its locations are
			// local paths. It must still make a valid local upstream.
			if ex.rclone.Backend != "" {
				t.Errorf("%s: a local type maps to backend %q", ex.name, ex.rclone.Backend)
			}
			if up := upstream(t, "", ex.rclone, "sub/dir", syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_WRITE); up.GetLocation().GetPath() == "" {
				t.Errorf("%s: empty local path", ex.name)
			}
		case caps.Union:
			// A union of every non-union member kind: one remote member and
			// one local member.
			for _, m := range exs {
				mc := m.typ.Capabilities()
				if mc.Local || mc.Union || !mc.UnionMember {
					continue
				}
				u := toDefinition("r1", ex.rclone)
				u.UnionUpstreams = []*syncrv1.UnionUpstream{
					upstream(t, "r2", m.rclone, "pool", syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_WRITE),
					upstream(t, "", localMember.rclone, "pool", syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_ONLY),
				}
				if _, err := builder.Remotes([]*syncrv1.RemoteDefinition{u, toDefinition("r2", m.rclone)}); err != nil {
					t.Errorf("%s with member %s: rejected by the worker: %v", ex.name, m.name, err)
				}
			}
		default:
			if _, err := builder.Remotes([]*syncrv1.RemoteDefinition{toDefinition("r1", ex.rclone)}); err != nil {
				t.Errorf("%s: rejected by the worker: %v", ex.name, err)
			}
		}
	}
}

// testOnlyBackends are supported by the worker without a built-in type.
func testOnlyBackends() []string { return []string{"memory"} }

// TestSupportedBackendsMatchBuiltinTypes checks that the worker's allowlist
// of backend types holds the rclone backend of every built-in type, and
// nothing else beyond testOnlyBackends: adding a type to one list without
// the other (and without reviewing its options) fails here.
func TestSupportedBackendsMatchBuiltinTypes(t *testing.T) {
	t.Parallel()
	want := map[string]bool{}
	for _, b := range testOnlyBackends() {
		want[b] = true
	}
	for _, ex := range examples(t) {
		if ex.rclone.Backend != "" {
			want[ex.rclone.Backend] = true
		}
	}
	supported := rcloneenv.SupportedBackends()
	for b := range want {
		if !slices.Contains(supported, b) {
			t.Errorf("rclone backend %q of a built-in type is not in rcloneenv.SupportedBackends; review its options (deniedOptions) and add it", b)
		}
	}
	for _, b := range supported {
		if !want[b] {
			t.Errorf("rcloneenv.SupportedBackends has %q, which no built-in type uses", b)
		}
	}
}

// TestBuiltinBackendOptionsKnownToRclone checks with the real rclone
// (SYNCR_TEST_RCLONE) that every option and secret the descriptors emit is
// an option of their rclone backend, so a misspelled option name cannot
// slip through (rclone ignores unknown RCLONE_CONFIG_* variables).
func TestBuiltinBackendOptionsKnownToRclone(t *testing.T) {
	t.Parallel()
	bin := os.Getenv("SYNCR_TEST_RCLONE")
	if bin == "" {
		t.Skip("SYNCR_TEST_RCLONE not set")
	}
	out, err := exec.CommandContext(t.Context(), bin, "config", "providers").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("rclone config providers: %v: %s", err, ee.Stderr)
		}
		t.Fatalf("rclone config providers: %v", err)
	}
	var providers []struct {
		Name    string
		Options []struct{ Name string }
	}
	if err := json.Unmarshal(out, &providers); err != nil {
		t.Fatalf("decode rclone config providers: %v", err)
	}
	known := map[string]map[string]bool{}
	for _, p := range providers {
		known[p.Name] = map[string]bool{}
		for _, o := range p.Options {
			known[p.Name][o.Name] = true
		}
	}
	for _, ex := range examples(t) {
		if ex.rclone.Backend == "" {
			continue
		}
		opts, ok := known[ex.rclone.Backend]
		if !ok {
			t.Errorf("%s: rclone has no backend %q", ex.name, ex.rclone.Backend)
			continue
		}
		for _, k := range slices.Concat(slices.Collect(maps.Keys(ex.rclone.Options)), slices.Collect(maps.Keys(ex.rclone.Secrets))) {
			if !opts[strings.ToLower(k)] {
				t.Errorf("%s: %q is not an option of rclone backend %q", ex.name, k, ex.rclone.Backend)
			}
		}
	}
}
