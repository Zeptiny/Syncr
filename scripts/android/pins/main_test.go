package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/worker/tools"
)

// TestWriteDefaultManifest pins the output build-worker.sh parses: exactly
// one "<tool> <version>" line for rclone and for restic, with the versions
// of the manifest.
func TestWriteDefaultManifest(t *testing.T) {
	t.Parallel()
	m := tools.DefaultManifest()
	var out strings.Builder
	if err := write(&out, m); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for line := range strings.Lines(out.String()) {
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.HasSuffix(line, "\n") {
			t.Fatalf("line %q is not \"<tool> <version>\\n\"", line)
		}
		got[fields[0]] = fields[1]
	}
	if len(got) != 2 {
		t.Fatalf("tools = %v, want rclone and restic", got)
	}
	for _, name := range []string{"rclone", "restic"} {
		spec, ok := m.Lookup(name)
		if !ok {
			t.Fatalf("the manifest has no %s", name)
		}
		if got[name] != spec.Version {
			t.Errorf("%s = %q, want the manifest's %q", name, got[name], spec.Version)
		}
	}
}

func TestWriteRefusesOtherShapes(t *testing.T) {
	t.Parallel()
	for name, m := range map[string]tools.Manifest{
		"empty":            {},
		"leading v":        {Tools: []tools.Spec{{Name: "rclone", Version: "v1.2.3"}}},
		"suffix":           {Tools: []tools.Spec{{Name: "rclone", Version: "1.2.3-beta"}}},
		"space in version": {Tools: []tools.Spec{{Name: "rclone", Version: "1.2.3 x"}}},
		"odd name":         {Tools: []tools.Spec{{Name: "Rclone", Version: "1.2.3"}}},
	} {
		if err := write(&strings.Builder{}, m); !errors.Is(err, errBadPin) {
			t.Errorf("%s: err = %v, want errBadPin", name, err)
		}
	}
}
