package probe

import (
	"context"
	"errors"
	"fmt"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Milestone 7: the heavy probes (link.HeavyProbeOps). Measuring a
// worker's folder (measure_local), a remote's (measure_remote) or a
// snapshot's (restic_measure), searching snapshots (restic_find) and
// comparing two (restic_diff) follow every rule of "Every probe" in the
// package documentation and run with Config.ScanTimeout; the worker ends
// them at a soft deadline before it and answers with what it found
// (complete=false). A worker already running one answers BUSY, which maps
// to domain.ErrUnavailable like any busy worker. GetRemoteUsage (about) is
// an ordinary probe with Config.Timeout.
//
// Not implemented yet: every method below fails with errors.ErrUnsupported.

// errNotYet is the failure of the operations this build does not run yet.
func errNotYet(what string) error {
	return fmt.Errorf("%s: %w", what, errors.ErrUnsupported)
}

// MeasureWorkerFolder measures a folder of a worker's filesystem
// (measure_local): its totals and each child folder's. Any user may
// measure any worker's folders, as they may list them (within its
// local_roots, which the worker enforces). workerID and path are required
// (*domain.ValidationError on "worker_id" or "path").
func (s *Service) MeasureWorkerFolder(_ context.Context, _ domain.Actor, _ domain.WorkerID, _ string) (*syncrv1.MeasureWorkerFolderResponse, error) {
	return nil, errNotYet("measure the worker's folder")
}

// MeasureRemoteFolder measures a folder of one of the actor's remotes
// (path is relative to the remote's root) through workerID or, if zero, a
// picked worker: measure_remote, or measure_local on the joined path for a
// remote on a worker's local filesystem, which needs workerID
// (*domain.ValidationError on "worker_id"). The measured path is returned
// relative to the remote's root.
func (s *Service) MeasureRemoteFolder(_ context.Context, _ domain.Actor, _ domain.RemoteID, _ string, _ domain.WorkerID) (*syncrv1.MeasureRemoteFolderResponse, error) {
	return nil, errNotYet("measure the remote's folder")
}

// RemoteUsage asks one of the actor's remotes for its usage (about)
// through workerID or, if zero, a picked worker. A backend that cannot
// tell answers with ProbeUsage.unsupported, not an error.
func (s *Service) RemoteUsage(_ context.Context, _ domain.Actor, _ domain.RemoteID, _ domain.WorkerID) (*syncrv1.GetRemoteUsageResponse, error) {
	return nil, errNotYet("ask the remote for its usage")
}

// Measure implements restic.Prober: the folder path (a tree path) of
// snapshot snapshotID of t (restic_measure).
func (r *Runner) Measure(_ context.Context, _ domain.UserID, _ restic.Target, _, _ string) (restic.Measurement, error) {
	return restic.Measurement{}, errNotYet("measure the snapshot's folder")
}

// Find implements restic.Prober: a search of t's snapshots (restic_find).
func (r *Runner) Find(_ context.Context, _ domain.UserID, _ restic.Target, _ restic.FindQuery) (restic.FindResult, error) {
	return restic.FindResult{}, errNotYet("search the snapshots")
}

// Diff implements restic.Prober: a comparison of two snapshots of t, the
// whole tree or one folder (restic_diff).
func (r *Runner) Diff(_ context.Context, _ domain.UserID, _ restic.Target, _, _, _ string) (restic.Comparison, error) {
	return restic.Comparison{}, errNotYet("compare the snapshots")
}
