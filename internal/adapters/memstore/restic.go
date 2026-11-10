package memstore

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// This file implements restic.Repository with the semantics of the Postgres
// schema (migration 00007): the foreign keys and CHECK constraints of
// restic_repositories and restic_snapshots, the unique indexes among live
// repositories (owner and lower(name), location, owner and restic ID), and
// the statistics LATERALs of GetRepository and ListRepositories.

// resticRow is one restic_repositories row with its snapshot cache.
type resticRow struct {
	// repo is stored with zero Stats (they are computed on read).
	repo        restic.Repo
	refreshedAt time.Time
	truncated   bool
	// mark is the ChangeMark stored with the cache.
	mark restic.ChangeMark
	// snaps are newest first (Time descending, then ID).
	snaps []restic.Snapshot
}

func cloneRepo(r restic.Repo) restic.Repo {
	r.PasswordEnc = slices.Clone(r.PasswordEnc)
	return r
}

func cloneSnapshot(s restic.Snapshot) restic.Snapshot {
	s.Paths = slices.Clone(s.Paths)
	s.Tags = slices.Clone(s.Tags)
	if s.Summary != nil {
		sum := *s.Summary
		s.Summary = &sum
	}
	return s
}

func cloneSnapshots(in []restic.Snapshot) []restic.Snapshot {
	out := make([]restic.Snapshot, len(in))
	for i, s := range in {
		out[i] = cloneSnapshot(s)
	}
	return out
}

// compareSnapshots orders snapshots newest first, then by ID (the order of
// ListSnapshots and of the newest-snapshot LATERAL).
func compareSnapshots(a, b restic.Snapshot) int {
	return cmp.Or(b.Time.Compare(a.Time), strings.Compare(a.ID, b.ID))
}

// isHex64 reports whether s is 64 lower-case hex digits (the schema's
// CHECK on restic IDs and snapshot IDs).
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// checkRepoLocked enforces the CHECK constraints and the unique indexes of
// a repository row about to be stored (r.ID's stored row, if any, is
// replaced by r).
func (s *Store) checkRepoLocked(r restic.Repo) error {
	invalid := func(what string) error {
		return fmt.Errorf("repository %s: %w: %s", r.ID, domain.ErrInvalidArgument, what)
	}
	switch {
	case utf8.RuneCountInString(r.Name) < 1 || utf8.RuneCountInString(r.Name) > restic.MaxNameLen:
		return invalid("name length")
	case utf8.RuneCountInString(r.Description) > restic.MaxDescriptionLen:
		return invalid("description length")
	case utf8.RuneCountInString(r.Path) > restic.MaxPathLen:
		return invalid("path length")
	case !isHex64(r.ResticID):
		return invalid("restic id")
	case r.FormatVersion < 0:
		return invalid("format version")
	case r.Version < 1:
		return invalid("version")
	case utf8.RuneCountInString(r.DeletionError) > restic.MaxDeletionErrorLen:
		return invalid("deletion error length")
	case r.Deleted() != (r.PasswordEnc == nil):
		return invalid("a live repository needs a password, a deleted one has none")
	case r.Deleting && (r.DeletionJobID.IsZero() || r.DeletionRequestedAt.IsZero() || r.Deleted()):
		return invalid("deleting without a job")
	case !r.DataDeletedAt.IsZero() && !r.Deleted():
		return invalid("data deleted but the repository is live")
	}
	if r.Deleted() {
		return nil
	}
	lower := strings.ToLower(r.Name)
	for _, o := range s.repos {
		other := o.repo
		if other.ID == r.ID || other.Deleted() {
			continue
		}
		switch {
		case other.OwnerID == r.OwnerID && strings.ToLower(other.Name) == lower:
			return fmt.Errorf("repository name %q: %w", r.Name, domain.ErrAlreadyExists)
		case other.RemoteID == r.RemoteID && other.Path == r.Path:
			return fmt.Errorf("repository location: %w", domain.ErrAlreadyExists)
		case other.OwnerID == r.OwnerID && other.ResticID == r.ResticID:
			return fmt.Errorf("restic repository %s: %w", r.ResticID, domain.ErrAlreadyExists)
		}
	}
	return nil
}

