// Package memstore is an in-memory implementation of the controller's
// repository ports (workers.Repository, workers.TokenRepository,
// jobs.Repository, jobs.SampleRepository, jobs.ActivityRepository,
// users.Repository, users.SessionRepository, users.APITokenRepository,
// remotes.Repository, schedules.Repository, contacts.Repository,
// notifications.Repository).
// It backs unit tests and a
// development controller without Postgres (dev.memory_store); everything is
// lost when the process exits. The live job broker is a separate adapter
// (internal/adapters/broker), used by every controller.
//
// It follows the ports' contracts exactly, including the conditional
// updates and the referential rules the database enforces with foreign keys
// and unique indexes, so tests against it exercise the same rules a database
// adapter must provide. A single mutex serializes the store, which
// makes every method trivially atomic; values are deep-copied in and out, so
// callers never share memory with the store.
//
// Allowed dependencies: the standard library, internal/domain and the
// internal/app packages whose ports it implements.
package memstore

import (
	"cmp"
	"slices"
	"sync"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/contacts"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/notifications"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Store holds workers, enrollment tokens, jobs, samples, users, sessions,
// API tokens, remotes, schedules with their runs, contacts and deliveries.
// The zero value is not usable; create it with New. Safe for concurrent
// use.
type Store struct {
	mu      sync.RWMutex
	workers map[domain.WorkerID]workers.Worker
	tokens  map[workers.TokenHash]workers.EnrollmentToken
	jobs    map[domain.JobID]jobs.Job
	// samples per job, sorted by Time.
	samples map[domain.JobID][]jobs.Sample

	users     map[domain.UserID]users.User
	sessions  map[users.TokenHash]users.Session
	apiTokens map[domain.APITokenID]users.APIToken
	// remotes holds live and deleted remotes, each with its members.
	remotes map[domain.RemoteID]remotes.Remote

	// schedules holds schedules with their remote and contact links (the
	// schedule_remotes and schedule_contacts rows).
	schedules map[domain.ScheduleID]schedules.Schedule
	// runs holds schedule runs (JobStatus is not stored: it is read from
	// jobs).
	runs map[domain.ScheduleRunID]schedules.Run
	// contacts holds notification contacts.
	contacts map[domain.ContactID]contacts.Contact
	// deliveries is the notification outbox.
	deliveries map[domain.DeliveryID]notifications.Delivery
	// jobNotified and runNotified are the notified_at columns of jobs and
	// schedule_runs.
	jobNotified map[domain.JobID]time.Time
	runNotified map[domain.ScheduleRunID]time.Time
}

// Compile-time checks that Store implements the ports.
var (
	_ workers.Repository       = (*Store)(nil)
	_ workers.TokenRepository  = (*Store)(nil)
	_ jobs.Repository          = (*Store)(nil)
	_ jobs.SampleRepository    = (*Store)(nil)
	_ users.Repository         = (*Store)(nil)
	_ users.SessionRepository  = (*Store)(nil)
	_ users.APITokenRepository = (*Store)(nil)
	_ remotes.Repository       = (*Store)(nil)
	_ jobs.ActivityRepository  = (*Store)(nil)
	_ schedules.Repository     = (*Store)(nil)
	_ contacts.Repository      = (*Store)(nil)
	_ notifications.Repository = (*Store)(nil)
)

// New returns an empty Store.
func New() *Store {
	return &Store{
		workers:   make(map[domain.WorkerID]workers.Worker),
		tokens:    make(map[workers.TokenHash]workers.EnrollmentToken),
		jobs:      make(map[domain.JobID]jobs.Job),
		samples:   make(map[domain.JobID][]jobs.Sample),
		users:     make(map[domain.UserID]users.User),
		sessions:  make(map[users.TokenHash]users.Session),
		apiTokens: make(map[domain.APITokenID]users.APIToken),
		remotes:   make(map[domain.RemoteID]remotes.Remote),

		schedules:   make(map[domain.ScheduleID]schedules.Schedule),
		runs:        make(map[domain.ScheduleRunID]schedules.Run),
		contacts:    make(map[domain.ContactID]contacts.Contact),
		deliveries:  make(map[domain.DeliveryID]notifications.Delivery),
		jobNotified: make(map[domain.JobID]time.Time),
		runNotified: make(map[domain.ScheduleRunID]time.Time),
	}
}

func cloneWorker(w workers.Worker) workers.Worker {
	w.SecretEnc = slices.Clone(w.SecretEnc)
	w.Labels = slices.Clone(w.Labels)
	w.Info.Tools = slices.Clone(w.Info.Tools)
	w.Info.JobKinds = slices.Clone(w.Info.JobKinds)
	w.Info.LocalRoots = slices.Clone(w.Info.LocalRoots)
	w.Link.PastEpochs = slices.Clone(w.Link.PastEpochs)
	return w
}

func cloneJob(j jobs.Job) jobs.Job {
	j.Definition = slices.Clone(j.Definition)
	j.RemoteIDs = slices.Clone(j.RemoteIDs)
	j.Warnings = slices.Clone(j.Warnings)
	j.Output = slices.Clone(j.Output)
	return j
}

// compareIDs orders IDs by their bytes (UUIDv7: creation order).
func compareIDs[K any](a, b domain.ID[K]) int {
	ua, ub := a.UUID(), b.UUID()
	for i := range ua {
		if c := cmp.Compare(ua[i], ub[i]); c != 0 {
			return c
		}
	}
	return 0
}
