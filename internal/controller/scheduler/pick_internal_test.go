package scheduler

import (
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/domain"
)

func TestPick(t *testing.T) {
	t.Parallel()
	id := func(n string) domain.WorkerID {
		w, err := domain.ParseWorkerID("00000000-0000-7000-8000-00000000000" + n)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	tests := []struct {
		name  string
		cands []candidate
		want  domain.WorkerID
		ok    bool
	}{
		{"none", nil, domain.WorkerID{}, false},
		{"least busy", []candidate{{id("1"), "a", 3}, {id("2"), "b", 0}, {id("3"), "c", 1}}, id("2"), true},
		{"tie by name, case-insensitive", []candidate{{id("1"), "Zulu", 1}, {id("2"), "alpha", 1}, {id("3"), "Bravo", 1}}, id("2"), true},
		{"tie by name then ID", []candidate{{id("2"), "nas", 0}, {id("1"), "NAS", 0}}, id("1"), true},
	}
	for _, tt := range tests {
		got, ok := pick(tt.cands)
		if ok != tt.ok || got.ID != tt.want {
			t.Errorf("%s: pick = (%v, %v), want (%v, %v)", tt.name, got.ID, ok, tt.want, tt.ok)
		}
	}
}

func TestClip(t *testing.T) {
	t.Parallel()
	if got := clip("short"); got != "short" {
		t.Errorf("clip(short) = %q", got)
	}
	long := strings.Repeat("é", maxMessage) // 2 bytes each
	got := clip(long)
	if len(got) > maxMessage+len("…") || !strings.HasSuffix(got, "…") || strings.ContainsRune(got, '�') {
		t.Errorf("clip(long) = %d bytes", len(got))
	}
	for _, r := range strings.TrimSuffix(got, "…") {
		if r != 'é' {
			t.Fatalf("clip split a rune: %q", r)
		}
	}
}
