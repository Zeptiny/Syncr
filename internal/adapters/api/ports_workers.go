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
	GetEnrollmentToken(ctx context.Context, id domain.EnrollmentTokenID) (workers.EnrollmentToken, error)
	// TokenState derives an enrollment token's state as of now.
	TokenState(t workers.EnrollmentToken) workers.TokenState
	RegisterPassive(ctx context.Context, p workers.PassiveParams) (workers.Worker, error)
	List(ctx context.Context, f workers.ListFilter) ([]workers.Worker, error)
	Get(ctx context.Context, id domain.WorkerID) (workers.Worker, error)
	Rename(ctx context.Context, id domain.WorkerID, name string) (workers.Worker, error)
	// SetLabels replaces a worker's labels (normalized by the service; an
	// invalid one is a *domain.ValidationError on "labels[i]").
	SetLabels(ctx context.Context, id domain.WorkerID, labels []string) (workers.Worker, error)
	Remove(ctx context.Context, id domain.WorkerID) error
	// Status derives a worker's connection status as of now.
	Status(w workers.Worker) domain.WorkerStatus
}

var _ WorkerService = (*workers.Service)(nil)
