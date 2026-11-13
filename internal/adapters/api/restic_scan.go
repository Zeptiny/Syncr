package api

import (
	"context"
	"slices"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/app/restic"
)

// Milestone 7: measuring a snapshot folder, searching a repository and
// comparing two snapshots. Owner-only like BrowseSnapshot; the restic use
// cases scope them and check their input. Each waits for one heavy probe
// (probeProcedure), and none accepts GET: they have no NO_SIDE_EFFECTS
// option, so a cross-site link cannot start a scan.

// MeasureSnapshotFolder measures a folder of a snapshot of one of the
// caller's repositories.
func (h *resticHandler) MeasureSnapshotFolder(ctx context.Context, req *connect.Request[syncrv1.MeasureSnapshotFolderRequest]) (*connect.Response[syncrv1.MeasureSnapshotFolderResponse], error) {
	id, err := parseRepositoryID("repository_id", req.Msg.GetRepositoryId())
	if err != nil {
		return nil, err
	}
	workerID, err := optionalWorkerID(req.Msg.GetWorkerId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	m, err := h.restic.Measure(ctx, actor, id, req.Msg.GetSnapshotId(), req.Msg.GetPath(), workerID)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.ResticServiceMeasureSnapshotFolderProcedure, err)
	}
	return connect.NewResponse(measureSnapshotFolderResponse(m)), nil
}

// FindInRepository searches the snapshots of one of the caller's
// repositories.
func (h *resticHandler) FindInRepository(ctx context.Context, req *connect.Request[syncrv1.FindInRepositoryRequest]) (*connect.Response[syncrv1.FindInRepositoryResponse], error) {
	id, err := parseRepositoryID("repository_id", req.Msg.GetRepositoryId())
	if err != nil {
		return nil, err
	}
	workerID, err := optionalWorkerID(req.Msg.GetWorkerId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	r, err := h.restic.Find(ctx, actor, id, findQueryFromProto(req.Msg), workerID)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.ResticServiceFindInRepositoryProcedure, err)
	}
	return connect.NewResponse(findResponse(r)), nil
}

// CompareSnapshots compares two snapshots of one of the caller's
// repositories.
func (h *resticHandler) CompareSnapshots(ctx context.Context, req *connect.Request[syncrv1.CompareSnapshotsRequest]) (*connect.Response[syncrv1.CompareSnapshotsResponse], error) {
	id, err := parseRepositoryID("repository_id", req.Msg.GetRepositoryId())
	if err != nil {
		return nil, err
	}
	workerID, err := optionalWorkerID(req.Msg.GetWorkerId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	c, err := h.restic.Compare(ctx, actor, id, req.Msg.GetFromSnapshotId(), req.Msg.GetToSnapshotId(), req.Msg.GetPath(), workerID)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.ResticServiceCompareSnapshotsProcedure, err)
	}
	return connect.NewResponse(compareResponse(c)), nil
}

// findQueryFromProto is the search a request asks for; the restic use
// cases check it.
func findQueryFromProto(req *syncrv1.FindInRepositoryRequest) restic.FindQuery {
	return restic.FindQuery{
		Patterns:    slices.Clone(req.GetPatterns()),
		IgnoreCase:  req.GetIgnoreCase(),
		SnapshotIDs: slices.Clone(req.GetSnapshotIds()),
	}
}

// measurementToProto converts a measured folder.
func measurementToProto(m restic.Measurement) *syncrv1.ProbeMeasurement {
	out := &syncrv1.ProbeMeasurement{
		Path: m.Path, Bytes: m.Bytes, Files: m.Files, Dirs: m.Dirs,
		Children:          make([]*syncrv1.ProbeChildMeasurement, 0, len(m.Children)),
		ChildrenTruncated: m.ChildrenTruncated, Complete: m.Complete, Unreadable: m.Unreadable,
	}
	for _, c := range m.Children {
		out.Children = append(out.Children, &syncrv1.ProbeChildMeasurement{Name: c.Name, Bytes: c.Bytes, Files: c.Files, Dirs: c.Dirs})
	}
	return out
}

// measureSnapshotFolderResponse is the response of MeasureSnapshotFolder.
func measureSnapshotFolderResponse(m restic.Measurement) *syncrv1.MeasureSnapshotFolderResponse {
	return &syncrv1.MeasureSnapshotFolderResponse{
		Measurement: measurementToProto(m),
		Worker:      probeWorkerRef(m.WorkerID, m.WorkerName),
		Elapsed:     durationpb.New(m.Elapsed),
	}
}

// findResponse is the response of FindInRepository.
func findResponse(r restic.FindResult) *syncrv1.FindInRepositoryResponse {
	out := &syncrv1.FindInRepositoryResponse{
		Matches:   make([]*syncrv1.ResticFindMatch, 0, len(r.Matches)),
		Truncated: r.Truncated, Complete: r.Complete,
		SnapshotsSearched: int32(min(max(r.SnapshotsSearched, 0), 1<<31-1)),
		Worker:            probeWorkerRef(r.WorkerID, r.WorkerName),
	}
	for _, m := range r.Matches {
		out.Matches = append(out.Matches, &syncrv1.ResticFindMatch{SnapshotId: m.SnapshotID, Entry: entryToProto(m.Entry)})
	}
	return out
}

// compareResponse is the response of CompareSnapshots.
func compareResponse(c restic.Comparison) *syncrv1.CompareSnapshotsResponse {
	out := &syncrv1.CompareSnapshotsResponse{
		Changes:   make([]*syncrv1.ResticDiffChange, 0, len(c.Changes)),
		Truncated: c.Truncated, Complete: c.Complete,
		Worker: probeWorkerRef(c.WorkerID, c.WorkerName),
	}
	for _, ch := range c.Changes {
		out.Changes = append(out.Changes, &syncrv1.ResticDiffChange{Path: ch.Path, Modifier: diffModifierToProto(ch.Modifier)})
	}
	if s := c.Stats; s != nil {
		out.Stats = &syncrv1.ResticDiffStats{
			ChangedFiles: s.ChangedFiles, AddedFiles: s.AddedFiles, AddedDirs: s.AddedDirs, AddedBytes: s.AddedBytes,
			RemovedFiles: s.RemovedFiles, RemovedDirs: s.RemovedDirs, RemovedBytes: s.RemovedBytes,
		}
	}
	return out
}

// diffModifierToProto converts a change's kind; an unknown one is OTHER.
func diffModifierToProto(m restic.DiffModifier) syncrv1.ResticDiffModifier {
	switch m {
	case restic.DiffAdded:
		return syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_ADDED
	case restic.DiffRemoved:
		return syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_REMOVED
	case restic.DiffChanged:
		return syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_CHANGED
	case restic.DiffMetadata:
		return syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_METADATA
	case restic.DiffType:
		return syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_TYPE
	}
	return syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_OTHER
}
