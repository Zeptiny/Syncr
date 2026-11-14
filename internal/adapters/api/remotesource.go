package api

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/link"
)

// remoteSourceField is the field path of a backup's remote source in a
// definition; CheckRemoteSource's fields are below it.
const remoteSourceField = "definition.restic_backup.remote_source"

// checkRemoteSource checks a backup of a remote (a checked definition with
// its Refs) against its repository with sc: nil for any other definition,
// or without sc (the dispatcher's spec builder refuses the same folders).
// The fields of a *domain.ValidationError are full definition paths, e.g.
// "definition.restic_backup.remote_source.path"; other errors are
// CheckRemoteSource's (see remoteSourceError).
func checkRemoteSource(ctx context.Context, sc RemoteSourceChecker, owner domain.UserID, def *syncrv1.JobDefinition, refs jobkind.Refs) error {
	remote, path, ok := jobkind.RemoteSourceOf(def)
	if !ok || sc == nil || refs.RepositoryID.IsZero() {
		return nil
	}
	err := sc.CheckRemoteSource(ctx, owner, refs.RepositoryID, remote, path)
	var verr *domain.ValidationError
	if errors.As(err, &verr) {
		var v domain.Validation
		v.AddAll(remoteSourceField, verr.Fields)
		return v.Err()
	}
	return err
}

// remoteSourceError maps an error of checkRemoteSource: a source remote or
// a repository that is not the caller's is NotFound ("remote not found",
// "repository not found", as RunJob says for the job's other references);
// anything else goes to fail.
func remoteSourceError(err error, fail func(error) error) error {
	switch {
	case errors.Is(err, restic.ErrUnknownSourceRemote):
		return connect.NewError(connect.CodeNotFound, errors.New("remote not found"))
	case errors.Is(err, restic.ErrUnknownRepository):
		return connect.NewError(connect.CodeNotFound, errors.New("repository not found"))
	}
	return fail(err)
}

// unusableSourceMethod returns, as a field error on field (the request's
// worker field), why the chosen worker is known not to run def, a backup
// of a remote: it is too old to back up remotes, or it cannot use the
// method (WorkerInfo.remote_sources), with the reason it reported. ok is
// false for any other definition, and when the worker cannot use the
// method as far as its latest Hello tells.
//
// Like wrongWorkerOS, this is an early answer from the worker's latest
// Hello: a worker that has not reported yet, or cannot be read, passes,
// and the dispatcher checks the worker's current process before it sends
// the job (tool_unavailable, worker_outdated).
func unusableSourceMethod(ctx context.Context, ws WorkerNames, id domain.WorkerID, def *syncrv1.JobDefinition, field string) (fe domain.FieldError, ok bool) {
	method, isSource := jobkind.RemoteSourceMethod(def)
	if !isSource || ws == nil || id.IsZero() {
		return domain.FieldError{}, false
	}
	w, err := ws.Get(ctx, id)
	if err != nil || workerInfoToProto(w.Info) == nil {
		return domain.FieldError{}, false
	}
	if !w.Info.HasFeatures(link.FeatureResticRemoteSource) {
		return domain.FieldError{Field: field, Message: fmt.Sprintf("worker %q must be upgraded to back up a folder of a remote", w.Name)}, true
	}
	rs := w.Info.RemoteSources
	support := &syncrv1.RemoteSourceSupport{Mirror: rs.Mirror, Mount: rs.Mount, MirrorReason: rs.MirrorReason, MountReason: rs.MountReason}
	if why := jobkind.RemoteSourceUnavailable(method, support); why != "" {
		return domain.FieldError{Field: field, Message: fmt.Sprintf("worker %q %s", w.Name, why)}, true
	}
	return domain.FieldError{}, false
}
