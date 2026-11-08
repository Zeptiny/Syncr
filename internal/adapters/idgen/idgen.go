// Package idgen mints entity IDs for the controller's use cases: UUIDv7 for
// production, Sequential for tests and fixtures. Both satisfy the
// IDGenerator ports of internal/app/... (NewWorkerID, NewJobID, NewUserID,
// NewAPITokenID, NewRemoteID, NewScheduleID, NewScheduleRunID,
// NewContactID, NewDeliveryID).
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

// NewUserID returns a new random UserID.
func (UUIDv7) NewUserID() domain.UserID { return domain.NewUserID() }

// NewAPITokenID returns a new random APITokenID.
func (UUIDv7) NewAPITokenID() domain.APITokenID { return domain.NewAPITokenID() }

// NewRemoteID returns a new random RemoteID.
func (UUIDv7) NewRemoteID() domain.RemoteID { return domain.NewRemoteID() }

// NewScheduleID returns a new random ScheduleID.
func (UUIDv7) NewScheduleID() domain.ScheduleID { return domain.NewScheduleID() }

// NewScheduleRunID returns a new random ScheduleRunID.
func (UUIDv7) NewScheduleRunID() domain.ScheduleRunID { return domain.NewScheduleRunID() }

// NewContactID returns a new random ContactID.
func (UUIDv7) NewContactID() domain.ContactID { return domain.NewContactID() }

// NewDeliveryID returns a new random DeliveryID.
func (UUIDv7) NewDeliveryID() domain.DeliveryID { return domain.NewDeliveryID() }

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

// NewUserID returns the next UserID.
func (s *Sequential) NewUserID() domain.UserID { return mint(domain.ParseUserID, s.next()) }

// NewAPITokenID returns the next APITokenID.
func (s *Sequential) NewAPITokenID() domain.APITokenID { return mint(domain.ParseAPITokenID, s.next()) }

// NewRemoteID returns the next RemoteID.
func (s *Sequential) NewRemoteID() domain.RemoteID { return mint(domain.ParseRemoteID, s.next()) }

// NewScheduleID returns the next ScheduleID.
func (s *Sequential) NewScheduleID() domain.ScheduleID { return mint(domain.ParseScheduleID, s.next()) }

// NewScheduleRunID returns the next ScheduleRunID.
func (s *Sequential) NewScheduleRunID() domain.ScheduleRunID {
	return mint(domain.ParseScheduleRunID, s.next())
}

// NewContactID returns the next ContactID.
func (s *Sequential) NewContactID() domain.ContactID { return mint(domain.ParseContactID, s.next()) }

// NewDeliveryID returns the next DeliveryID.
func (s *Sequential) NewDeliveryID() domain.DeliveryID { return mint(domain.ParseDeliveryID, s.next()) }

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