// withStatsLocked returns a copy of the row's repository with its Stats.
func (s *Store) withStatsLocked(row resticRow) restic.Repo {
	r := cloneRepo(row.repo)
	st := restic.RepoStats{
		SnapshotCount:        len(row.snaps),
		SnapshotsRefreshedAt: row.refreshedAt,
		SnapshotsTruncated:   row.truncated,
		SnapshotsMark:        row.mark,
		ChangeMark:           s.changeMarkLocked(r.ID),
	}
	if len(row.snaps) > 0 {
		st.LatestSnapshotAt = row.snaps[0].Time
		if sum := row.snaps[0].Summary; sum != nil {
			st.LatestSnapshotBytes = sum.TotalBytesProcessed
		}
	}
	for _, j := range s.jobs {
		if j.RepositoryID != r.ID {
			continue
		}
		var ref *restic.JobRef
		switch j.Kind {
		case restic.KindBackup:
			ref = &st.LastBackup
		case restic.KindCheck:
			ref = &st.LastCheck
		default:
			continue
		}
		if ref.ID.IsZero() || compareIDs(j.ID, ref.ID) > 0 {
			*ref = restic.JobRef{ID: j.ID, Status: j.Status, CreatedAt: j.CreatedAt, FinishedAt: j.FinishedAt}
		}
	}
	for _, sc := range s.schedules {
		if sc.RepositoryID == r.ID {
			st.ScheduleCount++
		}
	}
	r.Stats = st
	return r
}

// changeMarkLocked returns the current ChangeMark of repository id.
func (s *Store) changeMarkLocked(id domain.ResticRepoID) restic.ChangeMark {
	var m restic.ChangeMark
	for _, j := range s.jobs {
		if j.RepositoryID == id && restic.IsChange(j.Kind, j.Status, j.DispatchedAt, j.StartedAt) {
			m = m.With(j.FinishedAt)
		}
	}
	return m
}

// CreateRepo implements restic.Repository.
func (s *Store) CreateRepo(_ context.Context, r restic.Repo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.repos[r.ID]; ok {
		return fmt.Errorf("repository %s: %w", r.ID, domain.ErrAlreadyExists)
	}
	if _, ok := s.users[r.OwnerID]; !ok {
		return fmt.Errorf("owner %s of repository %s: %w", r.OwnerID, r.ID, domain.ErrNotFound)
	}
	if rem, ok := s.remotes[r.RemoteID]; !ok || rem.Deleted() {
		return fmt.Errorf("remote %s of repository %s: %w", r.RemoteID, r.ID, domain.ErrNotFound)
	}
	// Only the columns of a new repository are written.
	row := restic.Repo{
		ID: r.ID, OwnerID: r.OwnerID, Name: r.Name, Description: r.Description, RemoteID: r.RemoteID, Path: r.Path,
		PasswordEnc: slices.Clone(r.PasswordEnc), ResticID: r.ResticID, FormatVersion: r.FormatVersion,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Version: r.Version,
	}
	if err := s.checkRepoLocked(row); err != nil {
		return fmt.Errorf("create repository %s: %w", r.ID, err)
	}
	s.repos[r.ID] = resticRow{repo: row}
	return nil
}

// GetRepo implements restic.Repository.
func (s *Store) GetRepo(_ context.Context, id domain.ResticRepoID) (restic.Repo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	row, ok := s.repos[id]
	if !ok {
		return restic.Repo{}, fmt.Errorf("repository %s: %w", id, domain.ErrNotFound)
	}
	return s.withStatsLocked(row), nil
}

// GetRepoRow implements restic.Repository.
func (s *Store) GetRepoRow(_ context.Context, id domain.ResticRepoID) (restic.Repo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	row, ok := s.repos[id]
	if !ok {
		return restic.Repo{}, fmt.Errorf("repository %s: %w", id, domain.ErrNotFound)
	}
	return cloneRepo(row.repo), nil
}

// GetRepos implements restic.Repository.
func (s *Store) GetRepos(_ context.Context, ids []domain.ResticRepoID) ([]restic.Repo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids = slices.Clone(ids)
	slices.SortFunc(ids, compareIDs)
	out := []restic.Repo{}
	for _, id := range slices.Compact(ids) {
		if row, ok := s.repos[id]; ok {
			out = append(out, cloneRepo(row.repo))
		}
	}
	return out, nil
}

// ListRepos implements restic.Repository.
func (s *Store) ListRepos(_ context.Context, f restic.ListFilter) ([]restic.Repo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listReposLocked(f, s.withStatsLocked), nil
}

