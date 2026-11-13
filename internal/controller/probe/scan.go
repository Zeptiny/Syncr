package probe

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Milestone 7: the heavy probes (link.HeavyProbeOps). Measuring a
// worker's folder (measure_local), a remote's (measure_remote) or a
// snapshot's (restic_measure), searching snapshots (restic_find) and
// comparing two (restic_diff) follow every rule of "Every probe" in the
// package documentation and run with Config.ScanTimeout; the worker ends
// them at a soft deadline before it and answers with what it found
// (complete=false), which is a success here. A worker already running one
// answers BUSY, which maps to domain.ErrUnavailable ("already measuring or
// searching"). GetRemoteUsage (about) is an ordinary probe with
// Config.Timeout.

// The heavy probe operations.
const (
	opMeasureLocal  = "measure_local"
	opMeasureRemote = "measure_remote"
	opResticMeasure = "restic_measure"
	opResticFind    = "restic_find"
	opResticDiff    = "restic_diff"
)

// Bounds the controller asks the worker for (the worker caps them at
// 5000).
const (
	// FindMatches is the most matches a search returns.
	FindMatches = 1000
	// DiffChanges is the most changes a comparison returns.
	DiffChanges = 2000
)

// MeasureWorkerFolder measures a folder of a worker's filesystem
// (measure_local): its totals and each child folder's. Any user may
// measure any worker's folders, as they may list them (within its
// local_roots, which the worker enforces). workerID and path are required
// (*domain.ValidationError on "worker_id" or "path").
func (s *Service) MeasureWorkerFolder(ctx context.Context, actor domain.Actor, workerID domain.WorkerID, path string) (*syncrv1.MeasureWorkerFolderResponse, error) {
	const what = "measure the worker's folder"
	var v domain.Validation
	if workerID.IsZero() {
		v.Add("worker_id", "is required")
	}
	if path == "" {
		v.Add("path", "is required")
	}
	if err := v.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if err := s.r.allow(actor.UserID); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	w, err := s.r.worker(ctx, selection{owner: actor.UserID, op: opMeasureLocal, workerID: workerID})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	m, elapsed, err := s.r.measure(ctx, w, measureLocal(path), nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return &syncrv1.MeasureWorkerFolderResponse{Measurement: m, Worker: workerRef(w), Elapsed: elapsed}, nil
}

// MeasureRemoteFolder measures a folder of one of the actor's remotes
// (path is relative to the remote's root) through workerID or, if zero, a
// picked worker: measure_remote, or measure_local on the joined path for a
// remote on a worker's local filesystem, which needs workerID
// (*domain.ValidationError on "worker_id"). The measured path is returned
// relative to the remote's root.
func (s *Service) MeasureRemoteFolder(ctx context.Context, actor domain.Actor, remoteID domain.RemoteID, path string, workerID domain.WorkerID) (*syncrv1.MeasureRemoteFolderResponse, error) {
	const what = "measure the remote's folder"
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
	spec, op := measureLocal(t.loc.GetPath()), opMeasureLocal
	if !t.local() {
		spec = &syncrv1.ProbeSpec{Remotes: t.defs, Op: &syncrv1.ProbeSpec_MeasureRemote{MeasureRemote: &syncrv1.ProbeMeasureRemote{Location: t.loc}}}
		op = opMeasureRemote
	}
	w, err := s.r.worker(ctx, selection{owner: actor.UserID, op: op, workerID: workerID, explicit: cond(t.local(), localRemoteHint)})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	m, elapsed, err := s.r.measure(ctx, w, spec, t.secrets)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	m.Path = t.rel // the worker measured what rclone or the disk sees
	return &syncrv1.MeasureRemoteFolderResponse{Measurement: m, Worker: workerRef(w), Elapsed: elapsed}, nil
}

// RemoteUsage asks one of the actor's remotes for its usage (about)
// through workerID or, if zero, a picked worker. A backend that cannot
// tell answers with ProbeUsage.unsupported, not an error. A remote on a
// worker's local filesystem needs workerID, as listing it does
// (*domain.ValidationError on "worker_id"); its usage is unsupported (the
// worker reports none for its own folders), and nothing is sent to the
// worker.
func (s *Service) RemoteUsage(ctx context.Context, actor domain.Actor, remoteID domain.RemoteID, workerID domain.WorkerID) (*syncrv1.GetRemoteUsageResponse, error) {
	const what = "ask the remote for its usage"
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
	t, err := s.target(remoteID, "", rs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if t.local() {
		w, err := s.r.worker(ctx, selection{owner: actor.UserID, op: opListLocal, workerID: workerID, explicit: localRemoteHint})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		return &syncrv1.GetRemoteUsageResponse{Usage: &syncrv1.ProbeUsage{Unsupported: true}, Worker: workerRef(w)}, nil
	}
	w, err := s.r.worker(ctx, selection{owner: actor.UserID, op: opAbout, workerID: workerID})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	spec := &syncrv1.ProbeSpec{Remotes: t.defs, Op: &syncrv1.ProbeSpec_About{About: &syncrv1.ProbeAbout{Location: t.loc}}}
	res, err := s.r.exec(ctx, w, spec, s.r.cfg.Timeout, t.secrets)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if res.GetUsage() == nil {
		return nil, fmt.Errorf("%s: %w", what, noOutput(w, "usage"))
	}
	return &syncrv1.GetRemoteUsageResponse{Usage: res.GetUsage(), Worker: workerRef(w)}, nil
}

// measureLocal is the spec that measures path on the worker's filesystem.
func measureLocal(path string) *syncrv1.ProbeSpec {
	return &syncrv1.ProbeSpec{Op: &syncrv1.ProbeSpec_MeasureLocal{MeasureLocal: &syncrv1.ProbeMeasureLocal{Path: path}}}
}

// measure runs a measuring spec on w with the scan timeout and returns the
// measurement (complete or not) and how long the worker took.
func (r *Runner) measure(ctx context.Context, w workers.Worker, spec *syncrv1.ProbeSpec, secrets []string) (*syncrv1.ProbeMeasurement, *durationpb.Duration, error) {
	res, err := r.exec(ctx, w, spec, r.cfg.ScanTimeout, secrets)
	if err != nil {
		return nil, nil, err
	}
	if res.GetMeasurement() == nil {
		return nil, nil, noOutput(w, "measurement")
	}
	return res.GetMeasurement(), res.GetElapsed(), nil
}

// noOutput is the error of a worker that answered OK without the output
// its operation produces.
func noOutput(w workers.Worker, what string) error {
	return fmt.Errorf("%w: worker %q answered without a %s", domain.ErrUnavailable, w.Name, what)
}

// Measure implements restic.Prober: the folder path (a tree path) of
// snapshot snapshotID of t (restic_measure).
func (r *Runner) Measure(ctx context.Context, owner domain.UserID, t restic.Target, snapshotID, path string) (restic.Measurement, error) {
	const what = "measure the snapshot's folder"
	if err := r.allow(owner); err != nil {
		return restic.Measurement{}, fmt.Errorf("%s: %w", what, err)
	}
	rs, err := r.repository(ctx, owner, t)
	if err != nil {
		return restic.Measurement{}, fmt.Errorf("%s: %w", what, err)
	}
	spec := &syncrv1.ProbeSpec{Remotes: rs.defs, Op: &syncrv1.ProbeSpec_ResticMeasure{ResticMeasure: &syncrv1.ProbeResticMeasure{
		Repository: rs.repo, SnapshotId: snapshotID, Path: path,
	}}}
	w, res, err := r.onRepository(ctx, owner, t, rs, opResticMeasure, spec, r.cfg.ScanTimeout)
	if err != nil {
		return restic.Measurement{}, fmt.Errorf("%s: %w", what, err)
	}
	m := res.GetMeasurement()
	if m == nil {
		return restic.Measurement{}, fmt.Errorf("%s: %w", what, noOutput(w, "measurement"))
	}
	out := restic.Measurement{
		WorkerID: w.ID, WorkerName: w.Name, Path: m.GetPath(), Bytes: m.GetBytes(), Files: m.GetFiles(), Dirs: m.GetDirs(),
		ChildrenTruncated: m.GetChildrenTruncated(), Complete: m.GetComplete(), Unreadable: m.GetUnreadable(),
		Elapsed: res.GetElapsed().AsDuration(),
	}
	for _, c := range m.GetChildren() {
		out.Children = append(out.Children, restic.ChildMeasurement{Name: c.GetName(), Bytes: c.GetBytes(), Files: c.GetFiles(), Dirs: c.GetDirs()})
	}
	return out, nil
}

// Find implements restic.Prober: a search of t's snapshots (restic_find),
// at most FindMatches matches.
func (r *Runner) Find(ctx context.Context, owner domain.UserID, t restic.Target, q restic.FindQuery) (restic.FindResult, error) {
	const what = "search the snapshots"
	if err := r.allow(owner); err != nil {
		return restic.FindResult{}, fmt.Errorf("%s: %w", what, err)
	}
	rs, err := r.repository(ctx, owner, t)
	if err != nil {
		return restic.FindResult{}, fmt.Errorf("%s: %w", what, err)
	}
	spec := &syncrv1.ProbeSpec{Remotes: rs.defs, Op: &syncrv1.ProbeSpec_ResticFind{ResticFind: &syncrv1.ProbeResticFind{
		Repository: rs.repo, Patterns: q.Patterns, IgnoreCase: q.IgnoreCase, SnapshotIds: q.SnapshotIDs, MaxMatches: FindMatches,
	}}}
	w, res, err := r.onRepository(ctx, owner, t, rs, opResticFind, spec, r.cfg.ScanTimeout)
	if err != nil {
		return restic.FindResult{}, fmt.Errorf("%s: %w", what, err)
	}
	f := res.GetFind()
	if f == nil {
		return restic.FindResult{}, fmt.Errorf("%s: %w", what, noOutput(w, "search result"))
	}
	out := restic.FindResult{
		WorkerID: w.ID, WorkerName: w.Name, Truncated: f.GetTruncated(), Complete: f.GetComplete(),
		SnapshotsSearched: int(f.GetSnapshotsSearched()),
	}
	for _, m := range f.GetMatches() {
		out.Matches = append(out.Matches, restic.Match{SnapshotID: m.GetSnapshotId(), Entry: entryFromProto(m.GetEntry())})
	}
	return out, nil
}

// Diff implements restic.Prober: a comparison of two snapshots of t, the
// whole tree or one folder (restic_diff), at most DiffChanges changes.
func (r *Runner) Diff(ctx context.Context, owner domain.UserID, t restic.Target, fromID, toID, path string) (restic.Comparison, error) {
	const what = "compare the snapshots"
	if err := r.allow(owner); err != nil {
		return restic.Comparison{}, fmt.Errorf("%s: %w", what, err)
	}
	rs, err := r.repository(ctx, owner, t)
	if err != nil {
		return restic.Comparison{}, fmt.Errorf("%s: %w", what, err)
	}
	spec := &syncrv1.ProbeSpec{Remotes: rs.defs, Op: &syncrv1.ProbeSpec_ResticDiff{ResticDiff: &syncrv1.ProbeResticDiff{
		Repository: rs.repo, FromSnapshotId: fromID, ToSnapshotId: toID, Path: path, MaxChanges: DiffChanges,
	}}}
	w, res, err := r.onRepository(ctx, owner, t, rs, opResticDiff, spec, r.cfg.ScanTimeout)
	if err != nil {
		return restic.Comparison{}, fmt.Errorf("%s: %w", what, err)
	}
	d := res.GetDiff()
	if d == nil {
		return restic.Comparison{}, fmt.Errorf("%s: %w", what, noOutput(w, "comparison"))
	}
	out := restic.Comparison{WorkerID: w.ID, WorkerName: w.Name, Truncated: d.GetTruncated(), Complete: d.GetComplete()}
	for _, c := range d.GetChanges() {
		out.Changes = append(out.Changes, restic.Change{Path: c.GetPath(), Modifier: diffModifierFromProto(c.GetModifier())})
	}
	if s := d.GetStats(); s != nil {
		out.Stats = &restic.DiffStats{
			ChangedFiles: s.GetChangedFiles(), AddedFiles: s.GetAddedFiles(), AddedDirs: s.GetAddedDirs(), AddedBytes: s.GetAddedBytes(),
			RemovedFiles: s.GetRemovedFiles(), RemovedDirs: s.GetRemovedDirs(), RemovedBytes: s.GetRemovedBytes(),
		}
	}
	return out, nil
}

// diffModifierFromProto converts a change's kind; an unknown one is
// restic.DiffOther.
func diffModifierFromProto(m syncrv1.ResticDiffModifier) restic.DiffModifier {
	switch m {
	case syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_ADDED:
		return restic.DiffAdded
	case syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_REMOVED:
		return restic.DiffRemoved
	case syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_CHANGED:
		return restic.DiffChanged
	case syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_METADATA:
		return restic.DiffMetadata
	case syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_TYPE:
		return restic.DiffType
	}
	return restic.DiffOther
}
