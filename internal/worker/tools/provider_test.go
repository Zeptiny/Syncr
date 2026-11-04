package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// chain builds the four standard providers around temp directories and a
// content-parsing detector, with the managed one able to download.
type chain struct {
	srv        *archiveServer
	spec       Spec
	det        *contentDetector
	bundledDir string
	pathDir    string
	toolsDir   string
	configured map[string]string
}

func newChain(t *testing.T) *chain {
	t.Helper()
	srv, spec := zipFixture(t)
	return &chain{
		srv: srv, spec: spec, det: &contentDetector{},
		bundledDir: t.TempDir(), pathDir: t.TempDir(), toolsDir: t.TempDir(),
		configured: map[string]string{},
	}
}

func (c *chain) resolver(t *testing.T, download bool) *Resolver {
	t.Helper()
	inst := newTestInstaller(t, c.srv, c.toolsDir, c.det)
	lookPath := func(name string) (string, error) {
		p := filepath.Join(c.pathDir, c.spec.Binary(host.OS))
		if _, err := os.Stat(p); err != nil {
			return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
		}
		return p, nil
	}
	r, err := NewResolver(Manifest{Tools: []Spec{c.spec}},
		NewConfiguredProvider(c.configured, c.det),
		NewBundledProvider(c.bundledDir, c.det),
		NewManagedProvider(inst, host, download),
		NewPathProvider(lookPath, c.det),
	)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestProviderOrdering(t *testing.T) {
	t.Parallel()
	good, other := fakeBinary(fakeVersion), fakeBinary("0.0.1")

	tests := []struct {
		name       string
		configured []byte // content of the configured binary; nil = not configured
		bundled    []byte
		onPath     []byte
		download   bool
		wantSource Source
		wantErr    bool
		wantReject []Source // sources listed in Resolved.Rejected
		wantHits   int32
	}{
		{name: "configured wins over everything", configured: good, bundled: good, onPath: good, download: true, wantSource: SourceConfigured},
		{name: "bundled when nothing configured", bundled: good, onPath: good, download: true, wantSource: SourceBundled},
		{name: "managed download before PATH", onPath: good, download: true, wantSource: SourceManaged, wantHits: 1},
		{name: "PATH when downloads are off", onPath: good, wantSource: SourcePath},
		{name: "wrong-version configured falls through and is reported", configured: other, bundled: good, wantSource: SourceBundled, wantReject: []Source{SourceConfigured}},
		{name: "wrong-version bundled and PATH, download rescues", bundled: other, onPath: other, download: true, wantSource: SourceManaged, wantHits: 1, wantReject: []Source{SourceBundled}},
		{name: "PATH binary with wrong version is rejected", onPath: other, wantErr: true},
		{name: "nothing anywhere", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newChain(t)
			if tt.configured != nil {
				c.configured[fakeName] = writeFile(t, t.TempDir(), "mytool", tt.configured)
			}
			if tt.bundled != nil {
				writeFile(t, c.bundledDir, c.spec.Binary(host.OS), tt.bundled)
			}
			if tt.onPath != nil {
				writeFile(t, c.pathDir, c.spec.Binary(host.OS), tt.onPath)
			}

			got, err := c.resolver(t, tt.download).Resolve(t.Context(), fakeName)
			if tt.wantErr {
				var re *ResolveError
				if !errors.As(err, &re) {
					t.Fatalf("err = %v, want *ResolveError", err)
				}
				if tt.onPath != nil {
					if _, ok := errors.AsType[*VersionMismatchError](err); !ok {
						t.Errorf("PATH rejection is not a *VersionMismatchError: %v", err)
					}
				}
				if len(re.Rejected) != 4 {
					t.Errorf("ResolveError lists %d providers, want 4", len(re.Rejected))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Source != tt.wantSource || got.Version != fakeVersion || got.Tool != fakeName {
				t.Errorf("resolved %+v, want source %s", got, tt.wantSource)
			}
			if !filepath.IsAbs(got.Path) {
				t.Errorf("Path %q is not absolute", got.Path)
			}
			if raw, _ := os.ReadFile(got.Path); !strings.HasPrefix(string(raw), "faketool v"+fakeVersion) {
				t.Errorf("resolved path holds %q", raw)
			}
			var rejected []Source
			for _, r := range got.Rejected {
				rejected = append(rejected, r.Source)
			}
			if strings.Join(sourcesToStrings(rejected), ",") != strings.Join(sourcesToStrings(tt.wantReject), ",") {
				t.Errorf("Rejected = %v, want %v", rejected, tt.wantReject)
			}
			if c.srv.hits.Load() != tt.wantHits {
				t.Errorf("downloads = %d, want %d", c.srv.hits.Load(), tt.wantHits)
			}
		})
	}
}

func sourcesToStrings(s []Source) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = string(v)
	}
	return out
}

