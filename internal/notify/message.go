package notify

import (
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// MessageVersion is the current Message.Version.
const MessageVersion = 1

// Message is one notification. It is JSON-stable: it is the stored payload
// of a delivery and the body of the generic webhook (format "syncr"), so
// fields are only ever added within a Version. It contains no secrets.
type Message struct {
	// Version is MessageVersion.
	Version int `json:"version"`
	// ID is the delivery ID (set when the delivery is sent; empty in the
	// stored payload). It is stable across retries, so receivers can
	// deduplicate.
	ID string `json:"id"`
	// Event is what the notification reports.
	Event domain.NotificationEvent `json:"event"`
	// OccurredAt is when it happened (the job finished, the run was
	// missed).
	OccurredAt time.Time `json:"occurred_at"`
	// Title is a one-line summary (see Title), at most 200 runes.
	Title string `json:"title"`
	// URL links to the job or schedule in the web UI (http.public_url +
	// "/jobs/<id>" or "/schedules/<id>"); empty when no public URL is
	// configured.
	URL string `json:"url,omitempty"`
	// Schedule is set for a scheduled job or run.
	Schedule *ScheduleInfo `json:"schedule,omitempty"`
	// Job is set for a job outcome.
	Job *JobInfo `json:"job,omitempty"`
	// Run is set for a scheduled run that was missed or could not start.
	Run *RunInfo `json:"run,omitempty"`
	// Test marks a test notification.
	Test bool `json:"test,omitempty"`
}

// ScheduleInfo names the schedule of a notification.
type ScheduleInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// JobInfo describes the job a notification reports.
type JobInfo struct {
	ID         string           `json:"id"`
	Kind       string           `json:"kind"`
	Status     domain.JobStatus `json:"status"`
	WorkerID   string           `json:"worker_id"`
	WorkerName string           `json:"worker_name"`
	// ErrorCode and ErrorMessage say why a job failed or was interrupted
	// (see jobs.JobError).
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	// Bytes and Files transferred, and Errors counted, by the job.
	Bytes  int64 `json:"bytes"`
	Files  int64 `json:"files"`
	Errors int64 `json:"errors"`
	// StartedAt is when the job's process started (worker clock); zero if
	// it never did. FinishedAt is when the job ended.
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	// DurationSeconds is FinishedAt - StartedAt; zero if the job never
	// started.
	DurationSeconds float64 `json:"duration_seconds"`
}

// RunInfo describes a scheduled run that was missed or failed to start.
type RunInfo struct {
	ID string `json:"id"`
	// ScheduledFor is the occurrence the run was for.
	ScheduledFor time.Time `json:"scheduled_for"`
	// Reason is the run's reason code (e.g. "worker_offline"), Message its
	// details.
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
	// Coalesced counts earlier occurrences merged into the run.
	Coalesced int `json:"coalesced"`
}
