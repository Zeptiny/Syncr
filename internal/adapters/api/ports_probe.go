package api

import (
	"context"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// ProbeService browses workers and remotes and tests remotes through a
// worker (implemented in internal/controller/probe, which this package
// cannot import: the port therefore speaks the generated response types).
// Remotes are the actor's own (domain.ErrNotFound otherwise, admins
// included); any user may browse any worker's local roots. Worker-side
// failures arrive as domain errors: ErrUnavailable (offline, busy, no
// answer in time), ErrFailedPrecondition (an outdated worker, no usable
// worker, a tool failure), a *domain.ValidationError on "path" or
// "worker_id", and ErrRateLimited.
type ProbeService interface {
	// ListWorkerFiles lists a folder of a worker's filesystem; an empty path
	// lists its local roots.
	ListWorkerFiles(ctx context.Context, actor domain.Actor, workerID domain.WorkerID, path string, dirsOnly bool) (*syncrv1.ListWorkerFilesResponse, error)
	// ListRemoteFiles lists a folder of one of the actor's remotes, through
	// workerID or, if zero, a worker the controller chooses.
	ListRemoteFiles(ctx context.Context, actor domain.Actor, remoteID domain.RemoteID, path string, workerID domain.WorkerID, dirsOnly bool) (*syncrv1.ListRemoteFilesResponse, error)
	// TestRemote lists a remote and asks for its usage: the stored remote
	// id, the stored remote with the unsaved values in, or (id zero) the
	// unsaved new remote in. Worker-side failures are in the response.
	TestRemote(ctx context.Context, actor domain.Actor, id domain.RemoteID, in *remotes.Input, path string, workerID domain.WorkerID) (*syncrv1.TestRemoteResponse, error)
}
