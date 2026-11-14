package link

import (
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// TestRemoteSourceNames: the tag, the phases and the key length are part
// of the wire contract (and of stored snapshots: the tag outlives builds).
func TestRemoteSourceNames(t *testing.T) {
	t.Parallel()
	if RemoteSourceTagPrefix != "syncr-remote:" || ProgressPhaseMirror != "mirror" || ProgressPhaseBackup != "backup" || SourceKeyLen != 32 {
		t.Error("remote source names changed: they are part of the wire contract")
	}
	id, err := domain.ParseRemoteID("0f8e2c4a-1b3d-4e5f-8a7b-9c0d1e2f3a4b")
	if err != nil {
		t.Fatal(err)
	}
	tag := RemoteSourceTag(id)
	if tag != "syncr-remote:0f8e2c4a-1b3d-4e5f-8a7b-9c0d1e2f3a4b" || len(tag) != 49 {
		t.Errorf("RemoteSourceTag = %q (%d bytes), want the prefix and the ID in 49 bytes", tag, len(tag))
	}
	// A restic tag may not hold a comma (restic splits at commas).
	if strings.Contains(tag, ",") {
		t.Errorf("RemoteSourceTag = %q holds a comma", tag)
	}
}

func TestValidSourceKey(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		key  string
		want bool
	}{
		{"0123456789abcdef0123456789abcdef", true},
		{strings.Repeat("f", 32), true},
		{"", false},
		{strings.Repeat("a", 31), false},
		{strings.Repeat("a", 33), false},
		{strings.Repeat("a", 64), false},             // a bisync state key
		{"0123456789ABCDEF0123456789abcdef", false},  // upper case
		{"0123456789abcdeg0123456789abcdef", false},  // not hex
		{"../../../../../../../../../../..", false},  // 32 bytes of path
		{"0123456789abcdef0123456789abcde/", false},  // separator
		{"0123456789abcdef0123456789abcd\\e", false}, // backslash
	} {
		if got := ValidSourceKey(tt.key); got != tt.want {
			t.Errorf("ValidSourceKey(%q) = %v, want %v", tt.key, got, tt.want)
		}
	}
}
