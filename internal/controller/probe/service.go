package probe

import (
	"context"
	"errors"
	"fmt"
	pathpkg "path"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
)

const (
	// testEntries is how many entries a remote test lists; testSample how
	// many of their names it returns.
	testEntries = 50
	testSample  = 10
)

// Service is the API-facing part of the probes (api.ProbeService):
// browsing a worker's folders, browsing a remote and testing a remote. It
// returns the generated response types, because the API cannot import this
// package's own. Create it with NewService; safe for concurrent use.
type Service struct {
	r *Runner
}

// NewService returns the Service over r.
func NewService(r *Runner) *Service { return &Service{r: r} }

// ListWorkerFiles lists a folder of a worker's filesystem; an empty path
// lists its local roots. Any user may browse any worker (within its
// local_roots, which the worker enforces). workerID is required
// (*domain.ValidationError on "worker_id").
func (s *Service) ListWorkerFiles(ctx context.Context, actor domain.Actor, workerID domain.WorkerID, path string, dirsOnly bool) (*syncrv1.ListWorkerFilesResponse, error) {
	const what = "list the worker's files"
	if workerID.IsZero() {
		return nil, fmt.Errorf("%s: %w", what, fieldError("worker_id", "is required"))
	}
	if err := s.r.allow(actor.UserID); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	w, err := s.r.worker(ctx, selection{owner: actor.UserID, op: opListLocal, workerID: workerID})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	res, err := s.r.exec(ctx, w, s.listLocal(path, dirsOnly, s.r.cfg.MaxEntries), s.r.cfg.Timeout, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	l := res.GetListing()
	return &syncrv1.ListWorkerFilesResponse{Path: l.GetPath(), Entries: l.GetEntries(), Truncated: l.GetTruncated(), Worker: workerRef(w)}, nil
}

// ListRemoteFiles lists a folder of one of the actor's remotes (path is
// relative to the remote's root), through workerID or, if zero, a picked
// worker. A remote on a worker's local filesystem needs workerID
// (*domain.ValidationError on "worker_id"). The returned paths are
// relative to the remote's root.
func (s *Service) ListRemoteFiles(ctx context.Context, actor domain.Actor, remoteID domain.RemoteID, path string, workerID domain.WorkerID, dirsOnly bool) (*syncrv1.ListRemoteFilesResponse, error) {
	const what = "list the remote's files"
	if remoteID.IsZero() {
		return nil, fmt.Errorf("%s: %w", what, fieldError("remote_id", "is required"))
	}
	if err := s.r.allow(actor.UserID); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	rs, err := s.r.remotes.Resolve(ctx, actor.UserID, []domain.RemoteID{remoteID})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	t, err := s.target(remoteID, path, rs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	spec, op := s.listing(t, dirsOnly, s.r.cfg.MaxEntries)
	w, err := s.r.worker(ctx, selection{owner: actor.UserID, op: op, workerID: workerID, explicit: cond(t.local(), localRemoteHint)})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	res, err := s.r.exec(ctx, w, spec, s.r.cfg.Timeout, t.secrets)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	l := res.GetListing()
	return &syncrv1.ListRemoteFilesResponse{Path: t.rel, Entries: relativeEntries(t.rel, l.GetEntries()), Truncated: l.GetTruncated(), Worker: workerRef(w)}, nil
}

// TestRemote lists a remote's root (or path) through a worker and asks for
// its usage (best effort): the actor's stored remote id, that remote with
// the unsaved values in merged over it, or (id zero) the unsaved new
// remote in.
//
// What the worker, or the lack of a usable worker, reports is returned in
// the response (Ok false, Error, ErrorCode), never as an error. Errors are
// only: a *domain.ValidationError (the input, the path, a missing
// "worker_id" for a remote on a worker's filesystem), domain.ErrNotFound
// (the remote, or the chosen worker), domain.ErrRateLimited and internal
// failures.
func (s *Service) TestRemote(ctx context.Context, actor domain.Actor, id domain.RemoteID, in *remotes.Input, path string, workerID domain.WorkerID) (*syncrv1.TestRemoteResponse, error) {
	const what = "test the remote"
	if id.IsZero() && in == nil {
		return nil, fmt.Errorf("%s: %w", what, fieldError("remote_id", "is required"))
	}
	if err := s.r.allow(actor.UserID); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	var (
		rs  map[domain.RemoteID]remotes.Resolved
		err error
	)
	if in != nil {
		rs, id, err = s.r.remotes.Draft(ctx, actor, id, *in)
	} else {
		rs, err = s.r.remotes.Resolve(ctx, actor.UserID, []domain.RemoteID{id})
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	t, err := s.target(id, path, rs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	spec, op := s.listing(t, false, testEntries)
	w, err := s.r.worker(ctx, selection{owner: actor.UserID, op: op, workerID: workerID, explicit: cond(t.local(), localRemoteHint)})
	if err != nil {
		if reportable(err) {
			return &syncrv1.TestRemoteResponse{Error: publicMessage(err), ErrorCode: "no_worker"}, nil
		}
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	out := &syncrv1.TestRemoteResponse{Worker: workerRef(w)}
	res, err := s.r.exec(ctx, w, spec, s.r.cfg.Timeout, t.secrets)
	if err != nil {
		var f *workerError
		switch {
		case errors.As(err, &f):
			out.Error, out.ErrorCode = f.probe.Message, f.probe.Code
			if out.Error == "" {
				out.Error = publicMessage(f.mapped)
			}
		case reportable(err):
			out.Error, out.ErrorCode = publicMessage(err), "unavailable"
		default:
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		return out, nil
	}
	l := res.GetListing()
	out.Ok, out.Latency, out.Truncated = true, res.GetElapsed(), l.GetTruncated()
	out.Entries = int32(len(l.GetEntries())) //nolint:gosec // G115: at most testEntries
	for i, e := range l.GetEntries() {
		if i == testSample {
			break
		}
		out.Sample = append(out.Sample, e.GetName())
	}
	if t.local() {
		return out, nil // a folder of the worker has no usage to ask for
	}
	// Best effort: a remote that cannot tell its usage still passed.
	about := &syncrv1.ProbeSpec{Remotes: t.defs, Op: &syncrv1.ProbeSpec_About{About: &syncrv1.ProbeAbout{Location: t.loc}}}
	if canProbe(w, opAbout) {
		if res, err := s.r.exec(ctx, w, about, s.r.cfg.Timeout, t.secrets); err == nil {
			out.Usage = res.GetUsage()
		}
	}
	return out, nil
}

// reportable reports whether err is a condition TestRemote puts in its
// response: no usable worker, or a worker that is unavailable or outdated.
func reportable(err error) bool {
	return errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrFailedPrecondition)
}

// publicMessage is err's message without the domain sentinel's prefix
// ("unavailable: worker ..." -> "worker ...").
func publicMessage(err error) string {
	msg := err.Error()
	for _, class := range []error{domain.ErrUnavailable, domain.ErrFailedPrecondition, ErrNoWorker} {
		msg = strings.TrimPrefix(msg, class.Error()+": ")
	}
	return msg
}

func canProbe(w workers.Worker, op string) bool {
	return candidateOf(w, domain.WorkerOnline).canProbe(op)
}

// remoteTarget is a folder of a resolved remote.
type remoteTarget struct {
	defs []*syncrv1.RemoteDefinition
	loc  *syncrv1.Location
	// rel is the cleaned path relative to the remote's root ("" is the
	// root).
	rel     string
	secrets []string
}

// local reports whether the folder is on a worker's own filesystem.
func (t remoteTarget) local() bool { return t.loc.GetRemote() == "" }

// target locates path on remote id.
func (s *Service) target(id domain.RemoteID, path string, rs map[domain.RemoteID]remotes.Resolved) (remoteTarget, error) {
	defs, loc, err := jobkind.Locate(id, path, kindRemotes(rs))
	if err != nil {
		return remoteTarget{}, pathError(err)
	}
	return remoteTarget{defs: defs, loc: loc, rel: strings.Trim(pathpkg.Clean("/"+path), "/"), secrets: secretsOf(rs)}, nil
}

// listing returns the spec that lists t and its operation: list_remote,
// or list_local for a remote that is a folder of the worker.
func (s *Service) listing(t remoteTarget, dirsOnly bool, maxEntries int) (*syncrv1.ProbeSpec, string) {
	if t.local() {
		return s.listLocal(t.loc.GetPath(), dirsOnly, maxEntries), opListLocal
	}
	return &syncrv1.ProbeSpec{Remotes: t.defs, Op: &syncrv1.ProbeSpec_ListRemote{ListRemote: &syncrv1.ProbeListRemote{
		Location: t.loc, DirsOnly: dirsOnly, MaxEntries: int32(maxEntries), //nolint:gosec // G115: bounded by the configuration
	}}}, opListRemote
}

func (s *Service) listLocal(path string, dirsOnly bool, maxEntries int) *syncrv1.ProbeSpec {
	return &syncrv1.ProbeSpec{Op: &syncrv1.ProbeSpec_ListLocal{ListLocal: &syncrv1.ProbeListLocal{
		Path: path, DirsOnly: dirsOnly, MaxEntries: int32(maxEntries), //nolint:gosec // G115: bounded by the configuration
	}}}
}

// relativeEntries returns the entries with paths relative to the remote's
// root: the worker lists what rclone sees (the remote's root joined with
// the path), which callers must not need to know.
func relativeEntries(rel string, entries []*syncrv1.ProbeEntry) []*syncrv1.ProbeEntry {
	out := make([]*syncrv1.ProbeEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, &syncrv1.ProbeEntry{
			Name: e.GetName(), Path: pathpkg.Join(rel, e.GetName()), Type: e.GetType(),
			Size: e.GetSize(), ModTime: e.GetModTime(), Mode: e.GetMode(),
		})
	}
	return out
}

func workerRef(w workers.Worker) *syncrv1.ProbeWorkerRef {
	return &syncrv1.ProbeWorkerRef{Id: w.ID.String(), Name: w.Name}
}

// timeOf converts a wire timestamp; nil or invalid gives the zero time.
func timeOf(ts *timestamppb.Timestamp) time.Time {
	if ts == nil || ts.CheckValid() != nil {
		return time.Time{}
	}
	return ts.AsTime()
}