func TestManagedProviderWithoutDownloadNeverTouchesNetwork(t *testing.T) {
	t.Parallel()
	c := newChain(t)
	r := c.resolver(t, false)
	_, err := r.Resolve(t.Context(), fakeName)
	if !errors.Is(err, ErrNotFound) || !errors.Is(err, ErrNotInstalled) {
		t.Errorf("err = %v", err)
	}
	if c.srv.hits.Load() != 0 {
		t.Error("network used with downloads disabled")
	}

	// Once something installed it, the non-downloading chain finds it.
	if _, err := newTestInstaller(t, c.srv, c.toolsDir, c.det).Install(t.Context(), c.spec, host); err != nil {
		t.Fatal(err)
	}
	got, err := r.Resolve(t.Context(), fakeName)
	if err != nil || got.Source != SourceManaged {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestManagedProviderRepairsTamperedInstall(t *testing.T) {
	t.Parallel()
	c := newChain(t)
	r := c.resolver(t, true)
	first, err := r.Resolve(t.Context(), fakeName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first.Path, fakeBinary(fakeVersion)[:10], 0o755); err != nil {
		t.Fatal(err)
	}
	second, err := r.Resolve(t.Context(), fakeName)
	if err != nil || second.Source != SourceManaged {
		t.Fatalf("got %+v, %v", second, err)
	}
	if c.srv.hits.Load() != 2 {
		t.Errorf("downloads = %d, want 2 (a tampered binary must be replaced)", c.srv.hits.Load())
	}
}

func TestConfiguredPathErrorsAreNotAbsence(t *testing.T) {
	t.Parallel()
	c := newChain(t)
	c.configured[fakeName] = filepath.Join(t.TempDir(), "does-not-exist")
	_, err := NewConfiguredProvider(c.configured, c.det).Resolve(t.Context(), c.spec)
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("a typo'd configured path must be an error, got %v", err)
	}
	// A directory is not a binary either.
	c.configured[fakeName] = t.TempDir()
	if _, err := NewConfiguredProvider(c.configured, c.det).Resolve(t.Context(), c.spec); err == nil {
		t.Error("directory accepted as a binary")
	}
	// Empty values mean "not configured".
	_, err = NewConfiguredProvider(map[string]string{fakeName: ""}, c.det).Resolve(t.Context(), c.spec)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("empty path: err = %v", err)
	}
}

func TestResolverEdgeCases(t *testing.T) {
	t.Parallel()
	c := newChain(t)
	r := c.resolver(t, true)

	if _, err := r.Resolve(t.Context(), "nope"); !errors.Is(err, ErrUnknownTool) {
		t.Errorf("unknown tool: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.Resolve(ctx, fakeName); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	if c.srv.hits.Load() != 0 {
		t.Error("downloaded after cancellation")
	}

	if _, err := NewResolver(Manifest{}); err == nil {
		t.Error("NewResolver accepted an empty manifest")
	}
}

func TestTraceReportsEveryProvider(t *testing.T) {
	t.Parallel()
	c := newChain(t)
	writeFile(t, c.pathDir, c.spec.Binary(host.OS), fakeBinary("0.0.1"))
	writeFile(t, c.bundledDir, c.spec.Binary(host.OS), fakeBinary(fakeVersion))

	attempts, err := c.resolver(t, false).Trace(t.Context(), fakeName)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		source Source
		ok     bool
	}{{SourceConfigured, false}, {SourceBundled, true}, {SourceManaged, false}, {SourcePath, false}}
	if len(attempts) != len(want) {
		t.Fatalf("%d attempts", len(attempts))
	}
	for i, w := range want {
		if attempts[i].Source != w.source || (attempts[i].Err == nil) != w.ok {
			t.Errorf("attempt %d = %+v, want source %s ok=%v", i, attempts[i], w.source, w.ok)
		}
	}
	if _, ok := errors.AsType[*VersionMismatchError](attempts[3].Err); !ok {
		t.Errorf("PATH attempt error = %v", attempts[3].Err)
	}
	if _, err := c.resolver(t, false).Trace(t.Context(), "nope"); !errors.Is(err, ErrUnknownTool) {
		t.Errorf("Trace unknown tool: %v", err)
	}
}

func TestPathProviderLookupErrors(t *testing.T) {
	t.Parallel()
	c := newChain(t)
	boom := errors.New("relative result")
	_, err := NewPathProvider(func(string) (string, error) { return "", boom }, c.det).Resolve(t.Context(), c.spec)
	if !errors.Is(err, boom) || errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
	_, err = NewPathProvider(nil, c.det).Resolve(t.Context(), Spec{Name: "syncr-surely-not-installed-tool"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("default lookup of a missing tool: %v", err)
	}
}
