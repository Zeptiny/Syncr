package restic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/redact"
)

// Milestone 7: measuring a snapshot folder, searching a repository's
// snapshots and comparing two snapshots. Each runs one heavy probe on a
// worker (Prober.Measure, Find, Diff), which stops at the controller's
// scan timeout and then answers with what it found (Complete false). The
// input checks below match the worker's own, so a request the controller
// accepts is never refused by an up-to-date worker.

// Bounds of a search (Service.Find).
const (
	// MaxFindPatterns is the most patterns a search takes.
	MaxFindPatterns = 10
	// MaxFindPatternBytes bounds each pattern, in bytes.
	MaxFindPatternBytes = 256
	// MaxFindSnapshots is the most snapshots a search may name (a Windows
	// worker's command line holds them).
	MaxFindSnapshots = 100
)

// codeNotFound is the link error code of a missing snapshot or folder.
const codeNotFound = "not_found"

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
// child folder's. A measurement that stopped at the scan timeout is
// returned with Complete false. Invalid input is a
// *domain.ValidationError on "snapshot_id" or "path", as is a snapshot or
// a folder the repository does not have; ErrRepositoryDeleting while the
// repository's data is being deleted.
func (s *Service) Measure(ctx context.Context, actor domain.Actor, id domain.ResticRepoID, snapshotID, path string, workerID domain.WorkerID) (Measurement, error) {
	const op = "measure snapshot folder"
	if path == "" {
		path = "/"
	}
	t, password, err := s.scanTarget(ctx, actor, id, workerID, func(v *domain.Validation) {
		if !snapshotIDPattern.MatchString(snapshotID) {
			v.Add("snapshot_id", "must be a snapshot ID of 8 to 64 hex digits")
		}
		if msg := checkTreePath(path); msg != "" {
			v.Add("path", msg)
		}
	})
	if err != nil {
		return Measurement{}, fmt.Errorf("%s: %w", op, err)
	}
	m, err := s.prober.Measure(ctx, actor.UserID, t, snapshotID, path)
	if err != nil {
		return Measurement{}, fmt.Errorf("%s of repository %s: %w", op, id, scanError(err, password, func(pe *ProbeError) error {
			if missingSnapshot(pe.Message) {
				return fieldError("snapshot_id", notASnapshot)
			}
			return fieldError("path", "is not a folder in this snapshot")
		}))
	}
	if m.Path == "" {
		m.Path = path
	}
	return m, nil
}

// Find searches the snapshots of one of actor's repositories, through the
// given worker or, if zero, one the prober picks: newest snapshot first, at
// most the prober's bound of matches (Truncated beyond it). A search that
// stopped at the scan timeout or at the bound is returned with Complete
// false. Invalid input is a *domain.ValidationError on "patterns" (1 to
// MaxFindPatterns, each 1 to MaxFindPatternBytes bytes of valid UTF-8
// without NUL or line breaks) or "snapshot_ids" (at most MaxFindSnapshots
// IDs of 8 to 64 hex digits, also when one is not in the repository);
// ErrRepositoryDeleting while the repository's data is being deleted.
func (s *Service) Find(ctx context.Context, actor domain.Actor, id domain.ResticRepoID, q FindQuery, workerID domain.WorkerID) (FindResult, error) {
	const op = "search repository"
	t, password, err := s.scanTarget(ctx, actor, id, workerID, func(v *domain.Validation) {
		if msg := checkPatterns(q.Patterns); msg != "" {
			v.Add("patterns", msg)
		}
		if msg := checkSnapshotIDs(q.SnapshotIDs); msg != "" {
			v.Add("snapshot_ids", msg)
		}
	})
	if err != nil {
		return FindResult{}, fmt.Errorf("%s: %w", op, err)
	}
	q = FindQuery{Patterns: append([]string(nil), q.Patterns...), IgnoreCase: q.IgnoreCase, SnapshotIDs: append([]string(nil), q.SnapshotIDs...)}
	r, err := s.prober.Find(ctx, actor.UserID, t, q)
	if err != nil {
		return FindResult{}, fmt.Errorf("%s %s: %w", op, id, scanError(err, password, func(pe *ProbeError) error {
			for _, sid := range q.SnapshotIDs {
				if strings.Contains(pe.Message, sid) {
					return fieldError("snapshot_ids", sid+" "+notASnapshot)
				}
			}
			return fieldError("snapshot_ids", "name a snapshot that is not in this repository")
		}))
	}
	return r, nil
}

