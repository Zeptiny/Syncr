package memstore

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// CreateJob implements jobs.Repository.
func (s *Store) CreateJob(_ context.Context, j jobs.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[j.ID]; ok {
		return fmt.Errorf("job %s: %w", j.ID, domain.ErrAlreadyExists)
	}
	if w, ok := s.workers[j.WorkerID]; !ok || w.Removed() {
		return fmt.Errorf("worker %s: %w", j.WorkerID, domain.ErrNotFound)
	}
	s.jobs[j.ID] = cloneJob(j)
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
		if (f.WorkerID.IsZero() || j.WorkerID == f.WorkerID) &&
			(len(f.Statuses) == 0 || slices.Contains(f.Statuses, j.Status)) &&
			(f.Before.IsZero() || compareIDs(j.ID, f.Before) < 0) {
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

// ListQueuedJobs implements jobs.Repository.
func (s *Store) ListQueuedJobs(_ context.Context, before time.Time, limit int) ([]jobs.Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []jobs.Job
	for _, j := range s.jobs {
		if j.Status == domain.JobQueued && j.QueuedAt.Before(before) {
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

// ListJobSamples implements jobs.SampleRepository.
func (s *Store) ListJobSamples(_ context.Context, id domain.JobID) ([]jobs.Sample, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.samples[id]), nil
}
