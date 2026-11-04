package tools

import (
	"os"
	"testing"

	"github.com/Zeptiny/Syncr/internal/worker/proc"
)

func TestNew(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{}); err == nil {
		t.Error("New accepted a config without a Runner")
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, spec := zipFixture(t)
	runner := helperRunner{sup: proc.New(proc.Options{}), env: map[string]string{helperEnv: "faketool v1.2.3"}}
	r, err := New(Config{
		Manifest:   Manifest{Tools: []Spec{spec}},
		Configured: map[string]string{fakeName: exe},
		BundledDir: t.TempDir(),
		ToolsDir:   t.TempDir(),
		Runner:     runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Resolve(t.Context(), fakeName)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != SourceConfigured || got.Version != fakeVersion || got.Path != exe {
		t.Errorf("resolved %+v", got)
	}

	if _, err := New(Config{Manifest: Manifest{Tools: []Spec{{Name: "BAD"}}}, Runner: runner}); err == nil {
		t.Error("New accepted an invalid manifest")
	}
}
