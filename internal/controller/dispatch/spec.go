package dispatch

import (
	"context"
	"errors"
	"fmt"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
)

// Specs is the SpecBuilder of a controller: it builds a queued job's spec
// from its stored definition with the job kinds and the owner's resolved
// remotes. It holds no state and is safe for concurrent use.
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
	kinds   *jobkind.Registry
}

var _ SpecBuilder = (*Specs)(nil)

// NewSpecs returns a Specs over the remotes service and the job kinds.
// remotes may be nil: jobs that use a remote then fail with
// "remote_unavailable" (jobs on the worker's filesystem only still run).
func NewSpecs(remotes Remotes, kinds *jobkind.Registry) (*Specs, error) {
	if kinds == nil {
		return nil, errors.New("dispatch: specs need the job kinds")
	}
	return &Specs{remotes: remotes, kinds: kinds}, nil
}

// Build implements SpecBuilder. Errors: domain.ErrInvalidArgument when the
// stored definition cannot be decoded, checked or built (also when its kind
// is not the job's); domain.ErrNotFound (from Resolve or the kind) when a
// remote is missing, deleted or not the owner's; whatever else Resolve
// returns (domain.ErrFailedPrecondition for credentials that cannot be
// decrypted, store errors to retry).
func (s *Specs) Build(ctx context.Context, j jobs.Job) (*syncrv1.JobSpec, error) {
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
	resolved := make(map[domain.RemoteID]jobkind.Remote)
	if len(refs.RemoteIDs) > 0 {
		if s.remotes == nil {
			return nil, fmt.Errorf("job %s: %w: no remotes service is configured", j.ID, domain.ErrNotFound)
		}
		rs, err := s.remotes.Resolve(ctx, j.OwnerID, refs.RemoteIDs)
		if err != nil {
			return nil, fmt.Errorf("job %s: resolve its remotes: %w", j.ID, err)
		}
		for id, r := range rs {
			members := make([]jobkind.Member, len(r.Members))
			for i, m := range r.Members {
				members[i] = jobkind.Member{RemoteID: m.RemoteID, Path: m.Path, Mode: m.Mode}
			}
			// The map key is authoritative for the ID.
			resolved[id] = jobkind.Remote{ID: id, Rclone: r.Rclone, Members: members}
		}
	}
	spec, err := s.kinds.Build(def, resolved)
	if err != nil {
		return nil, fmt.Errorf("job %s: %w", j.ID, err)
	}
	return spec, nil
}
