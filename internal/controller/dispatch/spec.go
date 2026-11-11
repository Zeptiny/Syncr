package dispatch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/link"
)

// ErrWorkerOutdated reports a worker that lacks a link feature the job's
// kind requires (jobkind Registry.RequiredFeatures): the job fails with
// "worker_outdated". It matches domain.ErrFailedPrecondition.
var ErrWorkerOutdated = fmt.Errorf("the worker must be upgraded: %w", domain.ErrFailedPrecondition)

// ErrTimeLimitUnsupported reports a job with a time limit for a worker that
// does not report link.FeatureJobTimeLimit, so it would run the job without
// stopping it: the job fails with "worker_outdated" instead. It matches
// ErrWorkerOutdated (and so domain.ErrFailedPrecondition).
var ErrTimeLimitUnsupported = fmt.Errorf("it cannot enforce a time limit: %w", ErrWorkerOutdated)

// WrongOSError reports a worker that runs another operating system than
// the job's definition needs (jobkind OSRequirement, e.g. Windows VSS): the
// job fails with "invalid_spec", the code the worker itself would fail it
// with. It matches domain.ErrInvalidArgument.
type WrongOSError struct {
	// Required is what the definition needs.
	Required jobkind.OSRequirement
	// OS is what the worker reported ("" if nothing).
	OS string
}

func (e *WrongOSError) Error() string {
	return fmt.Sprintf("%s; the worker runs %q", e.Required.Explain(), e.OS)
}

// Unwrap makes the error match domain.ErrInvalidArgument.
func (e *WrongOSError) Unwrap() error { return domain.ErrInvalidArgument }

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
	cfg     SpecsConfig
}

var _ SpecBuilder = (*Specs)(nil)

// SpecsConfig tunes Specs. The zero value sets no default time limit.
type SpecsConfig struct {
	// DefaultMaxRuntime is the time limit of the jobs whose definition sets
	// no max_runtime (jobs.default_max_runtime): zero for none, otherwise
	// between jobs.MinTimeLimit and jobs.MaxTimeLimit. Jobs of kinds that
	// only the system starts (jobkind SystemOnly) never get it.
	DefaultMaxRuntime time.Duration
}

// NewSpecs returns a Specs over the remotes service, the restic
// repositories (restic.Service) and the job kinds, with at most one
// SpecsConfig (none: the zero SpecsConfig). remotes may be nil: jobs that
// use a remote then fail with "remote_unavailable" (jobs on the worker's
// filesystem only still run). repos may be nil: jobs on a restic
// repository then fail with "repository_unavailable".
func NewSpecs(remotes Remotes, repos Repos, kinds *jobkind.Registry, cfg ...SpecsConfig) (*Specs, error) {
	if kinds == nil {
		return nil, errors.New("dispatch: specs need the job kinds")
	}
	var c SpecsConfig
	switch len(cfg) {
	case 0:
	case 1:
		c = cfg[0]
	default:
		return nil, errors.New("dispatch: specs take at most one configuration")
	}
	if d := c.DefaultMaxRuntime; d != 0 && (d < jobs.MinTimeLimit || d > jobs.MaxTimeLimit) {
		return nil, fmt.Errorf("dispatch: invalid default time limit %v: it must be 0 (none) or between %v and %v",
			d, jobs.MinTimeLimit, jobs.MaxTimeLimit)
	}
	return &Specs{remotes: remotes, repos: repos, kinds: kinds, cfg: c}, nil
}

// TimeLimit returns the effective time limit of a job of definition def
// (zero: none), by jobs.EffectiveTimeLimit: def's max_runtime when it is a
// valid positive duration, else SpecsConfig.DefaultMaxRuntime unless def's
// kind is one only the system starts (an unregistered kind gets the
// default). Build sends exactly this limit, so the API's Job.time_limit
// should come from here too (TimeLimitOf for a stored job).
func (s *Specs) TimeLimit(def *syncrv1.JobDefinition) time.Duration {
	var own time.Duration
	if mr := def.GetMaxRuntime(); mr != nil && mr.CheckValid() == nil {
		own = mr.AsDuration()
	}
	kind := jobkind.KindOf(def)
	_, registered := s.kinds.Get(kind)
	startable, _ := s.kinds.UserStartable(kind)
	return jobs.EffectiveTimeLimit(own, s.cfg.DefaultMaxRuntime, registered && !startable)
}

// TimeLimitOf is TimeLimit for a stored job: it decodes j.Definition. A
// definition that cannot be decoded has no limit (zero), as such a job
// never runs.
func (s *Specs) TimeLimitOf(j jobs.Job) time.Duration {
	def, err := jobkind.Unmarshal(j.Definition)
	if err != nil {
		return 0
	}
	return s.TimeLimit(def)
}

// Build implements SpecBuilder for job j on a worker that reported w.
// The spec's max_runtime is the job's TimeLimit (unset when it has none).
// Errors: domain.ErrInvalidArgument when the stored definition cannot be
// decoded, checked or built (also when its kind is not the job's); a
// *WrongOSError when w reported another operating system than the
// definition needs (jobkind Registry.RequiredOS); ErrWorkerOutdated when w
// lacks a link feature the kind requires, and ErrTimeLimitUnsupported (which
// matches it) when the job has a time limit and w does not report
// link.FeatureJobTimeLimit;
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
	// The operating system first: upgrading the worker cannot change it.
	if req := s.kinds.RequiredOS(def); !req.Allows(w.OS) {
		return nil, fmt.Errorf("job %s: %w", j.ID, &WrongOSError{Required: req, OS: w.OS})
	}
	for _, f := range s.kinds.RequiredFeatures(kind) {
		if !w.HasFeatures(f) {
			return nil, fmt.Errorf("job %s: %w: it lacks the feature %q that %s jobs need", j.ID, ErrWorkerOutdated, f, kind)
		}
	}
	// A worker that ignores max_runtime would run the job without a limit.
	limit := s.TimeLimit(def)
	if limit > 0 && !w.HasFeatures(link.FeatureJobTimeLimit) {
		return nil, fmt.Errorf("job %s: %w: it lacks the feature %q (the job's time limit is %v)",
			j.ID, ErrTimeLimitUnsupported, link.FeatureJobTimeLimit, limit)
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
	spec.MaxRuntime = nil
	if limit > 0 {
		spec.MaxRuntime = durationpb.New(limit)
	}
	return spec, nil
}
