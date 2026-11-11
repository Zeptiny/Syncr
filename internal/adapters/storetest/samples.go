package storetest

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// appendSamples stores n samples of job id, newest first (so the store must
// order them itself): the one of rank r (by time) is at t0()+r seconds and
// counts r bytes.
func appendSamples(t *testing.T, s Store, id domain.JobID, n int) {
	t.Helper()
	for r := n - 1; r >= 0; r-- {
		smp := jobs.Sample{JobID: id, Time: t0().Add(time.Duration(r) * time.Second), Stats: jobs.Stats{Bytes: int64(r), TotalBytes: 1 << 40}}
		if err := s.AppendJobSample(context.Background(), smp); err != nil {
			t.Fatalf("AppendJobSample: %v", err)
		}
	}
}

// thinnedRanks are the ranks ListJobSamples keeps of n samples for
// maxPoints, stated as a closed form instead of the port's rule (they are
// equal): every rank for maxPoints <= 0 or n <= maxPoints, the first for
// maxPoints 1, else ceil(i*(n-1)/(maxPoints-1)) for i in [0, maxPoints).
func thinnedRanks(n, maxPoints int) []int {
	var out []int
	switch {
	case maxPoints <= 0 || n <= maxPoints:
		for r := range n {
			out = append(out, r)
		}
	case maxPoints == 1:
		out = []int{0}
	default:
		for i := range maxPoints {
			out = append(out, (i*(n-1)+maxPoints-2)/(maxPoints-1))
		}
	}
	return out
}

// testSamplesThinned: ListJobSamples returns at most maxPoints samples,
// evenly thinned by the port's exact rule (the first and the last always
// kept), by time; the total counts every sample of the job and only its
// own; maxPoints <= 0 returns everything.
func testSamplesThinned(t *testing.T, s Store) {
	ctx := context.Background()
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	sizes := []int{0, 1, 2, 5, 10, 37}
	ids := make(map[int]domain.JobID, len(sizes))
	for i, n := range sizes {
		j := newJob(t, i+1, w.ID)
		mustCreateJob(t, s, j)
		appendSamples(t, s, j.ID, n)
		ids[n] = j.ID
	}

	// A few cases pinned by hand, so a wrong closed form cannot hide a
	// wrong rule.
	for _, tt := range []struct{ n, maxPoints int }{{10, 4}, {10, 3}, {5, 4}, {37, 2}} {
		want := map[[2]int][]int{{10, 4}: {0, 3, 6, 9}, {10, 3}: {0, 5, 9}, {5, 4}: {0, 2, 3, 4}, {37, 2}: {0, 36}}[[2]int{tt.n, tt.maxPoints}]
		if got := thinnedRanks(tt.n, tt.maxPoints); !slices.Equal(got, want) {
			t.Fatalf("thinnedRanks(%d, %d) = %v, want %v", tt.n, tt.maxPoints, got, want)
		}
	}

	for _, n := range sizes {
		for _, maxPoints := range []int{-1, 0, 1, 2, 3, 4, 5, 9, 10, 11, 36, 37, 50} {
			name := fmt.Sprintf("%d samples, max %d", n, maxPoints)
			list, total, err := s.ListJobSamples(ctx, ids[n], maxPoints)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if total != n {
				t.Errorf("%s: total = %d, want %d", name, total, n)
			}
			var got []int
			for _, smp := range list {
				if smp.JobID != ids[n] || !smp.Time.Equal(t0().Add(time.Duration(smp.Stats.Bytes)*time.Second)) || smp.Stats.TotalBytes != 1<<40 {
					t.Errorf("%s: sample %+v is not one of the job's", name, smp)
				}
				got = append(got, int(smp.Stats.Bytes))
			}
			if want := thinnedRanks(n, maxPoints); !slices.Equal(got, want) {
				t.Errorf("%s: ranks = %v, want %v", name, got, want)
			}
			if maxPoints > 0 && len(list) > maxPoints {
				t.Errorf("%s: %d samples, more than asked for", name, len(list))
			}
		}
	}

	// The returned slice is the caller's.
	list, _, _ := s.ListJobSamples(ctx, ids[10], 3)
	list[0].Stats.Bytes = 99
	if again, _, _ := s.ListJobSamples(ctx, ids[10], 3); again[0].Stats.Bytes != 0 {
		t.Error("store aliases a ListJobSamples result")
	}
}

// testSamplesPurge: PurgeJobSamples deletes the samples of at most maxJobs
// jobs that finished before the cutoff and still have samples, those that
// finished first first, and returns how many samples it deleted; it never
// touches unfinished jobs, jobs that finished at or after the cutoff, or the
// jobs themselves.
func testSamplesPurge(t *testing.T, s Store) {
	ctx := context.Background()
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	cutoff := t0().Add(-24 * time.Hour)
	type spec struct {
		finished time.Time // zero: still running
		samples  int
	}
	specs := []spec{
		{cutoff.Add(-26 * time.Hour), 0}, // 1: the oldest, without samples
		{cutoff.Add(-24 * time.Hour), 3}, // 2
		{cutoff.Add(-23 * time.Hour), 2}, // 3
		{cutoff.Add(-6 * time.Hour), 4},  // 4
		{cutoff.Add(23 * time.Hour), 2},  // 5: recent
		{cutoff, 1},                      // 6: exactly at the cutoff
		{time.Time{}, 2},                 // 7: running for days
	}
	ids := make([]domain.JobID, len(specs))
	for i, sp := range specs {
		j := newJob(t, i+1, w.ID)
		j.CreatedAt, j.QueuedAt = cutoff.Add(-100*time.Hour), cutoff.Add(-100*time.Hour)
		j.StartedAt = cutoff.Add(-99 * time.Hour)
		j.Status = domain.JobRunning
		if !sp.finished.IsZero() {
			j.Status, j.FinishedAt, j.Stats = domain.JobSucceeded, sp.finished, jobs.Stats{Bytes: int64(1000 + i)}
		}
		mustCreateJob(t, s, j)
		appendSamples(t, s, j.ID, sp.samples)
		ids[i] = j.ID
	}

	for _, bad := range []int{0, -1} {
		_, err := s.PurgeJobSamples(ctx, cutoff, bad)
		wantErr(t, fmt.Sprintf("PurgeJobSamples(max jobs %d)", bad), err, domain.ErrInvalidArgument)
	}
	// One job per call: job 1 has no samples to delete, so job 2 goes
	// first; then jobs 3 and 4 in one call; then nothing is left.
	for i, tt := range []struct{ maxJobs, want int }{{1, 3}, {5, 6}, {5, 0}} {
		n, err := s.PurgeJobSamples(ctx, cutoff, tt.maxJobs)
		if err != nil || n != tt.want {
			t.Fatalf("purge %d (max jobs %d) = (%d, %v), want %d samples", i+1, tt.maxJobs, n, err, tt.want)
		}
	}
	for i, want := range []int{0, 0, 0, 0, 2, 1, 2} {
		if _, total, err := s.ListJobSamples(ctx, ids[i], 0); err != nil || total != want {
			t.Errorf("job %d has %d samples (%v) after the purge, want %d", i+1, total, err, want)
		}
	}
	// The jobs stay, with their final counters.
	for i := range 4 {
		j, err := s.GetJob(ctx, ids[i])
		if err != nil || j.Stats.Bytes != int64(1000+i) || !j.FinishedAt.Equal(specs[i].finished) {
			t.Errorf("job %d after the purge = (%+v, %v)", i+1, j, err)
		}
	}
}
