package api

import (
	"context"

	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// RemoteService is the part of the remotes use cases the API calls,
// implemented by *remotes.Service. See internal/app/remotes for the
// semantics. Backend descriptors come from Deps.Backends.
type RemoteService interface {
	Create(ctx context.Context, actor domain.Actor, in remotes.Input) (remotes.Remote, error)
	Get(ctx context.Context, actor domain.Actor, id domain.RemoteID) (remotes.Remote, error)
	List(ctx context.Context, actor domain.Actor, f remotes.ListFilter) ([]remotes.Remote, error)
	Update(ctx context.Context, actor domain.Actor, id domain.RemoteID, version int64, in remotes.Input) (remotes.Remote, error)
	Delete(ctx context.Context, actor domain.Actor, id domain.RemoteID) error
	// UnionsUsing returns the actor's live unions that use remote id
	// (Remote.used_by_unions).
	UnionsUsing(ctx context.Context, actor domain.Actor, id domain.RemoteID) ([]remotes.Remote, error)
	// Names returns display references of any remotes (jobs' remotes).
	Names(ctx context.Context, ids []domain.RemoteID) (map[domain.RemoteID]remotes.Ref, error)
}

var _ RemoteService = (*remotes.Service)(nil)
