// Package idgen mints entity IDs for the controller's use cases: UUIDv7 for
// production, Sequential for tests and fixtures. Both satisfy the
// IDGenerator ports of internal/app/... (NewWorkerID, NewJobID).
//
// Allowed dependencies: the standard library and internal/domain.
package idgen

import (
	"fmt"
	"sync/atomic"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// UUIDv7 mints random, time-ordered IDs (domain.New*). The zero value is
// ready to use.
type UUIDv7 struct{}

// NewWorkerID returns a new random WorkerID.
func (UUIDv7) NewWorkerID() domain.WorkerID { return domain.NewWorkerID() }

// NewJobID returns a new random JobID.
func (UUIDv7) NewJobID() domain.JobID { return domain.NewJobID() }

// Sequential mints predictable, strictly increasing IDs of the form
// 00000000-0000-7000-8000-00000000000n, sharing one counter across entity
// kinds. The zero value starts at 1. Safe for concurrent use.
type Sequential struct {
	n atomic.Uint64
}

// NewWorkerID returns the next WorkerID.
func (s *Sequential) NewWorkerID() domain.WorkerID { return mint(domain.ParseWorkerID, s.next()) }

// NewJobID returns the next JobID.
func (s *Sequential) NewJobID() domain.JobID { return mint(domain.ParseJobID, s.next()) }

func (s *Sequential) next() string {
	return fmt.Sprintf("00000000-0000-7000-8000-%012x", s.n.Add(1))
}

func mint[T any](parse func(string) (T, error), s string) T {
	id, err := parse(s)
	if err != nil {
		// Unreachable: next always produces a valid, non-nil UUID.
		panic(fmt.Sprintf("idgen: minted invalid id %q: %v", s, err))
	}
	return id
}
