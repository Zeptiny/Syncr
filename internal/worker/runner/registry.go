package runner

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/link"
)

// ErrUnknownKind is returned (wrapped) when no runner handles a job kind.
var ErrUnknownKind = errors.New("unknown job kind")

// Registry maps job kinds to runners. Runners are registered explicitly by
// the composition root (cmd/syncr-worker); there is no global registry and no
// init-time registration. It is safe for concurrent use.
type Registry struct {
	mu      sync.RWMutex
	runners map[string]Runner
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{runners: make(map[string]Runner)}
}

// Register adds runners. It fails, registering none of them, if a runner is
// nil, its kind is not a JobSpec operation, or its kind is already
// registered.
func (r *Registry) Register(runners ...Runner) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	batch := make(map[string]Runner, len(runners))
	for _, rn := range runners {
		if rn == nil {
			return errors.New("runner: register nil runner")
		}
		kind := rn.Kind()
		if !link.IsKind(kind) {
			return fmt.Errorf("runner: register %T: %w %q (not a JobSpec operation)", rn, ErrUnknownKind, kind)
		}
		if _, dup := r.runners[kind]; dup {
			return fmt.Errorf("runner: register %T: kind %q already registered", rn, kind)
		}
		if _, dup := batch[kind]; dup {
			return fmt.Errorf("runner: register %T: kind %q registered twice", rn, kind)
		}
		batch[kind] = rn
	}
	maps.Copy(r.runners, batch)
	return nil
}

// Lookup returns the runner for kind.
func (r *Registry) Lookup(kind string) (Runner, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rn, ok := r.runners[kind]
	return rn, ok
}

// ForSpec returns the runner for spec's kind. Errors are *Error values with
// code INVALID_SPEC (no operation set) or UNSUPPORTED_KIND (no runner,
// matching ErrUnknownKind), ready to be reported as the job's failure.
func (r *Registry) ForSpec(spec *syncrv1.JobSpec) (Runner, error) {
	kind := link.KindOf(spec)
	if kind == "" {
		return nil, Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "job spec has no operation")
	}
	rn, ok := r.Lookup(kind)
	if !ok {
		return nil, Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSUPPORTED_KIND, "%w %q on this worker", ErrUnknownKind, kind)
	}
	return rn, nil
}

// Kinds returns the registered kinds, sorted. Workers advertise them in their
// Hello event.
func (r *Registry) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Sorted(maps.Keys(r.runners))
}
