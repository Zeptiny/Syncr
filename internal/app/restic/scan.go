package restic

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Milestone 7: measuring a snapshot folder, searching a repository's
// snapshots and comparing two snapshots. Each runs one heavy probe on a
// worker (Prober.Measure, Find, Diff), which stops at the controller's
// scan timeout and then answers with what it found (Complete false).

// Measurement is a measured snapshot folder (Prober.Measure,
// Service.Measure).
type Measurement struct {
	// WorkerID and WorkerName name the worker that measured it.
	WorkerID   domain.WorkerID
	WorkerName string
	// Path is the measured tree path.
	Path string
	// Bytes is the size of every file below Path; Files and Dirs count the
	// files and the folders below it.
	Bytes, Files, Dirs int64
	// Children are the direct child folders' totals, by name.
	Children []ChildMeasurement
	// ChildrenTruncated: more child folders were found than returned.
	ChildrenTruncated bool
	// Complete: the walk covered everything. Otherwise it stopped at the
	// scan timeout and every number is a lower bound.
	Complete bool
	// Unreadable counts the entries the walk could not read and skipped;
	// with any, the totals are lower bounds even when Complete is set.
	Unreadable int64
	// Elapsed is how long the worker measured.
	Elapsed time.Duration
}

// ChildMeasurement is one direct child folder's totals.
type ChildMeasurement struct {
	// Name is the child folder's base name.
	Name string
	// Bytes, Files and Dirs are as Measurement's, below the child.
	Bytes, Files, Dirs int64
}

// FindQuery is a search of a repository's snapshots (Service.Find,
// Prober.Find).
type FindQuery struct {
	// Patterns are restic find's glob patterns: matched against the base
	// name, or against the whole path when they contain "/".
	Patterns []string
	// IgnoreCase matches case-insensitively.
	IgnoreCase bool
	// SnapshotIDs limits the search to these snapshots (8 to 64 hex
	// digits each); empty searches all of them.
	SnapshotIDs []string
}

// FindResult is what a search found.
type FindResult struct {
	// WorkerID and WorkerName name the worker that searched.
	WorkerID   domain.WorkerID
	WorkerName string
	// Matches are one per matching path and snapshot.
	Matches []Match
	// Truncated: the most matches a worker returns, or its result size
	// cap, was reached.
	Truncated bool
	// Complete: every requested snapshot was searched (false: the search
	// stopped at the scan timeout).
	Complete bool
	// SnapshotsSearched is how many snapshots were searched.
	SnapshotsSearched int
}

// Match is one matching path in one snapshot.
type Match struct {
	// SnapshotID is the snapshot's full ID.
	SnapshotID string
	// Entry is the match; Entry.Path is its snapshot tree path.
	Entry Entry
}

// DiffModifier is the kind of a change between two snapshots (restic
// diff's modifier). The string values are the API's names.
type DiffModifier string

// Diff modifiers.
const (
	// DiffAdded ("+"): only in the newer snapshot.
	DiffAdded DiffModifier = "added"
	// DiffRemoved ("-"): only in the older snapshot.
	DiffRemoved DiffModifier = "removed"
	// DiffChanged ("M"): the content changed.
	DiffChanged DiffModifier = "changed"
	// DiffMetadata ("U"): only metadata changed.
	DiffMetadata DiffModifier = "metadata"
	// DiffType ("T"): the type changed (e.g. a file became a folder).
	DiffType DiffModifier = "type"
	// DiffOther: anything restic reports that Syncr does not know.
	DiffOther DiffModifier = "other"
)

// Change is one changed path between two snapshots.
type Change struct {
	// Path is the snapshot tree path; a folder ends in "/", as restic
	// prints it.
	Path     string
	Modifier DiffModifier
}

// DiffStats are restic diff's closing statistics.
type DiffStats struct {
	ChangedFiles              int64
	AddedFiles, AddedDirs     int64
	AddedBytes                int64
	RemovedFiles, RemovedDirs int64
	RemovedBytes              int64
}

// Comparison is a comparison of two snapshots (Prober.Diff,
// Service.Compare).
type Comparison struct {
	// WorkerID and WorkerName name the worker that compared them.
	WorkerID   domain.WorkerID
	WorkerName string
	// Changes are the changed paths, in restic's order.
	Changes []Change
	// Stats are restic's totals; nil when the comparison stopped early.
	Stats *DiffStats
	// Truncated: the most changes a worker returns, or its result size
	// cap, was reached.
	Truncated bool
	// Complete: restic finished (Stats is set).
	Complete bool
}

// Measure measures a folder (a snapshot tree path; "" is the root "/") of a
// snapshot (8 to 64 hex digits) of one of actor's repositories, through the
// given worker or, if zero, one the prober picks: its totals and each
// child folder's. Invalid input is a *domain.ValidationError on "snapshot_id" or
// "path"; ErrRepositoryDeleting while the repository's data is being
// deleted.
//
// Not implemented yet: it fails with errors.ErrUnsupported.
func (s *Service) Measure(_ context.Context, _ domain.Actor, id domain.ResticRepoID, _, _ string, _ domain.WorkerID) (Measurement, error) {
	return Measurement{}, fmt.Errorf("measure snapshot folder of repository %s: %w", id, errors.ErrUnsupported)
}

// Find searches the snapshots of one of actor's repositories, through the
// given worker or, if zero, one the prober picks. Invalid input is a
// *domain.ValidationError on "patterns" (1 to 10, each 1 to 256 bytes of
// valid UTF-8 without NUL or line breaks) or "snapshot_ids";
// ErrRepositoryDeleting while the repository's data is being deleted.
//
// Not implemented yet: it fails with errors.ErrUnsupported.
func (s *Service) Find(_ context.Context, _ domain.Actor, id domain.ResticRepoID, _ FindQuery, _ domain.WorkerID) (FindResult, error) {
	return FindResult{}, fmt.Errorf("search repository %s: %w", id, errors.ErrUnsupported)
}

// Compare compares two snapshots of one of actor's repositories, the older
// (from) with the newer (to), the whole tree or one folder (a snapshot
// tree path; "" or "/" is the whole tree), through the given worker or, if
// zero, one the prober picks. Invalid input is a *domain.ValidationError on "from_snapshot_id",
// "to_snapshot_id" (also when both name the same snapshot) or "path";
// ErrRepositoryDeleting while the repository's data is being deleted.
//
// Not implemented yet: it fails with errors.ErrUnsupported.
func (s *Service) Compare(_ context.Context, _ domain.Actor, id domain.ResticRepoID, _, _, _ string, _ domain.WorkerID) (Comparison, error) {
	return Comparison{}, fmt.Errorf("compare snapshots of repository %s: %w", id, errors.ErrUnsupported)
}
