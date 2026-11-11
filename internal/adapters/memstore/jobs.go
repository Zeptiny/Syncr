package memstore

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// CreateJob implements jobs.Repository. Like the database's foreign keys,
// it requires the owner, every remote (deleted remotes exist), a set
// schedule and a set repository (deleted repositories exist) to exist.
func (s *Store) CreateJob(_ context.Context, j jobs.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[j.ID]; ok {
		return fmt.Errorf("job %s: %w", j.ID, domain.ErrAlreadyExists)
	}
	if w, ok := s.workers[j.WorkerID]; !ok || w.Removed() {
		return fmt.Errorf("worker %s: %w", j.WorkerID, domain.ErrNotFound)
	}
	if _, ok := s.users[j.OwnerID]; !ok {
		return fmt.Errorf("owner %s of job %s: %w", j.OwnerID, j.ID, domain.ErrNotFound)
	}
	for _, id := range j.RemoteIDs {
		if _, ok := s.remotes[id]; !ok {
			return fmt.Errorf("remote %s of job %s: %w", id, j.ID, domain.ErrNotFound)
		}
	}
	if !j.ScheduleID.IsZero() {
		if _, ok := s.schedules[j.ScheduleID]; !ok {
			return fmt.Errorf("schedule %s of job %s: %w", j.ScheduleID, j.ID, domain.ErrNotFound)
		}
	}
	if !j.RepositoryID.IsZero() {
		if _, ok := s.repos[j.RepositoryID]; !ok {
			return fmt.Errorf("repository %s of job %s: %w", j.RepositoryID, j.ID, domain.ErrNotFound)
		}
	}
	j = cloneJob(j)
	if len(j.ConcurrencyKeys) == 0 {
		j.ConcurrencyKeys = nil // the stored empty array reads back as nil
	}
	// Stored like the job_remotes rows: a set, read back by ID.
	slices.SortFunc(j.RemoteIDs, compareIDs)
	j.RemoteIDs = slices.Compact(j.RemoteIDs)
	s.jobs[j.ID] = j
	return nil
}

// GetJob implements jobs.Repository.
func (s *Store) GetJob(_ context.Context, id domain.JobID) (jobs.Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	if !ok {
		return jobs.Job{}, fmt.Errorf("job %s: %w", id, domain.ErrNotFound)
	}
	return cloneJob(j), nil
}

// UpdateJob implements jobs.Repository.
func (s *Store) UpdateJob(_ context.Context, j jobs.Job) (jobs.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.jobs[j.ID]
	if !ok {
		return jobs.Job{}, fmt.Errorf("job %s: %w", j.ID, domain.ErrNotFound)
	}
	if cur.Version != j.Version {
		return jobs.Job{}, fmt.Errorf("job %s is at version %d, not %d: %w", j.ID, cur.Version, j.Version, domain.ErrConflict)
	}
	next := cloneJob(j)
	// Immutable fields keep their stored values.
	next.WorkerID, next.OwnerID, next.Kind, next.Definition, next.CreatedAt = cur.WorkerID, cur.OwnerID, cur.Kind, cur.Definition, cur.CreatedAt
	next.ScheduleID, next.RepositoryID, next.ConcurrencyKeys = cur.ScheduleID, cur.RepositoryID, cur.ConcurrencyKeys
	next.RemoteIDs, next.SearchText = cur.RemoteIDs, cur.SearchText
	next.Version = cur.Version + 1
	s.jobs[j.ID] = next
	return cloneJob(next), nil
}

// ListJobs implements jobs.Repository.
func (s *Store) ListJobs(_ context.Context, f jobs.ListFilter) ([]jobs.Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []jobs.Job
	for _, j := range s.jobs {
		if matchesJob(j, f) {
			out = append(out, j)
		}
	}
	slices.SortFunc(out, func(a, b jobs.Job) int { return compareIDs(b.ID, a.ID) })
	if f.Limit >= 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	for i := range out {
		out[i] = cloneJob(out[i])
	}
	return out, nil
}

