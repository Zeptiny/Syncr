package postgres

import (
	"encoding"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Zeptiny/Syncr/internal/adapters/postgres/db"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// IDs. A required ID is always a valid UUID value (the nil UUID for the zero
// ID, which no row has); an optional one is NULL for the zero ID.

func pgID[K any](id domain.ID[K]) pgtype.UUID {
	return pgtype.UUID{Bytes: id.UUID(), Valid: true}
}

func pgOptID[K any](id domain.ID[K]) pgtype.UUID {
	return pgtype.UUID{Bytes: id.UUID(), Valid: !id.IsZero()}
}

// decodeID fills dst (a *domain.ID of any kind) from u; a NULL leaves it
// zero.
func decodeID(dst encoding.TextUnmarshaler, u pgtype.UUID) error {
	if !u.Valid {
		return nil
	}
	if err := dst.UnmarshalText([]byte(uuid.UUID(u.Bytes).String())); err != nil {
		return fmt.Errorf("decode id: %w", err)
	}
	return nil
}

// Times. Postgres keeps microseconds and the system clock already truncates
// to them; truncating on write makes that explicit. The zero time is stored
// as NULL in nullable columns and as 0001-01-01 in required ones; both come
// back as the zero time.

func pgTime(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC().Truncate(time.Microsecond), Valid: true}
}

func pgOptTime(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgTime(t)
}

func timeFrom(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time.UTC()
}

// textArray returns s as a non-nil slice: a nil slice would be encoded as
// NULL, but the array columns are NOT NULL and empty means "none".
func textArray(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// cleanText makes s storable in a text column: Postgres rejects NUL bytes
// and invalid UTF-8, which tool output can contain.
func cleanText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	return strings.ReplaceAll(s, "\x00", "�")
}

// nilIfEmpty maps the stored empty array back to the Go zero value.
func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

func cleanTexts(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = cleanText(s)
	}
	return out
}

// infoDoc is the stored form of workers.Info (workers.info jsonb). It is
// decoupled from the domain struct so renaming a Go field cannot silently
// change what is in the database; add fields only with omitempty so old
// rows still decode.
type infoDoc struct {
	Version           string    `json:"version,omitempty"`
	OS                string    `json:"os,omitempty"`
	Arch              string    `json:"arch,omitempty"`
	Hostname          string    `json:"hostname,omitempty"`
	Tools             []toolDoc `json:"tools,omitempty"`
	JobKinds          []string  `json:"job_kinds,omitempty"`
	MaxConcurrentJobs int       `json:"max_concurrent_jobs,omitempty"`
	LocalRoots        []string  `json:"local_roots,omitempty"`
	ReportedAt        time.Time `json:"reported_at,omitzero"`
}

type toolDoc struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func encodeInfo(in workers.Info) ([]byte, error) {
	doc := infoDoc{
		Version:           cleanText(in.Version),
		OS:                cleanText(in.OS),
		Arch:              cleanText(in.Arch),
		Hostname:          cleanText(in.Hostname),
		JobKinds:          cleanTexts(in.JobKinds),
		MaxConcurrentJobs: in.MaxConcurrentJobs,
		LocalRoots:        cleanTexts(in.LocalRoots),
		ReportedAt:        in.ReportedAt.UTC(),
	}
	for _, t := range in.Tools {
		doc.Tools = append(doc.Tools, toolDoc{Name: cleanText(t.Name), Version: cleanText(t.Version)})
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode worker info: %w", err)
	}
	return b, nil
}

func decodeInfo(b []byte) (workers.Info, error) {
	var doc infoDoc
	if len(b) > 0 {
		if err := json.Unmarshal(b, &doc); err != nil {
			return workers.Info{}, fmt.Errorf("decode worker info: %w", err)
		}
	}
	out := workers.Info{
		Version:           doc.Version,
		OS:                doc.OS,
		Arch:              doc.Arch,
		Hostname:          doc.Hostname,
		JobKinds:          doc.JobKinds,
		MaxConcurrentJobs: doc.MaxConcurrentJobs,
		LocalRoots:        doc.LocalRoots,
		ReportedAt:        doc.ReportedAt.UTC(),
	}
	for _, t := range doc.Tools {
		out.Tools = append(out.Tools, workers.Tool{Name: t.Name, Version: t.Version})
	}
	return out, nil
}

func createWorkerParams(w workers.Worker) (db.CreateWorkerParams, error) {
	info, err := encodeInfo(w.Info)
	if err != nil {
		return db.CreateWorkerParams{}, err
	}
	return db.CreateWorkerParams{
		ID:              pgID(w.ID),
		Name:            w.Name,
		Mode:            string(w.Mode),
		Url:             w.URL,
		CertFingerprint: w.CertFingerprint,
		SecretEnc:       w.SecretEnc,
		Info:            info,
		Epoch:           w.Link.Epoch,
		EpochStartedAt:  pgOptTime(w.Link.EpochStartedAt),
		PastEpochs:      textArray(w.Link.PastEpochs),
		AckSeq:          w.Link.AckSeq,
		LastSeen:        pgOptTime(w.Link.LastSeen),
		CreatedAt:       pgTime(w.CreatedAt),
		RemovedAt:       pgOptTime(w.RemovedAt),
	}, nil
}

