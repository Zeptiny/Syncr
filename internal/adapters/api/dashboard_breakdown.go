package api

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// durMicros converts a sum of microseconds; zero stays unset. It never
// goes through time.Duration, whose 292 years a sum may exceed.
func durMicros(us int64) *durationpb.Duration {
	if us == 0 {
		return nil
	}
	// The remainder is below a million microseconds, so the nanoseconds fit.
	return &durationpb.Duration{Seconds: us / 1e6, Nanos: int32(us % 1e6 * 1000)}
}

// micros returns d in whole microseconds (a Duration field of the API's
// messages, which never exceeds what the dashboard sums into it).
func micros(d *durationpb.Duration) int64 {
	return d.GetSeconds()*1e6 + int64(d.GetNanos())/1000
}

// microsToSeconds converts a sum of microseconds to seconds.
func microsToSeconds(us int64) float64 { return float64(us) / 1e6 }

// addToGroup adds the jobs of r to the dashboard group g.
func addToGroup(g *syncrv1.ActivityGroup, r jobs.TotalsRow) {
	switch r.Status {
	case domain.JobSucceeded:
		g.Succeeded += r.Jobs
	case domain.JobFailed:
		g.Failed += r.Jobs
	case domain.JobInterrupted:
		g.Interrupted += r.Jobs
	case domain.JobCancelled:
		g.Cancelled += r.Jobs
	}
	g.Bytes += r.Bytes
	g.Files += r.Files
	g.Started += r.Started
	g.Dispatched += r.Dispatched
	g.RunTime = durMicros(micros(g.GetRunTime()) + r.RunTimeMicros)
	g.QueueWait = durMicros(micros(g.GetQueueWait()) + r.QueueWaitMicros)
}

func groupJobs(g *syncrv1.ActivityGroup) int64 {
	return g.Succeeded + g.Failed + g.Interrupted + g.Cancelled
}

// sortedGroups returns the groups of m, most jobs first, then by name and
// key.
func sortedGroups(m map[string]*syncrv1.ActivityGroup) []*syncrv1.ActivityGroup {
	out := make([]*syncrv1.ActivityGroup, 0, len(m))
	for _, g := range m {
		out = append(out, g)
	}
	slices.SortFunc(out, func(a, b *syncrv1.ActivityGroup) int {
		return cmp.Or(cmp.Compare(groupJobs(b), groupJobs(a)), strings.Compare(a.Name, b.Name), strings.Compare(a.Key, b.Key))
	})
	return out
}

// breakdowns groups the range's finished jobs by kind and by worker (named
// from ws, which includes removed workers), and counts the failed and
// interrupted ones by error code.
func breakdowns(rows []jobs.TotalsRow, ws []workers.Worker) (kinds, byWorker []*syncrv1.ActivityGroup, reasons []*syncrv1.FailureReason) {
	names := make(map[domain.WorkerID]string, len(ws))
	for _, w := range ws {
		names[w.ID] = w.Name
	}
	kindGroups := map[string]*syncrv1.ActivityGroup{}
	workerGroups := map[string]*syncrv1.ActivityGroup{}
	type reason struct {
		msg   *syncrv1.FailureReason
		kinds map[string]int64
	}
	byCode := map[syncrv1.JobErrorCode]*reason{}
	for _, r := range rows {
		k := kindGroups[r.Kind]
		if k == nil {
			k = &syncrv1.ActivityGroup{Key: r.Kind}
			kindGroups[r.Kind] = k
		}
		addToGroup(k, r)
		id := r.WorkerID.String()
		w := workerGroups[id]
		if w == nil {
			w = &syncrv1.ActivityGroup{Key: id, Name: cmp.Or(names[r.WorkerID], id)}
			workerGroups[id] = w
		}
		addToGroup(w, r)
		if r.Status != domain.JobFailed && r.Status != domain.JobInterrupted {
			continue
		}
		code := link.ParseErrorCode(r.ErrorCode)
		c := byCode[code]
		if c == nil {
			c = &reason{msg: &syncrv1.FailureReason{Code: code}, kinds: map[string]int64{}}
			byCode[code] = c
		}
		if r.Status == domain.JobFailed {
			c.msg.Failed += r.Jobs
		} else {
			c.msg.Interrupted += r.Jobs
		}
		c.kinds[r.Kind] += r.Jobs
	}
	for _, c := range byCode {
		ks := make([]string, 0, len(c.kinds))
		for k := range c.kinds {
			ks = append(ks, k)
		}
		slices.SortFunc(ks, func(a, b string) int { return cmp.Or(cmp.Compare(c.kinds[b], c.kinds[a]), strings.Compare(a, b)) })
		c.msg.Kinds = ks
		reasons = append(reasons, c.msg)
	}
	slices.SortFunc(reasons, func(a, b *syncrv1.FailureReason) int {
		return cmp.Or(cmp.Compare(b.Failed+b.Interrupted, a.Failed+a.Interrupted), cmp.Compare(a.Code, b.Code))
	})
	if len(reasons) > dashboardFailureReasons {
		reasons = reasons[:dashboardFailureReasons]
	}
	return sortedGroups(kindGroups), sortedGroups(workerGroups), reasons
}

