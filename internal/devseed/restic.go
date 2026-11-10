package devseed

import (
	"context"
	"errors"
	"fmt"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Cipher encrypts the demo repositories' password as the restic service
// does (the controller's secrets keyring).
type Cipher interface {
	Encrypt(plaintext, associatedData []byte) ([]byte, error)
}

// seedRepositories stores the demo repositories with their snapshot
// caches. Nothing is created at their locations: the canned probes answer
// for them (Probes).
func (s *Seed) seedRepositories(ctx context.Context, now time.Time) error {
	clk := &stepClock{now: now}
	ids := &seedIDs{clock: clk}
	for _, d := range demoRepos() {
		remote, ok := s.Remotes[d.remote]
		if !ok {
			return fmt.Errorf("repository %q: unknown demo remote %q", d.name, d.remote)
		}
		created := now.Add(-d.age)
		clk.set(created)
		id := ids.NewResticRepoID()
		enc, err := s.d.Cipher.Encrypt([]byte(demoRepoPassword), restic.PasswordAD(id))
		if err != nil {
			return fmt.Errorf("repository %q: %w", d.name, err)
		}
		r := restic.Repo{
			ID: id, OwnerID: s.actor(d.owner).UserID, Name: d.name, Description: d.description, RemoteID: remote, Path: d.path,
			PasswordEnc: enc, ResticID: resticIDOf(d.key()), FormatVersion: 2, CreatedAt: created, UpdatedAt: created, Version: 1,
		}
		if err := s.d.Store.CreateRepo(ctx, r); err != nil {
			return fmt.Errorf("repository %q: %w", d.name, err)
		}
		s.Repositories[d.name] = id
		if d.count == 0 {
			continue
		}
		// The change mark is set once the history exists (markSnapshotCaches).
		if err := s.d.Store.ReplaceSnapshots(ctx, id, d.snapshots(now), false, now.Add(-d.refreshedAgo), restic.ChangeMark{}); err != nil {
			return fmt.Errorf("repository %q: snapshots: %w", d.name, err)
		}
	}
	return nil
}

// markSnapshotCaches stores each demo snapshot cache again with the change
// mark a refresh at its time would have read: the history's jobs that
// ended before it count, so only the ones that ended after it make the
// cache out of date.
func (s *Seed) markSnapshotCaches(ctx context.Context, _ time.Time) error {
	for _, d := range demoRepos() {
		id, ok := s.Repositories[d.name]
		if !ok || d.count == 0 {
			continue
		}
		r, err := s.d.Store.GetRepoRow(ctx, id)
		if err != nil {
			return fmt.Errorf("repository %q: %w", d.name, err)
		}
		if r.Deleted() {
			continue
		}
		list, err := s.d.Store.ListSnapshots(ctx, id)
		if err != nil {
			return fmt.Errorf("repository %q: snapshots: %w", d.name, err)
		}
		js, err := s.d.Store.ListJobs(ctx, jobs.ListFilter{RepositoryID: id, Limit: maxRepoJobs})
		if err != nil {
			return fmt.Errorf("repository %q: jobs: %w", d.name, err)
		}
		var mark restic.ChangeMark
		for _, j := range js {
			if restic.IsChange(j.Kind, j.Status, j.DispatchedAt, j.StartedAt) && !j.FinishedAt.After(list.RefreshedAt) {
				mark = mark.With(j.FinishedAt)
			}
		}
		if err := s.d.Store.ReplaceSnapshots(ctx, id, list.Snapshots, list.Truncated, list.RefreshedAt, mark); err != nil {
			return fmt.Errorf("repository %q: snapshots: %w", d.name, err)
		}
	}
	return nil
}

// maxRepoJobs bounds the jobs of one demo repository that
// markSnapshotCaches reads; the history has far fewer.
const maxRepoJobs = 10000

// bindProbes tells the canned probes the seed's start and the local roots
// of the workers that answer probes.
func (s *Seed) bindProbes(now time.Time) {
	if s.d.Probes == nil {
		return
	}
	roots := map[domain.WorkerID][]string{}
	for _, w := range demoWorkers() {
		if id, ok := s.Workers[w.name]; ok {
			roots[id] = w.info.LocalRoots
		}
	}
	s.d.Probes.bind(now, roots)
}

// storeRepos checks the repository of the history's jobs and schedules
// against the store, as the restic service does for the controller's.
type storeRepos struct{ store restic.Repository }

// UsableRepository implements the jobs and schedules services'
// RepositoryChecker.
func (c storeRepos) UsableRepository(ctx context.Context, owner domain.UserID, id domain.ResticRepoID, jobID domain.JobID) (string, domain.RemoteID, error) {
	r, err := c.store.GetRepo(ctx, id)
	switch {
	case err != nil:
		return "", domain.RemoteID{}, err
	case r.OwnerID != owner || r.Deleted():
		return "", domain.RemoteID{}, fmt.Errorf("repository %s: %w", id, domain.ErrNotFound)
	case r.Deleting && (jobID.IsZero() || jobID != r.DeletionJobID):
		return "", domain.RemoteID{}, fmt.Errorf("repository %s: %w", id, restic.ErrRepositoryDeleting)
	}
	return r.Name, r.RemoteID, nil
}

// deleteData replays a repository deletion with its data, requested at at
// through worker: the repository is marked deleting (which pauses its
// schedules) and its restic_delete_data job is created with the reserved
// ID. With finished, the job runs for run and succeeds, and the
// repository is deleted; otherwise the job stays queued (behind the jobs
// that hold the repository's key) and the repository stays deleting.
func (h *history) deleteData(ctx context.Context, repo, worker string, at time.Time, finished bool, run time.Duration) error {
	id, ok := h.s.Repositories[repo]
	if !ok {
		return fmt.Errorf("unknown demo repository %q", repo)
	}
	at = h.jobTime(at)
	h.clk.set(at)
	jobID := h.ids.NewJobID()
	if _, err := h.s.d.Store.MarkDeleting(ctx, id, jobID, at); err != nil {
		return err
	}
	dj := demoJob{owner: ownerAdmin, worker: worker, def: deleteDataDef(repo), status: domain.JobQueued}
	if finished {
		dj.status, dj.queue, dj.run, dj.samples = domain.JobSucceeded, 2*time.Second, run, 6
		dj.stats.Deletes = 1864
		dj.output = &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticDeleteData{ResticDeleteData: &syncrv1.ResticDeleteDataOutput{
			DeletedObjects: 1864, RemovedParts: []string{"locks", "snapshots", "index", "data", "keys", "config"}, DirectoryRemoved: true,
		}}}
	}
	got, err := h.s.seedJob(ctx, h.jobs, h.clk, h.now, at, dj, jobPreset{id: jobID})
	if err != nil {
		return err
	}
	if got != jobID {
		return errors.New("the deletion job did not take its reserved ID")
	}
	h.s.Jobs = append(h.s.Jobs, jobID)
	if !finished {
		return nil
	}
	end := at.Add(dj.queue + dj.run)
	h.clk.set(end)
	if err := h.s.d.Store.FinishDeletion(ctx, id, jobID, restic.DeletionOutcome{Succeeded: true}, end); err != nil {
		return err
	}
	return h.notify(ctx)
}