func workerFrom(r db.Worker) (workers.Worker, error) {
	w := workers.Worker{
		Name:            r.Name,
		Mode:            domain.WorkerMode(r.Mode),
		URL:             r.Url,
		CertFingerprint: r.CertFingerprint,
		SecretEnc:       r.SecretEnc,
		CreatedAt:       timeFrom(r.CreatedAt),
		RemovedAt:       timeFrom(r.RemovedAt),
		Link: workers.Link{
			Epoch:          r.Epoch,
			EpochStartedAt: timeFrom(r.EpochStartedAt),
			AckSeq:         r.AckSeq,
			PastEpochs:     nilIfEmpty(r.PastEpochs),
			LastSeen:       timeFrom(r.LastSeen),
		},
	}
	if err := decodeID(&w.ID, r.ID); err != nil {
		return workers.Worker{}, err
	}
	info, err := decodeInfo(r.Info)
	if err != nil {
		return workers.Worker{}, err
	}
	w.Info = info
	return w, nil
}

func workersFrom(rows []db.Worker) ([]workers.Worker, error) {
	out := make([]workers.Worker, len(rows))
	for i, r := range rows {
		w, err := workerFrom(r)
		if err != nil {
			return nil, err
		}
		out[i] = w
	}
	return out, nil
}

func tokenFrom(r db.EnrollmentToken) (workers.EnrollmentToken, error) {
	t := workers.EnrollmentToken{
		CreatedAt: timeFrom(r.CreatedAt),
		ExpiresAt: timeFrom(r.ExpiresAt),
		UsedAt:    timeFrom(r.UsedAt),
	}
	if len(r.Hash) != len(t.Hash) {
		return workers.EnrollmentToken{}, fmt.Errorf("decode token: hash is %d bytes, want %d", len(r.Hash), len(t.Hash))
	}
	copy(t.Hash[:], r.Hash)
	if err := decodeID(&t.UsedBy, r.UsedBy); err != nil {
		return workers.EnrollmentToken{}, err
	}
	return t, nil
}

func statsFrom(bytes, totalBytes, files, totalFiles, checks, deletes, renames, errs, elapsed int64) jobs.Stats {
	return jobs.Stats{
		Bytes: bytes, TotalBytes: totalBytes, Files: files, TotalFiles: totalFiles,
		Checks: checks, Deletes: deletes, Renames: renames, Errors: errs,
		Elapsed: time.Duration(elapsed),
	}
}

func jobFrom(r db.Job) (jobs.Job, error) {
	j := jobs.Job{
		Kind:              r.Kind,
		Status:            domain.JobStatus(r.Status),
		Definition:        r.Definition,
		SearchText:        r.SearchText,
		Epoch:             r.Epoch,
		CancelRequestedAt: timeFrom(r.CancelRequestedAt),
		CreatedAt:         timeFrom(r.CreatedAt),
		QueuedAt:          timeFrom(r.QueuedAt),
		DispatchedAt:      timeFrom(r.DispatchedAt),
		StartedAt:         timeFrom(r.StartedAt),
		FinishedAt:        timeFrom(r.FinishedAt),
		Requeues:          int(r.Requeues),
		Stats: statsFrom(r.StatBytes, r.StatTotalBytes, r.StatFiles, r.StatTotalFiles,
			r.StatChecks, r.StatDeletes, r.StatRenames, r.StatErrors, r.StatElapsedNs),
		Error:    jobs.JobError{Code: r.ErrorCode, Message: r.ErrorMessage},
		Warnings: nilIfEmpty(r.Warnings),
		ExitCode: int(r.ExitCode),
		Output:   r.Output,
		Version:  r.Version,
	}
	if err := decodeID(&j.ID, r.ID); err != nil {
		return jobs.Job{}, err
	}
	if err := decodeID(&j.WorkerID, r.WorkerID); err != nil {
		return jobs.Job{}, err
	}
	if err := decodeID(&j.OwnerID, r.OwnerID); err != nil {
		return jobs.Job{}, err
	}
	return j, nil
}

func jobsFrom(rows []db.Job) ([]jobs.Job, error) {
	out := make([]jobs.Job, len(rows))
	for i, r := range rows {
		j, err := jobFrom(r)
		if err != nil {
			return nil, err
		}
		out[i] = j
	}
	return out, nil
}

// remoteIDsByJob groups job_remotes rows by job, keeping their order.
func remoteIDsByJob(rows []db.JobRemote) (map[pgtype.UUID][]domain.RemoteID, error) {
	out := make(map[pgtype.UUID][]domain.RemoteID)
	for _, r := range rows {
		var id domain.RemoteID
		if err := decodeID(&id, r.RemoteID); err != nil {
			return nil, err
		}
		out[r.JobID] = append(out[r.JobID], id)
	}
	return out, nil
}

// likeEscape escapes the LIKE wildcards ("%", "_") and the escape
// character (a backslash) of s with a backslash, so a pattern built from
// it (ESCAPE with a backslash) matches s literally.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func sampleFrom(r db.JobSample) (jobs.Sample, error) {
	s := jobs.Sample{
		Time: timeFrom(r.T),
		Stats: statsFrom(r.Bytes, r.TotalBytes, r.Files, r.TotalFiles,
			r.Checks, r.Deletes, r.Renames, r.Errors, r.ElapsedNs),
	}
	if err := decodeID(&s.JobID, r.JobID); err != nil {
		return jobs.Sample{}, err
	}
	return s, nil
}

// limitArg converts a row limit to the nullable query argument; a negative
// limit means no limit.
func limitArg(n int) *int64 {
	if n < 0 {
		return nil
	}
	v := int64(n)
	return &v
}
