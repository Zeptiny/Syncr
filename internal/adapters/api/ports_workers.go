package api

import (
	"context"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// WorkerService is the part of the workers use cases the API calls
// (implemented by *workers.Service). Workers are shared by all users; the
// policy table restricts their management to admins.
type WorkerService interface {
	CreateEnrollmentToken(ctx context.Context, ttl time.Duration) (workers.EnrollmentGrant, error)
	RegisterPassive(ctx context.Context, p workers.PassiveParams) (workers.Worker, error)
	List(ctx context.Context, f workers.ListFilter) ([]workers.Worker, error)
	Get(ctx context.Context, id domain.WorkerID) (workers.Worker, error)
	Rename(ctx context.Context, id domain.WorkerID, name string) (workers.Worker, error)
	Remove(ctx context.Context, id domain.WorkerID) error
	// Status derives a worker's connection status as of now.
	Status(w workers.Worker) domain.WorkerStatus
}

var _ WorkerService = (*workers.Service)(nil)
