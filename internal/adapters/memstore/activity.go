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

// This file implements jobs.ActivityRepository: the aggregation of
// queries/activity.sql (date_trunc(unit, finished_at, tz) grouped by bucket
// and status) in Go, bucket for bucket the same as Postgres 17.

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
		g.Jobs++
		g.Bytes += j.Stats.Bytes
		g.Files += j.Stats.Files
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

// CountUnfinished implements jobs.ActivityRepository.
func (s *Store) CountUnfinished(_ context.Context, owner domain.UserID) (map[domain.JobStatus]int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[domain.JobStatus]int64{}
	for _, j := range s.jobs {
		if !j.Status.Terminal() && (owner.IsZero() || j.OwnerID == owner) {
			out[j.Status]++
		}
	}
	return out, nil
}