// Compare compares two snapshots of one of actor's repositories, the older
// (from) with the newer (to), the whole tree or one folder (a snapshot
// tree path; "" or "/" is the whole tree), through the given worker or, if
// zero, one the prober picks: at most the prober's bound of changes
// (Truncated beyond it; the statistics still cover everything). Without
// the statistics (Stats nil, Complete false) it stopped at the scan
// timeout. Invalid input is a *domain.ValidationError on
// "from_snapshot_id" or "to_snapshot_id" (8 to 64 hex digits, not
// "latest"; the same snapshot twice; a snapshot the repository does not
// have) or "path" (also a folder that is not in both snapshots);
// ErrRepositoryDeleting while the repository's data is being deleted.
func (s *Service) Compare(ctx context.Context, actor domain.Actor, id domain.ResticRepoID, fromID, toID, path string, workerID domain.WorkerID) (Comparison, error) {
	const op = "compare snapshots"
	if path == "" {
		path = "/"
	}
	t, password, err := s.scanTarget(ctx, actor, id, workerID, func(v *domain.Validation) {
		fromOK, toOK := snapshotIDPattern.MatchString(fromID), snapshotIDPattern.MatchString(toID)
		if !fromOK {
			v.Add("from_snapshot_id", "must be a snapshot ID of 8 to 64 hex digits")
		}
		if !toOK {
			v.Add("to_snapshot_id", "must be a snapshot ID of 8 to 64 hex digits")
		}
		// A short ID that starts another names the same snapshot (or
		// restic refuses it as ambiguous).
		if fromOK && toOK && (strings.HasPrefix(fromID, toID) || strings.HasPrefix(toID, fromID)) {
			v.Add("to_snapshot_id", "must be another snapshot than from_snapshot_id")
		}
		if msg := checkTreePath(path); msg != "" {
			v.Add("path", msg)
		}
	})
	if err != nil {
		return Comparison{}, fmt.Errorf("%s: %w", op, err)
	}
	c, err := s.prober.Diff(ctx, actor.UserID, t, fromID, toID, path)
	if err != nil {
		return Comparison{}, fmt.Errorf("%s of repository %s: %w", op, id, scanError(err, password, func(pe *ProbeError) error {
			if !missingSnapshot(pe.Message) {
				return fieldError("path", "is not a folder in both snapshots")
			}
			from, to := strings.Contains(pe.Message, fromID), strings.Contains(pe.Message, toID)
			switch {
			case from && !to:
				return fieldError("from_snapshot_id", notASnapshot)
			case to && !from:
				return fieldError("to_snapshot_id", notASnapshot)
			}
			const either = "this or the other snapshot is not in this repository"
			return &domain.ValidationError{Fields: []domain.FieldError{
				{Field: "from_snapshot_id", Message: either}, {Field: "to_snapshot_id", Message: either},
			}}
		}))
	}
	return c, nil
}

// notASnapshot is the message of a snapshot ID the repository does not
// have.
const notASnapshot = "is not a snapshot of this repository"

// scanTarget loads actor's repository id and checks the input with check
// (after the repository is found, so another user's repository is
// ErrNotFound whatever the input), then returns the probe target through
// workerID and the repository's password.
func (s *Service) scanTarget(ctx context.Context, actor domain.Actor, id domain.ResticRepoID, workerID domain.WorkerID, check func(*domain.Validation)) (Target, string, error) {
	r, err := s.owned(ctx, actor.UserID, id)
	if err != nil {
		return Target{}, "", err
	}
	var v domain.Validation
	check(&v)
	if err := v.Err(); err != nil {
		return Target{}, "", err
	}
	if r.Deleting {
		return Target{}, "", fmt.Errorf("repository %s: %w", id, ErrRepositoryDeleting)
	}
	password, err := s.open(r)
	if err != nil {
		return Target{}, "", fmt.Errorf("repository %s: %w", id, err)
	}
	return Target{RepositoryID: id, RemoteID: r.RemoteID, Path: r.Path, Password: password, WorkerID: workerID}, password, nil
}

// scanError maps an error of a scan: the worker's NOT_FOUND through
// notFound (a missing snapshot or folder, as a field error),
// PATH_NOT_ALLOWED to "worker_id" (the repository is outside the chosen
// worker's folders: the request's path is a snapshot path), anything else
// unchanged; the password never appears in the result.
func scanError(err error, password string, notFound func(*ProbeError) error) error {
	var pe *ProbeError
	if errors.As(err, &pe) {
		clean := *pe
		clean.Message = redact.Scrub(pe.Message, password)
		switch clean.Code {
		case codeNotFound:
			return notFound(&clean)
		case codePathNotAllowed:
			return fieldError("worker_id", "the repository is outside the worker's allowed folders")
		}
	}
	return scrubError(err, password)
}

// missingSnapshot reports whether a NOT_FOUND message is about a snapshot
// (restic's "no matching ID found", the worker's "has no snapshot"), not a
// folder.
func missingSnapshot(msg string) bool {
	return strings.Contains(msg, "no matching ID") || strings.Contains(msg, "no snapshot")
}

// checkPatterns returns what is wrong with a search's patterns ("" if
// nothing).
func checkPatterns(ps []string) string {
	switch {
	case len(ps) == 0:
		return "must have at least one pattern"
	case len(ps) > MaxFindPatterns:
		return fmt.Sprintf("must have at most %d patterns", MaxFindPatterns)
	}
	for i, p := range ps {
		which := "" // a single pattern is the field itself
		if len(ps) > 1 {
			which = fmt.Sprintf("pattern %d ", i+1)
		}
		switch {
		case p == "":
			return which + "must not be empty"
		case len(p) > MaxFindPatternBytes:
			return fmt.Sprintf("%smust be at most %d bytes", which, MaxFindPatternBytes)
		case !utf8.ValidString(p):
			return which + "must be valid UTF-8"
		case strings.ContainsAny(p, "\x00\r\n"):
			return which + "must not contain NUL characters or line breaks"
		}
	}
	return ""
}

// checkSnapshotIDs returns what is wrong with a search's snapshot IDs (""
// if nothing; none is every snapshot).
func checkSnapshotIDs(ids []string) string {
	if len(ids) > MaxFindSnapshots {
		return fmt.Sprintf("must name at most %d snapshots", MaxFindSnapshots)
	}
	for _, id := range ids {
		if !snapshotIDPattern.MatchString(id) {
			return "must be snapshot IDs of 8 to 64 hex digits"
		}
	}
	return ""
}