// scheduleHealth returns the caller's schedules with their newest run and
// last success: those whose newest run did not succeed (and is not still
// going) first, then by name.
func (h *dashboardHandler) scheduleHealth(ctx context.Context, actor domain.Actor) ([]*syncrv1.ScheduleHealth, error) {
	hs, err := h.schedules.Health(ctx, actor)
	if err != nil {
		return nil, err
	}
	type item struct {
		msg     *syncrv1.ScheduleHealth
		problem bool
	}
	items := make([]item, 0, len(hs))
	for _, s := range hs {
		msg := &syncrv1.ScheduleHealth{
			ScheduleId: s.Schedule.ID.String(), ScheduleName: s.Schedule.Name, Kind: s.Schedule.Kind,
			Enabled: s.Schedule.Enabled, LastSuccessAt: ts(s.LastSuccessAt), NextRunAt: ts(s.Schedule.NextRunAt),
		}
		problem := false
		if s.HasRun {
			msg.LatestRun = scheduleRunToProto(s.LatestRun)
			state, ok := lastRunState(s.LatestRun)
			problem = ok && state != runStateSucceeded && state != runStateActive
		}
		items = append(items, item{msg, problem})
	}
	slices.SortStableFunc(items, func(a, b item) int {
		switch {
		case a.problem == b.problem:
			return 0
		case a.problem:
			return -1
		default:
			return 1
		}
	})
	out := make([]*syncrv1.ScheduleHealth, len(items))
	for i, it := range items {
		out[i] = it.msg
	}
	return out, nil
}

// repositoryGrowth returns the caller's live repositories (by name) with
// what their successful backups that finished in the range added, bucketed
// like the activity. It reads at most dashboardBackups backup jobs, newest
// first, created from a day before the range (a backup may run long).
func (h *dashboardHandler) repositoryGrowth(ctx context.Context, actor domain.Actor, rng dashboardRange, buckets []jobs.ActivityBucket) ([]*syncrv1.RepositoryGrowth, error) {
	if h.restic == nil {
		return []*syncrv1.RepositoryGrowth{}, nil
	}
	repos, err := h.restic.List(ctx, actor, restic.ListFilter{})
	if err != nil {
		return nil, err
	}
	byID := make(map[domain.ResticRepoID]*syncrv1.RepositoryGrowth, len(repos))
	out := make([]*syncrv1.RepositoryGrowth, 0, len(repos))
	for _, r := range repos {
		if r.Deleted() {
			continue
		}
		g := &syncrv1.RepositoryGrowth{
			RepositoryId: r.ID.String(), Name: r.Name, SnapshotCount: int32(min(r.Stats.SnapshotCount, 1<<31-1)), //nolint:gosec // clamped
			LatestSnapshotAt: ts(r.Stats.LatestSnapshotAt), Added: make([]int64, len(buckets)),
		}
		byID[r.ID] = g
		out = append(out, g)
	}
	slices.SortFunc(out, func(a, b *syncrv1.RepositoryGrowth) int {
		return cmp.Or(strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), strings.Compare(a.RepositoryId, b.RepositoryId))
	})
	if len(out) == 0 {
		return out, nil
	}
	// The newest backup read for each repository so far: the list is newest
	// first by creation, which can differ from the order they finished in.
	lastAt := map[domain.ResticRepoID]time.Time{}
	firstAt := map[domain.ResticRepoID]time.Time{}
	var before domain.JobID
	truncated := false
	for read := 0; ; {
		if read >= dashboardBackups {
			truncated = true
			break
		}
		page, err := h.jobs.List(ctx, actor, jobs.ListFilter{
			Kinds: []string{restic.KindBackup}, Statuses: []domain.JobStatus{domain.JobSucceeded},
			CreatedAfter: rng.from.Add(-24 * time.Hour), Before: before, Limit: min(dashboardBackupPage, dashboardBackups-read),
		})
		if err != nil {
			return nil, err
		}
		read += len(page.Jobs)
		for _, j := range page.Jobs {
			g := byID[j.RepositoryID]
			if g == nil || j.FinishedAt.Before(rng.from) || !j.FinishedAt.Before(rng.until) {
				continue
			}
			b, err := backupOutput(j)
			if err != nil {
				h.errs.log.WarnContext(ctx, "dashboard: cannot read a backup's output", "job_id", j.ID.String(), "error", err.Error())
				continue
			}
			added := b.GetDataAddedPackedBytes()
			if added == 0 {
				added = b.GetDataAddedBytes() // restic before 0.17 did not report the packed size
			}
			g.Backups++
			g.AddedBytes += added
			if i := bucketIndex(buckets, j.FinishedAt); i >= 0 {
				g.Added[i] += added
			}
			if t := lastAt[j.RepositoryID]; t.IsZero() || j.FinishedAt.After(t) {
				lastAt[j.RepositoryID], g.LastSourceBytes = j.FinishedAt, b.GetTotalBytesProcessed()
			}
			if t := firstAt[j.RepositoryID]; t.IsZero() || j.FinishedAt.Before(t) {
				firstAt[j.RepositoryID], g.FirstSourceBytes = j.FinishedAt, b.GetTotalBytesProcessed()
			}
		}
		if page.Next.IsZero() {
			break
		}
		before = page.Next
	}
	for _, g := range out {
		g.Truncated = truncated
	}
	return out, nil
}

// backupOutput decodes the restic backup output of j.
func backupOutput(j jobs.Job) (*syncrv1.ResticBackupOutput, error) {
	var out syncrv1.JobOutput
	if err := proto.Unmarshal(j.Output, &out); err != nil {
		return nil, fmt.Errorf("decode output: %w", err)
	}
	return out.GetResticBackup(), nil
}

// bucketIndex returns the index of the bucket holding t (the last one that
// starts at or before it), or -1 before the first.
func bucketIndex(buckets []jobs.ActivityBucket, t time.Time) int {
	return sort.Search(len(buckets), func(i int) bool { return buckets[i].Start.After(t) }) - 1
}
