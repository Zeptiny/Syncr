// Package devseed fills an empty controller with fixed demo data, for UI
// development and screenshots: two users (admin and alice, with public
// passwords), workers in every connection state with labels (one that was
// removed, and one too old for probes and restic), the admin's remotes of
// every built-in backend type (a union among them), restic repositories
// with their snapshots (one whose data is being deleted, one already
// deleted), a contact of every built-in channel, schedules of every target
// type and policy with about a month of runs, and about 270 jobs of every
// kind over those 30 days in every status with statistics, results,
// errors, warnings and stored progress samples, and their notifications in
// every delivery status. One job keeps running: Animate reports its
// progress every second through the controller's job service, so live
// views move, and keeps the online workers online. Two more are shown
// running without moving: a backup with another waiting behind it on the
// same repository, and a check that a data deletion waits for.
//
// The demo workers, remotes and repositories do not exist. Probes answers
// the probes a seeded controller would send them (folder listings, remote
// tests, snapshot listings) with canned results.
//
// The seed runs in the controller's process when dev.seed is set, which
// configuration validation allows only together with dev.memory_store
// (and under which the controller's notification channels only log what
// they would send). It writes through the use cases (users, remotes,
// workers, contacts, jobs, schedules, notifications). Where they offer no
// way to create the past (history with its times, a worker last seen hours
// ago) it replays the history with services of its own whose clock it sets
// (history.go), or uses the store port directly. Times are relative to the
// seed's start; everything else (names, paths, statistics, the IDs'
// random bits) is fixed.
//
// Allowed dependencies: the standard library, the generated protobuf code,
// internal/domain, internal/app/..., internal/jobkind, internal/link,
// internal/cronexpr, internal/descriptor, internal/notify and
// internal/platform packages. Never the Postgres adapter, controller or
// worker internals.
package devseed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/contacts"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/notifications"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/notify"
)

// Demo credentials. They are public (documented): a seeded controller must
// never be reachable by anyone else.
//
//nolint:gosec // G101: public demo credentials
const (
	AdminUsername = "admin"
	AdminPassword = "syncr-demo-admin"
	AliceUsername = "alice"
	AlicePassword = "syncr-demo-alice"
)

// Store is what the seed writes directly, below the use cases: the
// history (through services of its own: jobs, schedules and their runs,
// deliveries), the repositories with their snapshot caches and deletions,
// a worker's last contact in the past, and the deliveries it leaves
// sending or queued again.
type Store interface {
	jobs.Repository
	jobs.SampleRepository
	schedules.Repository
	notifications.Repository
	restic.Repository
	// TouchWorker sets the worker's last contact to at (if later).
	TouchWorker(ctx context.Context, id domain.WorkerID, at time.Time) error
}

// Clock tells the time.
type Clock interface {
	Now() time.Time
}

// Deps are the seed's collaborators: the controller's services and store.
// All are required except Describer, Probes and Logger.
type Deps struct {
	Users   *users.Service
	Remotes *remotes.Service
	Workers *workers.Service
	// Jobs is the controller's job service: Animate records the running
	// job's progress through it, so its watchers (WatchJob) see it.
	Jobs  *jobs.Service
	Kinds *jobkind.Registry
	// Contacts creates the demo contacts; the history's schedules and
	// outbox check and resolve them through it.
	Contacts *contacts.Service
	// Channels are the controller's notification channels (the history's
	// outbox sends through scripted copies of them, which never send).
	Channels *notify.Registry
	// Describer describes the jobs of the history's notifications (the
	// controller's notifications.JobDescriber); nil leaves them
	// undescribed.
	Describer notifications.JobDescriber
	// Cipher encrypts the demo repositories' password (the controller's
	// secrets keyring).
	Cipher Cipher
	// Probes are the canned probe answers the controller uses instead of
	// its demo workers; Run tells them the seed's start and the workers'
	// local roots. Optional.
	Probes *Probes
	Store  Store
	Clock  Clock
	// Logger defaults to discarding.
	Logger *slog.Logger
}

