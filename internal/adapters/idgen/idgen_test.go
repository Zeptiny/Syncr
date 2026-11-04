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
	if g.NewWorkerID().IsZero() {
		t.Error("zero worker id")
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
