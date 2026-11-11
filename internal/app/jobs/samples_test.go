package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// TestListSamplesThinsAndCounts: ListSamples thins to the requested number
// of points (0 is the default, more than the maximum is capped), always
// keeps the first and the last sample and reports the job's total; Samples
// is ListSamples with the default; a negative request is invalid and
// another user's job is not found.
func TestListSamplesThinsAndCounts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	j := e.at(t, domain.JobRunning)
	const stored = jobs.MaxSamplePoints + 3
	start := e.clock.Now()
	for i := range stored {
		// Directly into the store: RecordProgress keeps one per interval.
		smp := jobs.Sample{JobID: j.ID, Time: start.Add(time.Duration(i) * time.Second), Stats: jobs.Stats{Bytes: int64(i)}}
		if err := e.store.AppendJobSample(ctx, smp); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct{ ask, want int }{
		{0, jobs.DefaultSamplePoints},
		{10, 10},
		{1, 1},
		{jobs.MaxSamplePoints + 1000, jobs.MaxSamplePoints},
	} {
		got, total, err := e.svc.ListSamples(ctx, e.owner, j.ID, tt.ask)
		if err != nil || len(got) != tt.want || total != stored {
			t.Fatalf("ListSamples(%d) = (%d samples, total %d, %v), want %d of %d", tt.ask, len(got), total, err, tt.want, stored)
		}
		if got[0].Stats.Bytes != 0 || (tt.want > 1 && got[len(got)-1].Stats.Bytes != stored-1) {
			t.Errorf("ListSamples(%d) runs from %d to %d, want the first and the last", tt.ask, got[0].Stats.Bytes, got[len(got)-1].Stats.Bytes)
		}
		for i := 1; i < len(got); i++ {
			if !got[i].Time.After(got[i-1].Time) {
				t.Fatalf("ListSamples(%d): samples %d and %d are out of order", tt.ask, i-1, i)
			}
		}
	}
	if def, err := e.svc.Samples(ctx, e.owner, j.ID); err != nil || len(def) != jobs.DefaultSamplePoints {
		t.Errorf("Samples = (%d samples, %v), want the default %d", len(def), err, jobs.DefaultSamplePoints)
	}
	if _, _, err := e.svc.ListSamples(ctx, e.owner, j.ID, -1); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("ListSamples(-1) err = %v, want ErrInvalidArgument", err)
	}
	other := e.addUser(t, domain.RoleUser)
	if _, _, err := e.svc.ListSamples(ctx, other, j.ID, 0); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("another user's ListSamples err = %v, want ErrNotFound", err)
	}
}