// ErrNotEmpty is returned by Run when the store already has users: the seed
// only fills an empty controller (it matches domain.ErrFailedPrecondition).
var ErrNotEmpty = fmt.Errorf("%w: the store is not empty (the demo data is seeded once, into a new in-memory store)", domain.ErrFailedPrecondition)

// Seed is the seeded demo data that Animate keeps alive.
type Seed struct {
	d   Deps
	log *slog.Logger

	// Admin and Alice are the demo users.
	Admin, Alice domain.Actor
	// Workers are the demo workers by name.
	Workers map[string]domain.WorkerID
	// Remotes are the demo remotes by name (the admin's and alice's).
	Remotes map[string]domain.RemoteID
	// Repositories are the demo restic repositories by name (the one
	// whose data was deleted included).
	Repositories map[string]domain.ResticRepoID
	// Contacts and Schedules are the demo contacts and schedules by name.
	Contacts  map[string]domain.ContactID
	Schedules map[string]domain.ScheduleID
	// Jobs are the seeded jobs (manual and scheduled), oldest first.
	Jobs []domain.JobID
	// Running is the job that Animate keeps running.
	Running domain.JobID

	// online are the workers Animate keeps online.
	online []domain.WorkerID
	// runningWorker, runningStart and runningEpoch identify the running
	// job's run.
	runningWorker domain.WorkerID
	runningStart  time.Time
	runningEpoch  string
}

