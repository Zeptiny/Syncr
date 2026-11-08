package memstore

import (
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
)

// TestTruncateMatchesPostgres pins truncate to what Postgres 17 returns for
// date_trunc(unit, ts, zone) (values taken from a postgres:17-alpine
// server); storetest checks the common cases against both stores.
func TestTruncateMatchesPostgres(t *testing.T) {
	t.Parallel()
	utc := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	tests := []struct {
		zone string
		unit jobs.BucketUnit
		in   string
		want string
	}{
		{"Europe/Berlin", jobs.BucketHour, "2026-10-25T00:30:00Z", "2026-10-25T00:00:00Z"}, // first pass of 02:xx
		{"Europe/Berlin", jobs.BucketHour, "2026-10-25T01:30:00Z", "2026-10-25T01:00:00Z"}, // second pass
		{"Europe/Berlin", jobs.BucketHour, "2026-03-29T01:30:00Z", "2026-03-29T01:00:00Z"},
		{"America/New_York", jobs.BucketHour, "2026-11-01T05:30:00Z", "2026-11-01T05:00:00Z"},
		{"America/New_York", jobs.BucketHour, "2026-11-01T06:30:00Z", "2026-11-01T06:00:00Z"},
		{"America/Sao_Paulo", jobs.BucketDay, "2018-11-04T15:00:00Z", "2018-11-04T03:00:00Z"},
		{"Australia/Lord_Howe", jobs.BucketHour, "2026-10-03T15:45:00Z", "2026-10-03T15:00:00Z"},
		{"Asia/Kolkata", jobs.BucketHour, "2026-11-04T04:45:00Z", "2026-11-04T04:30:00Z"},
		{"Asia/Kolkata", jobs.BucketDay, "2026-11-04T20:00:00Z", "2026-11-04T18:30:00Z"},
		{"Europe/Berlin", jobs.BucketDay, "2026-10-25T12:00:00Z", "2026-10-24T22:00:00Z"},
		{"Pacific/Apia", jobs.BucketDay, "2011-12-31T12:00:00Z", "2011-12-31T10:00:00Z"},
	}
	for _, tt := range tests {
		loc, err := time.LoadLocation(tt.zone)
		if err != nil {
			t.Fatal(err)
		}
		if got := truncate(utc(tt.in), tt.unit, loc); !got.Equal(utc(tt.want)) {
			t.Errorf("date_trunc(%s, %s, %s) = %s, want %s", tt.unit, tt.in, tt.zone, got.UTC().Format(time.RFC3339), tt.want)
		}
	}
}
