// Package cronexpr parses cron expressions with a time zone and computes
// their occurrences, for schedules (internal/app/schedules).
//
// # Syntax
//
// An expression has five fields, minute hour day-of-month month
// day-of-week, with the usual lists, ranges, steps and month and weekday
// names, or is one of the descriptors @yearly, @annually, @monthly,
// @weekly, @daily, @midnight and @hourly. It is case-insensitive. Seconds,
// @every and "TZ="/"CRON_TZ=" prefixes are rejected: the time zone is a
// separate IANA name ("Europe/Berlin", "UTC"; never "" or "Local", which
// would depend on the controller host). An expression that has no
// occurrence within five years (e.g. "0 0 31 2 *") is rejected too.
// Parsing uses github.com/robfig/cron/v3 (its parser only); errors are
// *domain.ValidationError with the fields "cron" and "timezone".
//
// # Occurrences and daylight saving time
//
//  1. Occurrences are local wall times (year, month, day, hour, minute in
//     the expression's zone) that match every field. When both day fields
//     are restricted, a day matches either (cron's OR rule); when one is
//     "*", only the other counts.
//  2. A wall time that does not exist (the spring-forward gap) runs at the
//     transition instant: Europe/Berlin 02:30 on the last Sunday of March
//     runs at 03:00 CEST. Several occurrences inside one gap collapse into
//     one run. A nightly backup is never silently skipped.
//  3. A wall time that exists twice (fall-back) runs once, at its first,
//     earlier instant; so "*/15" runs only during the first pass of the
//     repeated hour.
//
// Provisional: in the contracts step Next, NextN and Latest delegate to
// robfig's own iteration, which does not implement rules 2 and 3 exactly
// (it can skip gap times and run twice in the repeated hour). The schedules
// track replaces them with a field-wise iteration over days, hours and
// minutes that resolves each wall time as described above.
//
// Allowed dependencies: the standard library, internal/domain (for its
// validation error) and github.com/robfig/cron/v3.
package cronexpr