// Run seeds the demo data into an empty controller. It fails with
// ErrNotEmpty if any user exists (so it never runs twice on one store).
func Run(ctx context.Context, d Deps) (*Seed, error) {
	if d.Users == nil || d.Remotes == nil || d.Workers == nil || d.Jobs == nil || d.Kinds == nil || d.Contacts == nil ||
		d.Channels == nil || d.Cipher == nil || d.Store == nil || d.Clock == nil {
		return nil, errors.New("devseed: missing dependency")
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	empty, err := d.Users.SetupRequired(ctx)
	if err != nil {
		return nil, fmt.Errorf("devseed: %w", err)
	}
	if !empty {
		return nil, fmt.Errorf("devseed: %w", ErrNotEmpty)
	}
	s := &Seed{
		d: d, log: log, Workers: map[string]domain.WorkerID{}, Remotes: map[string]domain.RemoteID{},
		Repositories: map[string]domain.ResticRepoID{},
		Contacts:     map[string]domain.ContactID{}, Schedules: map[string]domain.ScheduleID{},
	}
	now := d.Clock.Now()
	steps := []struct {
		what string
		fn   func(context.Context, time.Time) error
	}{
		{"users", s.seedUsers},
		{"workers", s.seedWorkers},
		{"remotes", s.seedRemotes},
		{"repositories", s.seedRepositories},
		{"contacts", s.seedContacts},
		{"history", s.seedHistory},
		{"snapshot marks", s.markSnapshotCaches},
	}
	for _, step := range steps {
		if err := step.fn(ctx, now); err != nil {
			return nil, fmt.Errorf("devseed: %s: %w", step.what, err)
		}
	}
	log.Warn("DEVELOPMENT MODE: dev.seed filled the in-memory store with demo data; log in as "+AdminUsername+" / "+AdminPassword+
		" or "+AliceUsername+" / "+AlicePassword+" (public passwords: never expose this controller)",
		"workers", len(s.Workers), "remotes", len(s.Remotes), "repositories", len(s.Repositories), "contacts", len(s.Contacts), "schedules", len(s.Schedules), "jobs", len(s.Jobs))
	return s, nil
}

// seedSetupCode completes the first-run setup of the seed (it is armed and
// used at once, in-process, and never logged).
const seedSetupCode = "DEVSD-SEEDX-DEMOX-ADMIN"

func (s *Seed) seedUsers(ctx context.Context, _ time.Time) error {
	if err := s.d.Users.ArmSetup(seedSetupCode); err != nil {
		return err
	}
	admin, _, err := s.d.Users.CompleteSetup(ctx, users.SetupParams{
		Code: seedSetupCode, Username: AdminUsername, DisplayName: "Ada Admin", Password: AdminPassword,
		ClientIP: "127.0.0.1", UserAgent: "devseed",
	})
	if err != nil {
		return err
	}
	s.Admin = domain.Actor{UserID: admin.ID, Role: admin.Role}
	alice, err := s.d.Users.Create(ctx, s.Admin, users.CreateParams{
		Username: AliceUsername, DisplayName: "Alice Moreau", Password: AlicePassword, Role: domain.RoleUser,
	})
	if err != nil {
		return err
	}
	s.Alice = domain.Actor{UserID: alice.ID, Role: alice.Role}
	return nil
}

// actor returns the demo user of an owner index (0 admin, 1 alice).
func (s *Seed) actor(owner int) domain.Actor {
	if owner == ownerAlice {
		return s.Alice
	}
	return s.Admin
}

// epochOf is the link epoch of a demo worker's (pretend) process.
func epochOf(worker string) string { return "devseed-" + worker }

func (s *Seed) seedWorkers(ctx context.Context, now time.Time) error {
	for _, w := range demoWorkers() {
		id, err := s.addWorker(ctx, w)
		if err != nil {
			return fmt.Errorf("worker %q: %w", w.name, err)
		}
		s.Workers[w.name] = id
		if w.status == domain.WorkerPending {
			continue
		}
		wk, err := s.d.Workers.Get(ctx, id)
		if err != nil {
			return err
		}
		epoch := epochOf(w.name)
		if _, err := s.d.Workers.AdvanceEpoch(ctx, wk, epoch); err != nil {
			return err
		}
		w.info.ReportedAt = now.Add(-w.lastSeen)
		if err := s.d.Workers.SetInfo(ctx, id, epoch, w.info); err != nil {
			return err
		}
		if err := s.d.Store.TouchWorker(ctx, id, now.Add(-w.lastSeen)); err != nil {
			return err
		}
		if len(w.labels) > 0 {
			if _, err := s.d.Workers.SetLabels(ctx, id, w.labels); err != nil {
				return err
			}
		}
		if w.status == domain.WorkerOnline {
			s.online = append(s.online, id)
		}
	}
	s.bindProbes(now)
	return nil
}

func (s *Seed) addWorker(ctx context.Context, w demoWorker) (domain.WorkerID, error) {
	if w.mode == domain.WorkerPassive {
		wk, err := s.d.Workers.RegisterPassive(ctx, workers.PassiveParams{
			Name: w.name, URL: w.url, CertFingerprint: w.fingerprint, Secret: passivePairingSecret,
		})
		return wk.ID, err
	}
	grant, err := s.d.Workers.CreateEnrollmentToken(ctx, 0)
	if err != nil {
		return domain.WorkerID{}, err
	}
	e, err := s.d.Workers.Enroll(ctx, workers.EnrollParams{Token: grant.Token, Name: w.name, OS: w.info.OS, Arch: w.info.Arch, Mode: w.mode})
	return e.Worker.ID, err
}

func (s *Seed) seedRemotes(ctx context.Context, _ time.Time) error {
	for _, r := range demoRemotes() {
		in := r.input
		for _, m := range r.members {
			id, ok := s.Remotes[m]
			if !ok {
				return fmt.Errorf("remote %q: member %q is not seeded before it", in.Name, m)
			}
			in.Members = append(in.Members, remotes.Member{RemoteID: id, Mode: backend.UnionReadWrite})
		}
		created, err := s.d.Remotes.Create(ctx, s.actor(r.owner), in)
		if err != nil {
			return fmt.Errorf("remote %q: %w", in.Name, err)
		}
		s.Remotes[in.Name] = created.ID
	}
	return nil
}

// seedContacts creates the demo contacts.
func (s *Seed) seedContacts(ctx context.Context, _ time.Time) error {
	for _, c := range demoContacts() {
		created, err := s.d.Contacts.Create(ctx, s.actor(c.owner), contacts.Input{
			Name: c.name, Type: c.channel, Options: c.options, Secrets: c.secrets, Enabled: true,
			JobEvents: domain.EventSet(c.jobEvents).Normalize(),
		})
		if err != nil {
			return fmt.Errorf("contact %q: %w", c.name, err)
		}
		s.Contacts[c.name] = created.ID
	}
	return nil
}