// matchesJob reports whether j matches every set field of f.
func matchesJob(j jobs.Job, f jobs.ListFilter) bool {
	return (f.OwnerID.IsZero() || j.OwnerID == f.OwnerID) &&
		(f.WorkerID.IsZero() || j.WorkerID == f.WorkerID) &&
		(len(f.Statuses) == 0 || slices.Contains(f.Statuses, j.Status)) &&
		(f.RemoteID.IsZero() || slices.Contains(j.RemoteIDs, f.RemoteID)) &&
		(f.ScheduleID.IsZero() || j.ScheduleID == f.ScheduleID) &&
		(f.RepositoryID.IsZero() || j.RepositoryID == f.RepositoryID) &&
		(len(f.Kinds) == 0 || slices.Contains(f.Kinds, j.Kind)) &&
		(f.Query == "" || strings.Contains(j.SearchText, f.Query)) &&
		(f.CreatedAfter.IsZero() || !j.CreatedAt.Before(f.CreatedAfter)) &&
		(f.CreatedBefore.IsZero() || j.CreatedAt.Before(f.CreatedBefore)) &&
		(f.Before.IsZero() || compareIDs(j.ID, f.Before) < 0)
}

// ListUnfinishedJobs implements jobs.Repository.
func (s *Store) ListUnfinishedJobs(_ context.Context, workerID domain.WorkerID) ([]jobs.Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []jobs.Job
	for _, j := range s.jobs {
		if j.WorkerID == workerID && !j.Status.Terminal() {
			out = append(out, cloneJob(j))
		}
	}
	slices.SortFunc(out, func(a, b jobs.Job) int { return compareIDs(a.ID, b.ID) })
	return out, nil
}

// ListQueuedJobs implements jobs.Repository, key-aware like the query: a
// job with keys is no candidate while an older sharer is unfinished or
// finished at or after before.
func (s *Store) ListQueuedJobs(_ context.Context, before time.Time, limit int) ([]jobs.Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []jobs.Job
	for _, j := range s.jobs {
		if j.Status != domain.JobQueued || !j.QueuedAt.Before(before) {
			continue
		}
		waiting := slices.ContainsFunc(s.olderSharersLocked(j), func(o jobs.Job) bool {
			return !o.Status.Terminal() || !o.FinishedAt.IsZero() && !o.FinishedAt.Before(before)
		})
		if !waiting {
			out = append(out, j)
		}
	}
	slices.SortFunc(out, func(a, b jobs.Job) int {
		if c := a.QueuedAt.Compare(b.QueuedAt); c != 0 {
			return c
		}
		return compareIDs(a.ID, b.ID)
	})
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	for i := range out {
		out[i] = cloneJob(out[i])
	}
	return out, nil
}

// AppendJobSample implements jobs.SampleRepository.
func (s *Store) AppendJobSample(_ context.Context, smp jobs.Sample) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[smp.JobID]; !ok {
		return fmt.Errorf("job %s: %w", smp.JobID, domain.ErrNotFound)
	}
	list := s.samples[smp.JobID]
	i, found := slices.BinarySearchFunc(list, smp.Time, func(e jobs.Sample, t time.Time) int { return e.Time.Compare(t) })
	if found {
		return nil // idempotent on (JobID, Time)
	}
	s.samples[smp.JobID] = slices.Insert(list, i, smp)
	return nil
}

// LatestJobSample implements jobs.SampleRepository.
func (s *Store) LatestJobSample(_ context.Context, id domain.JobID) (jobs.Sample, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := s.samples[id]
	if len(list) == 0 {
		return jobs.Sample{}, false, nil
	}
	return list[len(list)-1], true, nil
}

// LatestJobSamples implements jobs.SampleRepository.
func (s *Store) LatestJobSamples(_ context.Context, ids []domain.JobID) (map[domain.JobID]jobs.Sample, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[domain.JobID]jobs.Sample, len(ids))
	for _, id := range ids {
		if list := s.samples[id]; len(list) > 0 {
			out[id] = list[len(list)-1]
		}
	}
	return out, nil
}

