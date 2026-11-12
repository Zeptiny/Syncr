// Package cronexpr parses cron expressions with a time zone and computes
// their occurrences, for schedules (internal/app/schedules).
//
// # Syntax
//
// An expression has five fields, minute hour day-of-month month
// day-of-week, with the usual lists, ranges, steps and month and weekday
// names (Sunday is 0 or 7, as in crontab(5)), or is one of the descriptors
// @yearly, @annually, @monthly, @weekly, @daily, @midnight and @hourly. It
// is case-insensitive. Seconds, @every and "TZ="/"CRON_TZ=" prefixes are
// rejected: the time zone is a separate IANA name ("Europe/Berlin", "UTC";
// never "" or "Local", which would depend on the controller host). An
// expression that has no
// occurrence within five years (e.g. "0 0 31 2 *") is rejected too.
// Parsing uses github.com/robfig/cron/v3 (its parser only); errors are
// *domain.ValidationError with the fields "cron" and "timezone".
//
// # Occurrences and daylight saving time
//
//  1. Occurrences are local wall times (year, month, day, hour, minute in
//     the expression's zone) that match every field. When both day fields
//     are restricted, a day matches either (cron's OR rule); when one
//     starts with "*" or "?", steps included ("*/2"), a day must match both,
//     as in Vixie cron and cronie: "0 0 */2 * 1" runs on odd days that are
//     Mondays.
//  2. A wall time that does not exist (the spring-forward gap) runs at the
//     transition instant: Europe/Berlin 02:30 on the last Sunday of March
//     runs at 03:00 CEST. Several occurrences inside one gap collapse into
//     one run. A nightly backup is never silently skipped.
//  3. A wall time that exists twice (fall-back) runs once, at its first,
//     earlier instant; so "*/15" runs only during the first pass of the
//     repeated hour.
//
// Each wall time therefore runs at most once, and occurrences strictly
// increase. Rules 2 and 3 are one rule: a wall time runs at the first
// instant at which the zone's clock shows it or a later time. The
// computation (next.go) iterates the calendar field-wise (months, days,
// hours, minutes, so a yearly expression costs a few hundred steps) and
// resolves each matching wall time against the zone's periods
// (time.Time.ZoneBounds), which is exact for any transition, including the
// 30-minute shifts of Australia/Lord_Howe and the day Pacific/Apia skipped
// in 2011. robfig's own iteration is not used: it steps instants by hours,
// so it can skip gap times and run twice in a repeated hour.
//
// Next looks five years ahead: "0 0 29 2 *" has no occurrence then from
// 2097-03-01 on (2100 is not a leap year), and the schedule stops.
//
// Allowed dependencies: the standard library, internal/domain (for its
// validation error) and github.com/robfig/cron/v3.
package cronexpr
