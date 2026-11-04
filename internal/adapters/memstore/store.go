// Package memstore is an in-memory implementation of the controller's
// repository ports (workers.Repository, workers.TokenRepository,
// jobs.Repository, jobs.SampleRepository). It backs unit tests and a
// development controller without Postgres (dev.memory_store); everything is
// lost when the process exits. The live job broker is a separate adapter
// (internal/adapters/broker), used by every controller.
//
// It follows the ports' contracts exactly, including the conditional
// updates, so tests against it exercise the same concurrency rules a
// database adapter must provide. A single mutex serializes the store, which
// makes every method trivially atomic; values are deep-copied in and out, so
// callers never share memory with the store.
//
// Allowed dependencies: the standard library, internal/domain and the
// internal/app packages whose ports it implements.
package memstore

import (
	"cmp"
	"slices"
	"sync"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Store holds workers, enrollment tokens, jobs and samples. The zero value
// is not usable; create it with New. Safe for concurrent use.
type Store struct {
	mu      sync.RWMutex
	workers map[domain.WorkerID]workers.Worker
	tokens  map[workers.TokenHash]workers.EnrollmentToken
	jobs    map[domain.JobID]jobs.Job
	// samples per job, sorted by Time.
	samples map[domain.JobID][]jobs.Sample
}

// Compile-time checks that Store implements the ports.
var (
	_ workers.Repository      = (*Store)(nil)
	_ workers.TokenRepository = (*Store)(nil)
	_ jobs.Repository         = (*Store)(nil)
	_ jobs.SampleRepository   = (*Store)(nil)
)

// New returns an empty Store.
func New() *Store {
	return &Store{
		workers: make(map[domain.WorkerID]workers.Worker),
		tokens:  make(map[workers.TokenHash]workers.EnrollmentToken),
		jobs:    make(map[domain.JobID]jobs.Job),
		samples: make(map[domain.JobID][]jobs.Sample),
	}
}

func cloneWorker(w workers.Worker) workers.Worker {
	w.SecretEnc = slices.Clone(w.SecretEnc)
	w.Info.Tools = slices.Clone(w.Info.Tools)
	w.Info.JobKinds = slices.Clone(w.Info.JobKinds)
	w.Info.LocalRoots = slices.Clone(w.Info.LocalRoots)
	w.Link.PastEpochs = slices.Clone(w.Link.PastEpochs)
	return w
}

func cloneJob(j jobs.Job) jobs.Job {
	j.SpecEnc = slices.Clone(j.SpecEnc)
	j.Warnings = slices.Clone(j.Warnings)
	j.Output = slices.Clone(j.Output)
	return j
}

// compareIDs orders IDs by their bytes (UUIDv7: creation order).
func compareIDs[K any](a, b domain.ID[K]) int {
	ua, ub := a.UUID(), b.UUID()
	for i := range ua {
		if c := cmp.Compare(ua[i], ub[i]); c != 0 {
			return c
		}
	}
	return 0
}