// ListJobSamples implements jobs.SampleRepository, with the port's exact
// thinning rule.
func (s *Store) ListJobSamples(_ context.Context, id domain.JobID, maxPoints int) ([]jobs.Sample, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := s.samples[id]
	n := len(list)
	if maxPoints <= 0 || n <= maxPoints {
		return slices.Clone(list), n, nil
	}
	out := make([]jobs.Sample, 0, maxPoints)
	for r, smp := range list {
		if keepSample(r, n, maxPoints) {
			out = append(out, smp)
		}
	}
	return out, n, nil
}

// keepSample reports whether ListJobSamples keeps the sample at rank r of
// n when thinning them to maxPoints (n > maxPoints >= 1): the first one,
// and then the first of each of maxPoints equal spans of ranks. The
// Postgres query computes the same with integer division (r >= 1, so both
// truncate like floor).
func keepSample(r, n, maxPoints int) bool {
	if r == 0 {
		return true
	}
	return r*(maxPoints-1)/(n-1) > (r-1)*(maxPoints-1)/(n-1)
}

// PurgeJobSamples implements jobs.SampleRepository.
func (s *Store) PurgeJobSamples(_ context.Context, finishedBefore time.Time, maxJobs int) (int, error) {
	if maxJobs <= 0 {
		return 0, fmt.Errorf("purge job samples: %w: max jobs %d is not positive", domain.ErrInvalidArgument, maxJobs)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []jobs.Job
	for jid, list := range s.samples {
		if j, ok := s.jobs[jid]; ok && len(list) > 0 && !j.FinishedAt.IsZero() && j.FinishedAt.Before(finishedBefore) {
			due = append(due, j)
		}
	}
	// Those that finished first first, as the index orders them; the port
	// leaves the order of ties open, the ID breaks them here.
	slices.SortFunc(due, func(a, b jobs.Job) int {
		return cmp.Or(a.FinishedAt.Compare(b.FinishedAt), compareIDs(a.ID, b.ID))
	})
	deleted := 0
	for _, j := range due[:min(len(due), maxJobs)] {
		deleted += len(s.samples[j.ID])
		delete(s.samples, j.ID)
	}
	return deleted, nil
}

// olderJob orders jobs by (CreatedAt, ID), the order of the concurrency
// keys.
func olderJob(a, b jobs.Job) int {
	return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), compareIDs(a.ID, b.ID))
}

// sharesKey reports whether a and b have a key in common (Postgres &&).
func sharesKey(a, b []string) bool {
	return slices.ContainsFunc(a, func(k string) bool { return slices.Contains(b, k) })
}

// olderSharersLocked returns the jobs older than j (by olderJob) that share
// one of its keys, oldest first; none for a job without keys.
func (s *Store) olderSharersLocked(j jobs.Job) []jobs.Job {
	if len(j.ConcurrencyKeys) == 0 {
		return nil
	}
	var out []jobs.Job
	for _, o := range s.jobs {
		if o.ID != j.ID && sharesKey(o.ConcurrencyKeys, j.ConcurrencyKeys) && olderJob(o, j) < 0 {
			out = append(out, o)
		}
	}
	slices.SortFunc(out, olderJob)
	return out
}

// ListJobBlockers implements jobs.Repository.
func (s *Store) ListJobBlockers(_ context.Context, ids []domain.JobID) (map[domain.JobID]domain.JobID, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[domain.JobID]domain.JobID)
	for _, id := range ids {
		j, ok := s.jobs[id]
		if !ok {
			continue
		}
		for _, o := range s.olderSharersLocked(j) {
			if !o.Status.Terminal() {
				out[id] = o.ID
				break
			}
		}
	}
	return out, nil
}

// ListWorkersWaitingOnKeys implements jobs.Repository.
func (s *Store) ListWorkersWaitingOnKeys(_ context.Context, keys []string) ([]domain.WorkerID, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []domain.WorkerID{}
	for _, j := range s.jobs {
		if j.Status == domain.JobQueued && sharesKey(j.ConcurrencyKeys, keys) && !slices.Contains(out, j.WorkerID) {
			out = append(out, j.WorkerID)
		}
	}
	slices.SortFunc(out, compareIDs)
	return out, nil
}
