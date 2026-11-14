package api

import (
	"cmp"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
)

// Time limits of the calls that wait for a worker's probe.
const (
	// defaultProbeTimeout is Deps.ProbeTimeout when it is not set: the
	// default of the controller's longest probe timeout.
	defaultProbeTimeout = 60 * time.Second
	// probeCallMargin is what such a call gets on top of the probe's
	// limit: picking the worker, resolving the remote, storing the result.
	probeCallMargin = 15 * time.Second
	// probeDeadlineGrace is added to the probe's limit for the
	// connection's read and write deadlines, so the error of a call that
	// timed out still reaches the client.
	probeDeadlineGrace = 30 * time.Second
)

// probeCallTimeout is the time limit of a call that waits for a probe
// bounded by probe (zero: the default).
func probeCallTimeout(probe time.Duration) time.Duration {
	return cmp.Or(max(probe, 0), defaultProbeTimeout) + probeCallMargin
}

// probeProcedure reports whether a procedure waits for a worker's probe:
// every ProbeService call, and the ResticService calls that initialize or
// verify a repository, list its snapshots, or browse, measure, search or
// compare them.
func probeProcedure(procedure string) bool {
	switch procedure {
	case syncrv1connect.ProbeServiceListWorkerFilesProcedure,
		syncrv1connect.ProbeServiceListRemoteFilesProcedure,
		syncrv1connect.ProbeServiceTestRemoteProcedure,
		syncrv1connect.ProbeServiceGetSftpHostKeysProcedure,
		syncrv1connect.ProbeServiceMeasureWorkerFolderProcedure,
		syncrv1connect.ProbeServiceMeasureRemoteFolderProcedure,
		syncrv1connect.ProbeServiceGetRemoteUsageProcedure,
		syncrv1connect.ResticServiceCreateRepositoryProcedure,
		syncrv1connect.ResticServiceUpdateRepositoryProcedure,
		syncrv1connect.ResticServiceListSnapshotsProcedure,
		syncrv1connect.ResticServiceBrowseSnapshotProcedure,
		syncrv1connect.ResticServiceMeasureSnapshotFolderProcedure,
		syncrv1connect.ResticServiceFindInRepositoryProcedure,
		syncrv1connect.ResticServiceCompareSnapshotsProcedure:
		return true
	}
	return false
}

// extendProbeDeadlines lifts the connection's read and write deadlines of
// the calls that wait for a probe to the probe's limit plus a grace
// period, as extendWatchDeadlines does for streams: the server-wide
// http.Server ReadTimeout and WriteTimeout are meant for ordinary calls
// and would cut these short.
func extendProbeDeadlines(next http.Handler, probe time.Duration, logger *slog.Logger) http.Handler {
	limit := probeCallTimeout(probe) - probeCallMargin + probeDeadlineGrace
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if probeProcedure(r.URL.Path) {
			rc := http.NewResponseController(w)
			deadline := time.Now().Add(limit)
			if err := errors.Join(rc.SetReadDeadline(deadline), rc.SetWriteDeadline(deadline)); err != nil && !errors.Is(err, http.ErrNotSupported) {
				logger.WarnContext(r.Context(), "cannot extend the deadlines of a probe call", "error", err.Error())
			}
		}
		next.ServeHTTP(w, r)
	})
}

// repositoryNames, remoteNames and workerNames return the services of deps
// as the narrow lookups the handlers use, or nil when they are not wired
// (an interface holding a nil service would not be nil).
func repositoryNames(deps Deps) RepositoryNames {
	if deps.Restic == nil {
		return nil
	}
	return deps.Restic
}

func remoteSourceChecker(deps Deps) RemoteSourceChecker {
	if deps.Restic == nil {
		return nil
	}
	return deps.Restic
}

func remoteNames(deps Deps) RemoteNames {
	if deps.Remotes == nil {
		return nil
	}
	return deps.Remotes
}

func workerNames(deps Deps) WorkerNames {
	if deps.Workers == nil {
		return nil
	}
	return deps.Workers
}
