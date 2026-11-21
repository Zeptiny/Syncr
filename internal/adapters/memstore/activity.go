package memstore

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// This file implements jobs.ActivityRepository: the aggregations of
// queries/activity.sql in Go (date_trunc(unit, finished_at, tz) bucket for
// bucket the same as Postgres 17), with run times and queue waits in whole
// microseconds like Postgres's timestamps.

// JobActivity implements jobs.ActivityRepository.
func (s *Store) JobActivity(_ context.Context, q jobs.ActivityQuery) ([]jobs.ActivityRow, error) {
	if !q.Unit.Valid() || q.Location == nil {
		return nil, fmt.Errorf("job activity: %w: unit %q or time zone missing", domain.ErrInvalidArgument, q.Unit)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	type key struct {
		bucket int64 // Unix nanoseconds
		status domain.JobStatus
	}
	groups := map[key]*jobs.ActivityRow{}
	for _, j := range s.jobs {
		if j.FinishedAt.IsZero() || j.FinishedAt.Before(q.From) || !j.FinishedAt.Before(q.Until) ||
			(!q.OwnerID.IsZero() && j.OwnerID != q.OwnerID) {
			continue
		}
		b := truncate(j.FinishedAt, q.Unit, q.Location)
		k := key{b.UnixNano(), j.Status}
		g := groups[k]
		if g == nil {
			g = &jobs.ActivityRow{Bucket: b, Status: j.Status}
			groups[k] = g
		}
		g.Add(sumsOf(j))
	}
	out := make([]jobs.ActivityRow, 0, len(groups))
	for _, g := range groups {
		out = append(out, *g)
	}
	slices.SortFunc(out, func(a, b jobs.ActivityRow) int {
		return cmp.Or(a.Bucket.Compare(b.Bucket), cmp.Compare(a.Status, b.Status))
	})
	return out, nil
}

// truncate is Postgres's date_trunc(unit, t, loc):
//
//   - hour: the local time is cut to the hour keeping t's UTC offset, so
//     both passes of a repeated (fall-back) hour get their own bucket and an
//     offset like +05:30 buckets at half past the UTC hour;
//   - day: local midnight of t's local date, resolved again in loc: a
//     midnight that exists twice resolves to the later instant, one that
//     does not exist (America/Sao_Paulo 2018-11-04) with the offset before
//     the transition (which is the transition instant).
func truncate(t time.Time, unit jobs.BucketUnit, loc *time.Location) time.Time {
	lt := t.In(loc)
	if unit == jobs.BucketHour {
		return lt.Add(-(time.Duration(lt.Minute())*time.Minute + time.Duration(lt.Second())*time.Second +
			time.Duration(lt.Nanosecond()))).In(time.UTC)
	}
	y, m, d := lt.Date()
	return resolveLocal(y, m, d, loc).In(time.UTC)
}

// resolveLocal returns the instant of local midnight on y-m-d in loc with
// Postgres's rules for ambiguous (later instant) and non-existent (offset
// before the transition) local times.
func resolveLocal(y int, m time.Month, d int, loc *time.Location) time.Time {
	naive := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	_, before := naive.Add(-36 * time.Hour).In(loc).Zone()
	_, after := naive.Add(36 * time.Hour).In(loc).Zone()
	var valid []time.Time
	for _, off := range []int{before, after} {
		u := naive.Add(-time.Duration(off) * time.Second)
		if lt := u.In(loc); lt.Year() == y && lt.Month() == m && lt.Day() == d && lt.Hour() == 0 && lt.Minute() == 0 &&
			!slices.ContainsFunc(valid, u.Equal) {
			valid = append(valid, u)
		}
	}
	switch len(valid) {
	case 1:
		return valid[0]
	case 2:
		if valid[1].After(valid[0]) {
			return valid[1]
		}
		return valid[0]
	default:
		return naive.Add(-time.Duration(before) * time.Second)
	}
}

// sumsOf returns the Sums of the one finished job j.
func sumsOf(j jobs.Job) jobs.Sums {
	out := jobs.Sums{Jobs: 1, Bytes: j.Stats.Bytes, Files: j.Stats.Files}
	if run, ok := runTime(j); ok {
		out.Started, out.RunTime = 1, run
	}
	if wait, ok := span(j.CreatedAt, j.DispatchedAt); ok {
		out.Dispatched, out.QueueWait = 1, wait
	}
	return out
}

// runTime returns j's run time (FinishedAt - StartedAt), if it has one.
func runTime(j jobs.Job) (time.Duration, bool) { return span(j.StartedAt, j.FinishedAt) }

// span returns to - from with both cut to whole microseconds, as Postgres
// stores them, if both are set and to is not before from.
func span(from, to time.Time) (time.Duration, bool) {
	if from.IsZero() || to.IsZero() {
		return 0, false
	}
	d := to.Truncate(time.Microsecond).Sub(from.Truncate(time.Microsecond))
	return d, d >= 0
}

// JobTotals implements jobs.ActivityRepository.
func (s *Store) JobTotals(_ context.Context, q jobs.TotalsQuery) ([]jobs.TotalsRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	type key struct {
		kind   string
		worker domain.WorkerID
		status domain.JobStatus
		code   string
		bucket int
	}
	groups := map[key]*jobs.TotalsRow{}
	for _, j := range s.jobs {
		if j.FinishedAt.IsZero() || !q.From.IsZero() && j.FinishedAt.Before(q.From) ||
			!q.Until.IsZero() && !j.FinishedAt.Before(q.Until) || !q.OwnerID.IsZero() && j.OwnerID != q.OwnerID {
			continue
		}
		bucket := -1
		if run, ok := runTime(j); ok {
			bucket = 0
			for _, b := range q.RunTimeBounds {
				if b < run {
					bucket++
				}
			}
		}
		k := key{j.Kind, j.WorkerID, j.Status, j.Error.Code, bucket}
		g := groups[k]
		if g == nil {
			g = &jobs.TotalsRow{Kind: k.kind, WorkerID: k.worker, Status: k.status, ErrorCode: k.code, RunTimeBucket: bucket}
			groups[k] = g
		}
		g.Add(sumsOf(j))
	}
	out := make([]jobs.TotalsRow, 0, len(groups))
	for _, g := range groups {
		out = append(out, *g)
	}
	return out, nil
}

// UnfinishedJobs implements jobs.ActivityRepository.
func (s *Store) UnfinishedJobs(_ context.Context, owner domain.UserID) ([]jobs.UnfinishedRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	type key struct {
		status domain.JobStatus
		kind   string
		worker domain.WorkerID
	}
	groups := map[key]*jobs.UnfinishedRow{}
	for _, j := range s.jobs {
		if j.Status.Terminal() || !owner.IsZero() && j.OwnerID != owner {
			continue
		}
		k := key{j.Status, j.Kind, j.WorkerID}
		g := groups[k]
		if g == nil {
			g = &jobs.UnfinishedRow{Status: j.Status, Kind: j.Kind, WorkerID: j.WorkerID, OldestQueuedAt: j.QueuedAt}
			groups[k] = g
		}
		g.Jobs++
		if j.QueuedAt.Before(g.OldestQueuedAt) {
			g.OldestQueuedAt = j.QueuedAt
		}
	}
	out := make([]jobs.UnfinishedRow, 0, len(groups))
	for _, g := range groups {
		out = append(out, *g)
	}
	return out, nil
}
