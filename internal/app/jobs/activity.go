package jobs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Limits of Activity's range.
const (
	// MaxActivityRange bounds Until - From.
	MaxActivityRange = 400 * 24 * time.Hour
	// MaxHourlyActivityRange bounds Until - From of hourly buckets.
	MaxHourlyActivityRange = 31 * 24 * time.Hour
)

// activitySampleStep is the spacing of the instants Activity truncates to
// enumerate buckets. Every bucket is at least 30 minutes long (an hour cut
// by a 30-minute DST shift, as on Lord Howe Island), so no bucket falls
// between two samples.
const activitySampleStep = 15 * time.Minute

// errNoActivity is returned when the service was built without
// Deps.Activity.
var errNoActivity = errors.New("job activity is not available (no activity repository)")

// Activity counts the finished jobs of f by bucket: every bucket that
// overlaps [f.From, f.Until), oldest first, empty ones included (the first
// may start before f.From; it counts only jobs from f.From on). They are the
// actor's jobs, or every user's for an admin with f.AllUsers (ignored for
// other actors, as in List). Buckets are hours or local days of
// f.Location, as Postgres's date_trunc(unit, finished_at, zone) computes
// them (see ActivityRepository.JobActivity). The range must be positive
// and at most MaxActivityRange (MaxHourlyActivityRange for hours).
func (s *Service) Activity(ctx context.Context, actor domain.Actor, f ActivityFilter) ([]ActivityBucket, error) {
	const op = "job activity"
	if s.activity == nil {
		return nil, fmt.Errorf("%s: %w", op, errNoActivity)
	}
	var v domain.Validation
	if !f.Unit.Valid() {
		v.Add("unit", `must be "hour" or "day"`)
	}
	if f.Location == nil || f.Location == time.Local {
		v.Add("timezone", "must be an IANA time zone")
	}
	span := f.Until.Sub(f.From)
	switch {
	case f.From.IsZero() || f.Until.IsZero() || span <= 0:
		v.Add("range", "must end after it starts")
	case span > MaxActivityRange:
		v.Add("range", fmt.Sprintf("must be at most %d days", MaxActivityRange/(24*time.Hour)))
	case f.Unit == BucketHour && span > MaxHourlyActivityRange:
		v.Add("range", fmt.Sprintf("must be at most %d days for hourly buckets", MaxHourlyActivityRange/(24*time.Hour)))
	}
	if err := v.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	buckets := activityBuckets(f.From, f.Until, f.Unit, f.Location)
	owner, ok := activityOwner(actor, f.AllUsers)
	if !ok {
		return buckets, nil // nobody owns no jobs
	}
	rows, err := s.activity.JobActivity(ctx, ActivityQuery{
		From: f.From, Until: f.Until, Unit: f.Unit, Location: f.Location, OwnerID: owner,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	for _, r := range rows {
		i, found := slices.BinarySearchFunc(buckets, r.Bucket, func(b ActivityBucket, t time.Time) int { return b.Start.Compare(t) })
		if !found {
			// The store and this package disagree about a bucket boundary
			// (e.g. different time zone data): keep the row rather than
			// lose it.
			buckets = slices.Insert(buckets, i, ActivityBucket{Start: r.Bucket.UTC()})
		}
		b := &buckets[i]
		switch r.Status {
		case domain.JobSucceeded:
			b.Succeeded += r.Jobs
		case domain.JobFailed:
			b.Failed += r.Jobs
		case domain.JobInterrupted:
			b.Interrupted += r.Jobs
		case domain.JobCancelled:
			b.Cancelled += r.Jobs
		default:
			continue // not a terminal status: not part of the activity
		}
		b.Bytes += r.Bytes
		b.Files += r.Files
	}
	return buckets, nil
}

// CountUnfinished counts the queued, dispatched and running jobs by
// status: the actor's, or every user's for an admin with allUsers.
// Statuses without jobs are omitted.
func (s *Service) CountUnfinished(ctx context.Context, actor domain.Actor, allUsers bool) (map[domain.JobStatus]int64, error) {
	const op = "count unfinished jobs"
	if s.activity == nil {
		return nil, fmt.Errorf("%s: %w", op, errNoActivity)
	}
	owner, ok := activityOwner(actor, allUsers)
	if !ok {
		return map[domain.JobStatus]int64{}, nil
	}
	counts, err := s.activity.CountUnfinished(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if counts == nil {
		counts = map[domain.JobStatus]int64{}
	}
	return counts, nil
}

// activityOwner returns the owner filter of the dashboard queries (zero:
// every owner), or false when the actor owns nothing (no user ID).
func activityOwner(actor domain.Actor, allUsers bool) (domain.UserID, bool) {
	if actor.IsAdmin() && allUsers {
		return domain.UserID{}, true
	}
	return actor.UserID, !actor.UserID.IsZero()
}

// activityBuckets returns the empty buckets overlapping [from, until), in
// order.
func activityBuckets(from, until time.Time, unit BucketUnit, loc *time.Location) []ActivityBucket {
	var out []ActivityBucket
	add := func(t time.Time) {
		b := truncateActivity(t, unit, loc)
		if n := len(out); n == 0 || out[n-1].Start.Before(b) {
			out = append(out, ActivityBucket{Start: b})
		}
	}
	for t := from; t.Before(until); t = t.Add(activitySampleStep) {
		add(t)
	}
	add(until.Add(-time.Nanosecond))
	return out
}

// truncateActivity returns the start of t's activity bucket, as Postgres 17
// computes date_trunc(unit, t, loc.String()), in UTC:
//
//   - hour: the local time cut to the hour keeping t's UTC offset, so both
//     passes of a repeated (fall-back) hour have their own bucket and an
//     offset like +05:30 buckets at half past the UTC hour;
//   - day: local midnight of t's local date, resolved again in loc: a
//     midnight that exists twice resolves to the later instant, one that
//     does not exist (America/Sao_Paulo 2018-11-04) with the offset before
//     the transition (which is the transition instant).
func truncateActivity(t time.Time, unit BucketUnit, loc *time.Location) time.Time {
	lt := t.In(loc)
	if unit == BucketHour {
		return lt.Add(-(time.Duration(lt.Minute())*time.Minute + time.Duration(lt.Second())*time.Second +
			time.Duration(lt.Nanosecond()))).UTC()
	}
	y, m, d := lt.Date()
	return localMidnight(y, m, d, loc).UTC()
}

// localMidnight returns the instant of midnight on y-m-d in loc with
// Postgres's rules for ambiguous (the later instant) and non-existent (the
// offset before the transition) local times.
func localMidnight(y int, m time.Month, d int, loc *time.Location) time.Time {
	naive := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	_, before := naive.Add(-36 * time.Hour).In(loc).Zone()
	_, after := naive.Add(36 * time.Hour).In(loc).Zone()
	var latest time.Time
	for _, off := range []int{before, after} {
		u := naive.Add(-time.Duration(off) * time.Second)
		lt := u.In(loc)
		if ly, lm, ld := lt.Date(); ly == y && lm == m && ld == d && lt.Hour() == 0 && lt.Minute() == 0 && u.After(latest) {
			latest = u
		}
	}
	if latest.IsZero() {
		return naive.Add(-time.Duration(before) * time.Second)
	}
	return latest
}
