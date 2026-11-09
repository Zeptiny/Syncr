package postgres

import (
	"context"
	"errors"
	"fmt"

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

// ListJobSamples implements jobs.SampleRepository.
func (s *Store) ListJobSamples(ctx context.Context, id domain.JobID) ([]jobs.Sample, error) {
	rows, err := s.q.ListJobSamples(ctx, pgID(id))
	if err != nil {
		return nil, mapErr(fmt.Sprintf("list samples of job %s", id), err)
	}
	out := make([]jobs.Sample, len(rows))
	for i, r := range rows {
		if out[i], err = sampleFrom(r); err != nil {
			return nil, err
		}
	}
	return out, nil
}
