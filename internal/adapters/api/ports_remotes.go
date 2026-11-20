package api

import (
	"context"
	"errors"
	"fmt"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
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
	// RemotesUsing returns the actor's live remotes that use remote id:
	// the unions that have it as a member (Remote.used_by_unions) and the
	// wrappers that store their data in it (Remote.wrapped_by).
	RemotesUsing(ctx context.Context, actor domain.Actor, id domain.RemoteID) ([]remotes.Remote, error)
	// Names returns display references of any remotes (jobs' remotes).
	Names(ctx context.Context, ids []domain.RemoteID) (map[domain.RemoteID]remotes.Ref, error)
	RemoteInfos
}

// RemoteInfos tells the checks of a job definition what its remotes are
// (remotes.Service.Infos): RunJob and the schedule handlers pass them to
// jobkind's Registry.CheckWith. ErrNotFound if a remote is not a live
// remote of owner.
type RemoteInfos interface {
	Infos(ctx context.Context, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]remotes.Info, error)
}

var _ RemoteService = (*remotes.Service)(nil)

// checkWithRemotes runs kinds.CheckWith for the checked definition def
// with the infos of refs' remotes (those of owner) and returns refs with
// the concurrency keys it computed. With no infos port, or a remote that
// is not a live remote of owner (domain.ErrNotFound), refs come back as
// they are: the use case that stores the job or schedule reports such a
// remote as before ("remote not found"). Errors are CheckWith's
// *domain.ValidationError, or the port's other errors.
func checkWithRemotes(ctx context.Context, kinds *jobkind.Registry, infos RemoteInfos, owner domain.UserID, def *syncrv1.JobDefinition, refs jobkind.Refs) (jobkind.Refs, error) {
	if infos == nil {
		return refs, nil
	}
	got, err := infos.Infos(ctx, owner, refs.RemoteIDs)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return refs, nil
	case err != nil:
		return jobkind.Refs{}, fmt.Errorf("check the job's remotes: %w", err)
	}
	_, checked, err := kinds.CheckWith(def, kindInfos(got))
	if err != nil {
		return jobkind.Refs{}, err
	}
	return checked, nil
}

// kindInfos converts the remotes service's infos for jobkind.
func kindInfos(in map[domain.RemoteID]remotes.Info) map[domain.RemoteID]jobkind.RemoteInfo {
	out := make(map[domain.RemoteID]jobkind.RemoteInfo, len(in))
	for id, info := range in {
		out[id] = jobkind.RemoteInfo{
			Wraps:      jobkind.Wrap{RemoteID: info.Wrapped.RemoteID, Path: info.Wrapped.Path},
			NoChecksum: info.Capabilities.NoChecksum,
		}
	}
	return out
}