// ListRepoRows implements restic.Repository.
func (s *Store) ListRepoRows(_ context.Context, f restic.ListFilter) ([]restic.Repo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listReposLocked(f, func(row resticRow) restic.Repo { return cloneRepo(row.repo) }), nil
}

// listReposLocked returns the live repositories matching f, read with
// read, by lower-cased name, then ID.
func (s *Store) listReposLocked(f restic.ListFilter, read func(resticRow) restic.Repo) []restic.Repo {
	out := []restic.Repo{}
	for _, row := range s.repos {
		r := row.repo
		if !r.Deleted() && (f.OwnerID.IsZero() || r.OwnerID == f.OwnerID) && (f.RemoteID.IsZero() || r.RemoteID == f.RemoteID) {
			out = append(out, read(row))
		}
	}
	slices.SortFunc(out, func(a, b restic.Repo) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), compareIDs(a.ID, b.ID))
	})
	return out
}

// UpdateRepo implements restic.Repository.
func (s *Store) UpdateRepo(_ context.Context, r restic.Repo) (restic.Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.repos[r.ID]
	if !ok || row.repo.Deleted() {
		return restic.Repo{}, fmt.Errorf("repository %s: %w", r.ID, domain.ErrNotFound)
	}
	if row.repo.Version != r.Version {
		return restic.Repo{}, fmt.Errorf("repository %s is at version %d, not %d: %w", r.ID, row.repo.Version, r.Version, domain.ErrConflict)
	}
	next := row.repo
	next.Name, next.Description, next.PasswordEnc, next.UpdatedAt = r.Name, r.Description, slices.Clone(r.PasswordEnc), r.UpdatedAt
	next.Version++
	if err := s.checkRepoLocked(next); err != nil {
		return restic.Repo{}, fmt.Errorf("update repository %s: %w", r.ID, err)
	}
	row.repo = next
	s.repos[r.ID] = row
	return s.withStatsLocked(row), nil
}

// DeleteRepo implements restic.Repository.
func (s *Store) DeleteRepo(_ context.Context, id domain.ResticRepoID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.repos[id]
	switch {
	case !ok || row.repo.Deleted():
		return fmt.Errorf("repository %s: %w", id, domain.ErrNotFound)
	case row.repo.Deleting:
		return fmt.Errorf("repository %s: its data is being deleted (job %s): %w", id, row.repo.DeletionJobID, domain.ErrFailedPrecondition)
	}
	row.repo.DeletedAt, row.repo.UpdatedAt, row.repo.PasswordEnc = at, at, nil
	row.repo.Version++
	row.snaps = nil
	s.repos[id] = row
	return nil
}

// MarkDeleting implements restic.Repository: the repository, its enabled
// schedules and their open runs change under one lock, like the one
// transaction of the database.
func (s *Store) MarkDeleting(_ context.Context, id domain.ResticRepoID, jobID domain.JobID, at time.Time) (restic.Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if jobID.IsZero() {
		return restic.Repo{}, fmt.Errorf("mark repository %s deleting: %w: no job", id, domain.ErrInvalidArgument)
	}
	row, ok := s.repos[id]
	switch {
	case !ok || row.repo.Deleted():
		return restic.Repo{}, fmt.Errorf("repository %s: %w", id, domain.ErrNotFound)
	case row.repo.Deleting:
		return restic.Repo{}, fmt.Errorf("repository %s is already deleting (job %s): %w", id, row.repo.DeletionJobID, domain.ErrConflict)
	}
	r := &row.repo
	r.Deleting, r.DeletionJobID, r.DeletionRequestedAt, r.DeletionError, r.DeletionCancelled, r.UpdatedAt = true, jobID, at, "", false, at
	r.Version++
	s.repos[id] = row
	for sid, sc := range s.schedules {
		if sc.RepositoryID != id {
			continue
		}
		if sc.Enabled {
			sc.Enabled, sc.NextRunAt, sc.PausedReason, sc.UpdatedAt = false, time.Time{}, schedules.ReasonRepositoryDeleting, at
			sc.Version++
			s.schedules[sid] = sc
		}
		for rid, run := range s.runs {
			if run.ScheduleID == sid && run.Status.Open() {
				run.Status, run.Reason, run.Message, run.UpdatedAt = schedules.RunCancelled, schedules.ReasonRepositoryDeleting, "", at
				s.runs[rid] = run
			}
		}
	}
	return s.withStatsLocked(row), nil
}

