package storetest

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// This file is the contract suite of the Milestone 4 additions to
// workers.Repository: unique live names and labels.

func testWorkerNamesUniqueAmongLive(t *testing.T, s Store) {
	ctx := context.Background()
	a := newWorker(t, 1)
	a.Name = "NAS-01"
	mustCreateWorker(t, s, a)
	dup := newWorker(t, 2)
	dup.Name = "nas-01"
	wantErr(t, "CreateWorker(name taken, other case)", s.CreateWorker(ctx, dup), domain.ErrAlreadyExists)
	if _, err := s.GetWorker(ctx, dup.ID); err == nil {
		t.Error("a worker with a taken name was stored")
	}
	b := newWorker(t, 3)
	mustCreateWorker(t, s, b)
	_, err := s.RenameWorker(ctx, b.ID, "Nas-01")
	wantErr(t, "RenameWorker(name taken)", err, domain.ErrAlreadyExists)
	if got, err := s.RenameWorker(ctx, a.ID, "nas-01"); err != nil || got.Name != "nas-01" {
		t.Errorf("RenameWorker(own name, other case) = (%q, %v)", got.Name, err)
	}
	// A removed worker's name is free again.
	if _, err := s.RemoveWorker(ctx, a.ID, t0().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	mustCreateWorker(t, s, dup)
	if _, err := s.RenameWorker(ctx, b.ID, "NAS-01"); !errorsIs(err, domain.ErrAlreadyExists) {
		t.Errorf("RenameWorker(name of the new live worker) = %v", err)
	}
}

func testWorkerLabels(t *testing.T, s Store) {
	ctx := context.Background()
	a, b := newWorker(t, 1), newWorker(t, 2)
	mustCreateWorker(t, s, a)
	mustCreateWorker(t, s, b)
	got, err := s.SetWorkerLabels(ctx, a.ID, []string{"linux", "nas"})
	skipIfUnsupported(t, "SetWorkerLabels", "track C (workers)", err)
	if !slices.Equal(got.Labels, []string{"linux", "nas"}) {
		t.Errorf("SetWorkerLabels returned %v", got.Labels)
	}
	if _, err := s.SetWorkerLabels(ctx, b.ID, []string{"nas"}); err != nil {
		t.Fatal(err)
	}
	if w, _ := s.GetWorker(ctx, a.ID); !slices.Equal(w.Labels, []string{"linux", "nas"}) {
		t.Errorf("GetWorker labels = %v", w.Labels)
	}
	byLabel := func(label string) []domain.WorkerID {
		ws, err := s.ListWorkers(ctx, workers.ListFilter{Label: label})
		if err != nil {
			t.Fatalf("ListWorkers(%q): %v", label, err)
		}
		var ids []domain.WorkerID
		for _, w := range ws {
			ids = append(ids, w.ID)
		}
		return ids
	}
	if got := byLabel("nas"); !slices.Equal(got, []domain.WorkerID{a.ID, b.ID}) {
		t.Errorf("label nas: %v", got)
	}
	if got := byLabel("linux"); !slices.Equal(got, []domain.WorkerID{a.ID}) {
		t.Errorf("label linux: %v", got)
	}
	if got := byLabel("windows"); len(got) != 0 {
		t.Errorf("label windows: %v", got)
	}
	// Clearing, aliasing, removed workers.
	labels := []string{"x"}
	if _, err := s.SetWorkerLabels(ctx, b.ID, labels); err != nil {
		t.Fatal(err)
	}
	labels[0] = "mutated"
	if w, _ := s.GetWorker(ctx, b.ID); !slices.Equal(w.Labels, []string{"x"}) {
		t.Errorf("SetWorkerLabels aliases its argument: %v", w.Labels)
	}
	if got, err := s.SetWorkerLabels(ctx, b.ID, nil); err != nil || len(got.Labels) != 0 {
		t.Errorf("clear labels = (%v, %v)", got.Labels, err)
	}
	if _, err := s.RemoveWorker(ctx, a.ID, t0()); err != nil {
		t.Fatal(err)
	}
	_, err = s.SetWorkerLabels(ctx, a.ID, []string{"y"})
	wantErr(t, "SetWorkerLabels(removed)", err, domain.ErrNotFound)
	_, err = s.SetWorkerLabels(ctx, workerID(t, 99), []string{"y"})
	wantErr(t, "SetWorkerLabels(missing)", err, domain.ErrNotFound)
	if got := byLabel("nas"); len(got) != 0 {
		t.Errorf("label nas after remove and clear: %v", got)
	}
}
