package probe

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// ErrNoWorker reports that no worker can run a probe or a job right now.
// It matches domain.ErrFailedPrecondition.
var ErrNoWorker = fmt.Errorf("no usable worker: %w", domain.ErrFailedPrecondition)

// Candidate is a worker as the pick sees it.
type Candidate struct {
	ID     domain.WorkerID
	Name   string
	Status domain.WorkerStatus
	// Version is the worker's reported version, for messages.
	Version string
	// Features, ProbeOps and JobKinds are what its latest Hello listed.
	Features []string
	ProbeOps []string
	JobKinds []string
	// Unfinished is its number of queued, dispatched and running jobs.
	Unfinished int
	// Preferred marks the worker to take if it is eligible (the one that
	// last worked on the repository: its restic cache is warm).
	Preferred bool
}

// canProbe reports whether c's worker implements probe operation op.
func (c Candidate) canProbe(op string) bool {
	return slices.Contains(c.Features, link.FeatureProbe) && slices.Contains(c.ProbeOps, op)
}

// canRun reports whether c's worker runs jobs of kind, with every feature
// of required.
func (c Candidate) canRun(kind string, required []string) bool {
	if !slices.Contains(c.JobKinds, kind) {
		return false
	}
	return !slices.ContainsFunc(required, func(f string) bool { return !slices.Contains(c.Features, f) })
}

// Pick returns the worker for probe operation op. Eligible workers are
// online, report link.FeatureProbe and list op in their probe operations.
// Among them: a Preferred one first, then the fewest Unfinished jobs, then
// the name (case-insensitive), then the ID. With no eligible worker it
// returns an error wrapping ErrNoWorker that says why.
func Pick(cands []Candidate, op string) (Candidate, error) {
	return pick(cands, func(c Candidate) bool { return c.canProbe(op) }, "can run this request")
}

// PickForJob is Pick for a job kind instead of a probe operation: eligible
// workers are online, list kind in their job kinds and report every
// feature of required. The order is Pick's.
func PickForJob(cands []Candidate, kind string, required []string) (Candidate, error) {
	return pick(cands, func(c Candidate) bool { return c.canRun(kind, required) }, fmt.Sprintf("can run %s jobs", kind))
}

func pick(cands []Candidate, able func(Candidate) bool, what string) (Candidate, error) {
	var eligible []Candidate
	online, capable := 0, 0
	for _, c := range cands {
		isOnline, isAble := c.Status == domain.WorkerOnline, able(c)
		if isOnline {
			online++
		}
		if isAble {
			capable++
		}
		if isOnline && isAble {
			eligible = append(eligible, c)
		}
	}
	if len(eligible) == 0 {
		switch {
		case len(cands) == 0:
			return Candidate{}, fmt.Errorf("%w: there are no workers", ErrNoWorker)
		case online == 0:
			return Candidate{}, fmt.Errorf("%w: no worker is online", ErrNoWorker)
		case capable == 0:
			return Candidate{}, fmt.Errorf("%w: no worker %s; upgrade a worker", ErrNoWorker, what)
		default:
			return Candidate{}, fmt.Errorf("%w: the workers that %s are offline, and the online ones must be upgraded", ErrNoWorker, what)
		}
	}
	return slices.MinFunc(eligible, func(a, b Candidate) int {
		return cmp.Or(
			cmp.Compare(preferredRank(a), preferredRank(b)),
			cmp.Compare(a.Unfinished, b.Unfinished),
			cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)),
			cmp.Compare(a.ID.String(), b.ID.String()),
		)
	}), nil
}

func preferredRank(c Candidate) int {
	if c.Preferred {
		return 0
	}
	return 1
}

// CheckExplicit validates the worker a caller chose for probe operation
// op: a missing (or removed) one is domain.ErrNotFound, an offline or
// never connected one domain.ErrUnavailable, one that lacks the probe
// feature or the operation domain.ErrFailedPrecondition (it is outdated).
func CheckExplicit(c Candidate, found bool, op string) error {
	return checkExplicit(c, found, c.canProbe(op), "this request")
}

// CheckExplicitForJob is CheckExplicit for a job kind and the link
// features it requires.
func CheckExplicitForJob(c Candidate, found bool, kind string, required []string) error {
	return checkExplicit(c, found, c.canRun(kind, required), kind+" jobs")
}

func checkExplicit(c Candidate, found, able bool, what string) error {
	switch {
	case !found:
		return fmt.Errorf("worker %w", domain.ErrNotFound)
	case c.Status == domain.WorkerPending:
		return fmt.Errorf("%w: worker %q has never connected", domain.ErrUnavailable, c.Name)
	case c.Status != domain.WorkerOnline:
		return fmt.Errorf("%w: worker %q is offline", domain.ErrUnavailable, c.Name)
	case !able:
		return fmt.Errorf("%w: worker %q (version %s) cannot run %s; upgrade it",
			domain.ErrFailedPrecondition, c.Name, cmp.Or(c.Version, "unknown"), what)
	}
	return nil
}