// ListPendingDeletions implements restic.Repository.
func (s *Store) ListPendingDeletions(_ context.Context, startedBefore time.Time, limit int) ([]restic.PendingDeletion, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []restic.PendingDeletion{}
	for _, row := range s.repos {
		r := row.repo
		if !r.Deleting {
			continue
		}
		p := restic.PendingDeletion{RepoID: r.ID, OwnerID: r.OwnerID, JobID: r.DeletionJobID, RequestedAt: r.DeletionRequestedAt}
		j, exists := s.jobs[r.DeletionJobID]
		switch {
		case exists && j.Status.Terminal():
			p.JobStatus, p.JobErrorCode, p.JobErrorMessage = j.Status, j.Error.Code, j.Error.Message
		case !exists && r.DeletionRequestedAt.Before(startedBefore):
		default:
			continue
		}
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b restic.PendingDeletion) int {
		return cmp.Or(a.RequestedAt.Compare(b.RequestedAt), compareIDs(a.RepoID, b.RepoID))
	})
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// FinishDeletion implements restic.Repository.
func (s *Store) FinishDeletion(_ context.Context, id domain.ResticRepoID, jobID domain.JobID, o restic.DeletionOutcome, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.repos[id]
	if !ok || !row.repo.Deleting || row.repo.DeletionJobID != jobID {
		return fmt.Errorf("finish deletion of repository %s by job %s: not deleting with that job: %w", id, jobID, domain.ErrConflict)
	}
	r := &row.repo
	r.Deleting, r.UpdatedAt = false, at
	r.Version++
	if o.Succeeded {
		r.DeletedAt, r.DataDeletedAt, r.PasswordEnc = at, at, nil
		row.snaps = nil
	} else {
		r.DeletionError, r.DeletionCancelled = clipRunes(o.Error, restic.MaxDeletionErrorLen), o.Cancelled
	}
	s.repos[id] = row
	return nil
}

// clipRunes returns s cut to at most n runes.
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// ReplaceSnapshots implements restic.Repository.
func (s *Store) ReplaceSnapshots(_ context.Context, id domain.ResticRepoID, snaps []restic.Snapshot, truncated bool, refreshedAt time.Time, mark restic.ChangeMark) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.repos[id]
	if !ok || row.repo.Deleted() {
		return fmt.Errorf("repository %s: %w", id, domain.ErrNotFound)
	}
	seen := make(map[string]bool, len(snaps))
	for _, sn := range snaps {
		switch {
		case !isHex64(sn.ID):
			return fmt.Errorf("snapshot of repository %s: %w: the ID is not 64 hex digits", id, domain.ErrInvalidArgument)
		case seen[sn.ID]:
			return fmt.Errorf("snapshot %s of repository %s: %w", sn.ID, id, domain.ErrAlreadyExists)
		}
		seen[sn.ID] = true
	}
	row.snaps = cloneSnapshots(snaps)
	slices.SortFunc(row.snaps, compareSnapshots)
	row.refreshedAt, row.truncated, row.mark = refreshedAt, truncated, mark
	s.repos[id] = row
	return nil
}

// ListSnapshots implements restic.Repository.
func (s *Store) ListSnapshots(_ context.Context, id domain.ResticRepoID) (restic.SnapshotList, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	row, ok := s.repos[id]
	if !ok {
		return restic.SnapshotList{}, fmt.Errorf("repository %s: %w", id, domain.ErrNotFound)
	}
	return restic.SnapshotList{
		Snapshots: cloneSnapshots(row.snaps), RefreshedAt: row.refreshedAt, Truncated: row.truncated,
		Mark: row.mark, CurrentMark: s.changeMarkLocked(id),
	}, nil
}

// repositoryOnRemoteLocked returns the name of a live repository on remote
// (the first by lower-cased name, then ID), or false.
func (s *Store) repositoryOnRemoteLocked(remote domain.RemoteID) (string, bool) {
	var found *restic.Repo
	for _, row := range s.repos {
		r := row.repo
		if r.RemoteID != remote || r.Deleted() {
			continue
		}
		if found == nil || cmp.Or(cmp.Compare(strings.ToLower(r.Name), strings.ToLower(found.Name)), compareIDs(r.ID, found.ID)) < 0 {
			found = &r
		}
	}
	if found == nil {
		return "", false
	}
	return found.Name, true
}
