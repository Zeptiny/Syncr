package jobkind

import (
	"strconv"
	"strings"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Bounds of a definition's time limit (JobDefinition.max_runtime). They
// equal the controller's (jobs.MinTimeLimit, jobs.MaxTimeLimit), which this
// package cannot import; the upper one is also rclone's max_duration bound.
const (
	MinMaxRuntime = time.Minute
	MaxMaxRuntime = 366 * 24 * time.Hour
)

// checkMaxRuntime checks def's max_runtime: unset, zero (which counts as
// unset, as for the dispatcher: jobs.EffectiveTimeLimit), or a valid
// duration between MinMaxRuntime and MaxMaxRuntime.
func checkMaxRuntime(def *syncrv1.JobDefinition) *domain.ValidationError {
	mr := def.GetMaxRuntime()
	if mr == nil || (mr.GetSeconds() == 0 && mr.GetNanos() == 0) {
		return nil
	}
	if d := mr.AsDuration(); mr.CheckValid() != nil || d < MinMaxRuntime || d > MaxMaxRuntime {
		return &domain.ValidationError{Fields: []domain.FieldError{{
			Field: "definition.max_runtime", Message: "must be between 1 minute and 366 days",
		}}}
	}
	return nil
}

// maxRuntime returns def's own time limit, if it sets a valid one.
func maxRuntime(def *syncrv1.JobDefinition) (time.Duration, bool) {
	mr := def.GetMaxRuntime()
	if mr == nil || mr.CheckValid() != nil || mr.AsDuration() <= 0 {
		return 0, false
	}
	return mr.AsDuration(), true
}

// formatDuration formats d for people in days, hours, minutes and seconds,
// leaving out the units that are zero: "6h", "1h30m", "2d", "1d12h",
// "45s". Fractions of a second are dropped; zero or less is "0s".
func formatDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	if d <= 0 {
		return "0s"
	}
	var b strings.Builder
	for _, u := range []struct {
		size time.Duration
		unit string
	}{{24 * time.Hour, "d"}, {time.Hour, "h"}, {time.Minute, "m"}, {time.Second, "s"}} {
		if n := d / u.size; n > 0 {
			b.WriteString(strconv.FormatInt(int64(n), 10))
			b.WriteString(u.unit)
			d -= n * u.size
		}
	}
	return b.String()
}
