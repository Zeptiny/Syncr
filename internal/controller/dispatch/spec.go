package dispatch

import (
	"context"
	"errors"
	"fmt"
	"slices"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
)

// ErrWorkerOutdated reports a worker that lacks a link feature the job's
// kind requires (jobkind Registry.RequiredFeatures): the job fails with
// "worker_outdated". It matches domain.ErrFailedPrecondition.
var ErrWorkerOutdated = fmt.Errorf("the worker must be upgraded: %w", domain.ErrFailedPrecondition)

// errRepositoryUnavailable reports a job whose restic repository was
// deleted or is not its owner's any more: the job fails with
// "repository_unavailable". It matches domain.ErrNotFound.
var errRepositoryUnavailable = fmt.Errorf("repository unavailable: %w", domain.ErrNotFound)

// Specs is the SpecBuilder of a controller: it builds a queued job's spec
// from its stored definition with the job kinds, the owner's resolved
// remotes and, for restic jobs, the owner's resolved repository. It holds no state and is safe for concurrent use.
//
// Building a spec is pure apart from Resolve: the definition is decoded
// and checked again (jobkind.Unmarshal, Registry.Check), the remotes it
// refers to are resolved for the job's owner with their credentials
// (Remotes.Resolve, which also returns the members of unions), and the
// kind builds the spec (Registry.Build). The spec, which contains secrets,
// is returned to the dispatcher only; nothing here logs or stores it, and
// no error message contains option or secret values.
type Specs struct {
	remotes Remotes
	repos   Repos
	kinds   *jobkind.Registry
}

var _ SpecBuilder = (*Specs)(nil)

// NewSpecs returns a Specs over the remotes service, the restic
// repositories (restic.Service) and the job kinds. remotes may be nil: jobs
// that use a remote then fail with "remote_unavailable" (jobs on the
// worker's filesystem only still run). repos may be nil: jobs on a restic
// repository then fail with "repository_unavailable".
func NewSpecs(remotes Remotes, repos Repos, kinds *jobkind.Registry) (*Specs, error) {
	if kinds == nil {
		return nil, errors.New("dispatch: specs need the job kinds")
	}
	return &Specs{remotes: remotes, repos: repos, kinds: kinds}, nil
}

// Build implements SpecBuilder for job j on a worker that reported w.
// Errors: domain.ErrInvalidArgument when the stored definition cannot be
// decoded, checked or built (also when its kind is not the job's);
// ErrWorkerOutdated when w lacks a link feature the kind requires;
// errRepositoryUnavailable when its repository is gone; domain.ErrNotFound
// (from Resolve or the kind) when a remote is missing, deleted or not the
// owner's; whatever else the Resolves return (domain.ErrFailedPrecondition
// for credentials or a password that cannot be decrypted, store errors to
// retry).
func (s *Specs) Build(ctx context.Context, j jobs.Job, w workers.Info) (*syncrv1.JobSpec, error) {
	def, err := jobkind.Unmarshal(j.Definition)
	if err != nil {
		return nil, fmt.Errorf("job %s: %w", j.ID, err)
	}
	kind, refs, err := s.kinds.Check(def)
	if err != nil {
		return nil, fmt.Errorf("job %s: check the definition: %w", j.ID, err)
	}
	if kind != j.Kind {
		return nil, fmt.Errorf("job %s: %w: the definition is a %q, the job a %q", j.ID, domain.ErrInvalidArgument, kind, j.Kind)
	}
	for _, f := range s.kinds.RequiredFeatures(kind) {
		if !w.HasFeatures(f) {
			return nil, fmt.Errorf("job %s: %w: it lacks the feature %q that %s jobs need", j.ID, ErrWorkerOutdated, f, kind)
		}
	}
	in := jobkind.Resolved{Remotes: make(map[domain.RemoteID]jobkind.Remote)}
	remoteIDs := refs.RemoteIDs
	if id := refs.RepositoryID; !id.IsZero() {
		if s.repos == nil {
			return nil, fmt.Errorf("job %s: repository %s: %w: no repository service is configured", j.ID, id, errRepositoryUnavailable)
		}
		repo, err := s.repos.Resolve(ctx, j.OwnerID, id)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			// errRepositoryUnavailable first: it also matches ErrNotFound.
			return nil, fmt.Errorf("job %s: repository %s: %w", j.ID, id, errRepositoryUnavailable)
		case err != nil:
			return nil, fmt.Errorf("job %s: resolve its repository: %w", j.ID, err)
		}
		// The requested ID is authoritative.
		in.Repositories = map[domain.ResticRepoID]jobkind.Repository{id: {
			ID: id, RemoteID: repo.RemoteID, Path: repo.Path, Password: repo.Password, ResticID: repo.ResticID,
		}}
		if !slices.Contains(remoteIDs, repo.RemoteID) {
			remoteIDs = append(slices.Clone(remoteIDs), repo.RemoteID)
		}
	}
	if len(remoteIDs) > 0 {
		if s.remotes == nil {
			return nil, fmt.Errorf("job %s: %w: no remotes service is configured", j.ID, domain.ErrNotFound)
		}
		rs, err := s.remotes.Resolve(ctx, j.OwnerID, remoteIDs)
		if err != nil {
			return nil, fmt.Errorf("job %s: resolve its remotes: %w", j.ID, err)
		}
		for id, r := range rs {
			members := make([]jobkind.Member, len(r.Members))
			for i, m := range r.Members {
				members[i] = jobkind.Member{RemoteID: m.RemoteID, Path: m.Path, Mode: m.Mode}
			}
			// The map key is authoritative for the ID.
			in.Remotes[id] = jobkind.Remote{ID: id, Rclone: r.Rclone, Members: members}
		}
	}
	spec, err := s.kinds.Build(def, in)
	if err != nil {
		return nil, fmt.Errorf("job %s: %w", j.ID, err)
	}
	return spec, nil
}
