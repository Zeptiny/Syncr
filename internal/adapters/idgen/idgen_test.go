package idgen

import (
	"sync"
	"testing"
)

func TestUUIDv7(t *testing.T) {
	t.Parallel()
	var g UUIDv7
	a, b := g.NewJobID(), g.NewJobID()
	if a.IsZero() || a == b || a.String() >= b.String() {
		t.Errorf("job ids %v, %v: want distinct, increasing", a, b)
	}
	if g.NewWorkerID().IsZero() || g.NewUserID().IsZero() || g.NewAPITokenID().IsZero() || g.NewRemoteID().IsZero() {
		t.Error("zero id")
	}
}

func TestSequential(t *testing.T) {
	t.Parallel()
	var g Sequential
	if got := g.NewWorkerID().String(); got != "00000000-0000-7000-8000-000000000001" {
		t.Errorf("first id = %s", got)
	}
	if got := g.NewJobID().String(); got != "00000000-0000-7000-8000-000000000002" {
		t.Errorf("second id = %s", got)
	}
	if u, k, r := g.NewUserID(), g.NewAPITokenID(), g.NewRemoteID(); u.String() != "00000000-0000-7000-8000-000000000003" ||
		k.String() != "00000000-0000-7000-8000-000000000004" || r.String() != "00000000-0000-7000-8000-000000000005" {
		t.Errorf("user, token, remote ids = %s, %s, %s", u, k, r)
	}

	// Concurrent use yields distinct IDs.
	var wg sync.WaitGroup
	ids := make(chan string, 100)
	for range 100 {
		wg.Go(func() { ids <- g.NewJobID().String() })
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate id %s", id)
		}
		seen[id] = true
	}
}

func TestMilestone4IDs(t *testing.T) {
	t.Parallel()
	var g UUIDv7
	if g.NewScheduleID().IsZero() || g.NewScheduleRunID().IsZero() || g.NewContactID().IsZero() || g.NewDeliveryID().IsZero() {
		t.Error("zero id")
	}
	var s Sequential
	got := []string{s.NewScheduleID().String(), s.NewScheduleRunID().String(), s.NewContactID().String(), s.NewDeliveryID().String()}
	for i, id := range got {
		if want := "00000000-0000-7000-8000-00000000000" + string(rune('1'+i)); id != want {
			t.Errorf("id %d = %s, want %s", i, id, want)
		}
	}
}

func TestMilestone5IDs(t *testing.T) {
	t.Parallel()
	var g UUIDv7
	if g.NewResticRepoID().IsZero() || g.NewProbeID().IsZero() {
		t.Error("zero id")
	}
	var s Sequential
	if r, p := s.NewResticRepoID().String(), s.NewProbeID().String(); r != "00000000-0000-7000-8000-000000000001" ||
		p != "00000000-0000-7000-8000-000000000002" {
		t.Errorf("repository, probe ids = %s, %s", r, p)
	}
}

func TestEnrollmentTokenIDs(t *testing.T) {
	t.Parallel()
	var g UUIDv7
	if g.NewEnrollmentTokenID().IsZero() {
		t.Error("zero id")
	}
	var s Sequential
	if got := s.NewEnrollmentTokenID().String(); got != "00000000-0000-7000-8000-000000000001" {
		t.Errorf("first enrollment token id = %s", got)
	}
}
