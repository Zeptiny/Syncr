package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Zeptiny/Syncr/internal/adapters/postgres/db"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// AppendJobSample implements jobs.SampleRepository: INSERT ... ON CONFLICT
// (job_id, t) DO NOTHING, so a repeated sample is a no-op. A missing job
// violates the foreign key and reports ErrNotFound.
func (s *Store) AppendJobSample(ctx context.Context, smp jobs.Sample) error {
	err := s.q.AppendJobSample(ctx, db.AppendJobSampleParams{
		JobID:      pgID(smp.JobID),
		T:          pgTime(smp.Time),
		Bytes:      smp.Stats.Bytes,
		TotalBytes: smp.Stats.TotalBytes,
		Files:      smp.Stats.Files,
		TotalFiles: smp.Stats.TotalFiles,
		Checks:     smp.Stats.Checks,
		Deletes:    smp.Stats.Deletes,
		Renames:    smp.Stats.Renames,
		Errors:     smp.Stats.Errors,
		ElapsedNs:  int64(smp.Stats.Elapsed),
	})
	return mapErr(fmt.Sprintf("append sample of job %s", smp.JobID), err)
}

// LatestJobSample implements jobs.SampleRepository.
func (s *Store) LatestJobSample(ctx context.Context, id domain.JobID) (jobs.Sample, bool, error) {
	row, err := s.q.LatestJobSample(ctx, pgID(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return jobs.Sample{}, false, nil
	}
	if err != nil {
		return jobs.Sample{}, false, mapErr(fmt.Sprintf("latest sample of job %s", id), err)
	}
	smp, err := sampleFrom(row)
	return smp, err == nil, err
}

// LatestJobSamples implements jobs.SampleRepository: one query, a primary
// key probe per job.
func (s *Store) LatestJobSamples(ctx context.Context, ids []domain.JobID) (map[domain.JobID]jobs.Sample, error) {
	out := make(map[domain.JobID]jobs.Sample, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	pgIDs := make([]pgtype.UUID, len(ids))
	for i, id := range ids {
		pgIDs[i] = pgID(id)
	}
	rows, err := s.q.LatestJobSamples(ctx, pgIDs)
	if err != nil {
		return nil, mapErr(fmt.Sprintf("latest samples of %d jobs", len(ids)), err)
	}
	for _, r := range rows {
		smp, err := sampleFrom(r)
		if err != nil {
			return nil, err
		}
		out[smp.JobID] = smp
	}
	return out, nil
}

// ListJobSamples implements jobs.SampleRepository: one statement returns
// the thinned samples with the job's sample count (see the query).
func (s *Store) ListJobSamples(ctx context.Context, id domain.JobID, maxPoints int) ([]jobs.Sample, int, error) {
	rows, err := s.q.ListJobSamples(ctx, db.ListJobSamplesParams{JobID: pgID(id), MaxPoints: int64(maxPoints)})
	if err != nil {
		return nil, 0, mapErr(fmt.Sprintf("list samples of job %s", id), err)
	}
	out := make([]jobs.Sample, len(rows))
	total := 0
	for i, r := range rows {
		out[i], err = sampleFrom(db.JobSample{
			JobID: r.JobID, T: r.T, Bytes: r.Bytes, TotalBytes: r.TotalBytes, Files: r.Files, TotalFiles: r.TotalFiles,
			Checks: r.Checks, Deletes: r.Deletes, Renames: r.Renames, Errors: r.Errors, ElapsedNs: r.ElapsedNs,
		})
		if err != nil {
			return nil, 0, err
		}
		total = int(r.Total)
	}
	return out, total, nil
}

// PurgeJobSamples implements jobs.SampleRepository: one DELETE over the
// oldest finished jobs that still have samples (see the query).
func (s *Store) PurgeJobSamples(ctx context.Context, finishedBefore time.Time, maxJobs int) (int, error) {
	if maxJobs <= 0 {
		return 0, fmt.Errorf("purge job samples: %w: max jobs %d is not positive", domain.ErrInvalidArgument, maxJobs)
	}
	n, err := s.q.PurgeJobSamples(ctx, db.PurgeJobSamplesParams{FinishedBefore: pgTime(finishedBefore), MaxJobs: int64(maxJobs)})
	if err != nil {
		return 0, mapErr("purge job samples", err)
	}
	return int(n), nil
}
