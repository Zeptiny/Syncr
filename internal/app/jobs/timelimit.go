package jobs

import "time"

// Bounds of a job's time limit: a definition's max_runtime and the
// controller's default (jobs.default_max_runtime) are each zero (none) or
// between them.
const (
	MinTimeLimit = time.Minute
	MaxTimeLimit = 366 * 24 * time.Hour
)

// EffectiveTimeLimit is the time limit of a job (zero: none): maxRuntime,
// its definition's max_runtime, when it is positive; otherwise, unless the
// job's kind is one that only the system starts (systemOnly, e.g.
// restic_delete_data, jobkind SystemOnly), defaultLimit, the controller's
// jobs.default_max_runtime (zero or negative: none).
//
// The dispatcher sends this limit to the worker (dispatch.Specs.Build) and
// the API shows it as Job.time_limit, so both must call this function (or
// dispatch.Specs.TimeLimit, which does) rather than restate the rule.
func EffectiveTimeLimit(maxRuntime, defaultLimit time.Duration, systemOnly bool) time.Duration {
	switch {
	case maxRuntime > 0:
		return maxRuntime
	case systemOnly || defaultLimit < 0:
		return 0
	}
	return defaultLimit
}
