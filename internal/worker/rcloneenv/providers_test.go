package rcloneenv

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"slices"
	"testing"
)

// RcloneBackend is a backend of the pinned rclone, as "rclone config
// providers" lists it. It is exported for this package's external tests
// (backend_accept_test.go).
type RcloneBackend struct {
	// Name is rclone's name of the backend ("google cloud storage"),
	// Prefix the type a config uses ("gcs"); most backends have one
	// value for both.
	Name, Prefix string
	Options      []RcloneOption
}

// RcloneOption is an option of an RcloneBackend.
type RcloneOption struct {
	Name string
	// Examples are the values rclone suggests (for an s3 provider, every
	// provider it knows).
	Examples []struct{ Value string }
}

// OptionNames returns the names of b's options.
func (b RcloneBackend) OptionNames() []string {
	names := make([]string, len(b.Options))
	for i, o := range b.Options {
		names[i] = o.Name
	}
	return names
}

// Option returns b's option name and whether it has one.
func (b RcloneBackend) Option(name string) (RcloneOption, bool) {
	i := slices.IndexFunc(b.Options, func(o RcloneOption) bool { return o.Name == name })
	if i < 0 {
		return RcloneOption{}, false
	}
	return b.Options[i], true
}

// RcloneBackends runs "rclone config providers" with the pinned rclone
// (SYNCR_TEST_RCLONE) and returns its backends indexed by Name and by
// Prefix, so a backend type such as "gcs" (Name "google cloud storage")
// finds its options. It skips the test when SYNCR_TEST_RCLONE is not set.
func RcloneBackends(t *testing.T) map[string]RcloneBackend {
	t.Helper()
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
	var list []RcloneBackend
	if err := json.Unmarshal(out, &list); err != nil {
		t.Fatalf("decode rclone config providers: %v", err)
	}
	backends := make(map[string]RcloneBackend, 2*len(list))
	for _, b := range list {
		for _, key := range slices.Compact([]string{b.Name, b.Prefix}) {
			if key == "" {
				continue
			}
			if prev, dup := backends[key]; dup {
				t.Fatalf("rclone config providers: %q names both backend %q and backend %q", key, prev.Name, b.Name)
			}
			backends[key] = b
		}
	}
	return backends
}
