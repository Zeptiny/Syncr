package notifications

import (
	"cmp"
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/notify"
)

// Retry delays: the first retry waits BaseBackoff, every further one twice
// as long, up to MaxBackoff; each delay d is jittered into [d/2, d]. A
// target's RetryAfter replaces the delay (capped at MaxBackoff). With the
// default 8 attempts the last one comes about an hour after the first.
const (
	BaseBackoff = 30 * time.Second
	MaxBackoff  = time.Hour
)

// PurgeInterval is how often Run purges old deliveries.
const PurgeInterval = time.Hour

// MaxErrorMessageRunes bounds the job error message stored in a delivery's
// payload (tool output can be long).
const MaxErrorMessageRunes = 1000

const (
	// planBatch is how many outcomes Plan reads per query.
	planBatch = 100
	// maxDeliverRounds bounds one DeliverDue pass (rounds of
	// Config.Concurrency claims); a full pass wakes the loop again.
	maxDeliverRounds = 64
	// completeTimeout bounds recording an attempt's outcome, which runs
	// even when the pass's context ended (shutdown), so no outcome of a
	// finished send is lost.
	completeTimeout = 10 * time.Second
	// reasonOwnerDisabled is schedules.ReasonOwnerDisabled: a run that
	// failed because its owner is disabled notifies nobody (app packages
	// cannot import each other; a test pins the value).
	reasonOwnerDisabled = "owner_disabled"
)

// Deps are the service's collaborators. All are required except Jobs,
// Random, PublicURL and Logger.
type Deps struct {
	Repo Repository
	// Channels are the channel types deliveries are sent with.
	Channels *notify.Registry
	// Contacts resolves a delivery's contact at send time
	// (*contacts.Service).
	Contacts ContactResolver
	// Admins tells whether a delivery's owner is an administrator. Only
	// administrators are shown what a destination or the network answered
	// (see notify.SendError.Public); everybody else gets
	// notify.GenericReason in the delivery log. Nil means nobody is an
	// administrator.
	Admins AdminChecker
	// Jobs describes the reported jobs (notify.JobInfo.Description, the
	// title of a job without a schedule); nil means jobs are not described
	// (titles name their kind).
	Jobs  JobDescriber
	Clock Clock
	IDs   IDGenerator
	// Random is the source of the retry jitter; nil means crypto/rand.
	Random io.Reader
	// PublicURL is the web UI's base URL (http.public_url), for the links
	// in messages; empty for none.
	PublicURL string
	// Logger defaults to discarding. The service never logs a contact's
	// configuration.
	Logger *slog.Logger
}

// Service implements the outbox (see the package documentation). It is
// safe for concurrent use.
type Service struct {
	repo      Repository
	channels  *notify.Registry
	contacts  ContactResolver
	admins    AdminChecker
	jobs      JobDescriber
	clock     Clock
	ids       IDGenerator
	publicURL string
	log       *slog.Logger
	cfg       Config

	randMu sync.Mutex
	random io.Reader

	wake chan struct{}
}

// New returns a Service.
func New(d Deps, cfg Config) (*Service, error) {
	if d.Repo == nil || d.Channels == nil || d.Contacts == nil || d.Clock == nil || d.IDs == nil {
		return nil, errors.New("notifications: missing dependency")
	}
	def := DefaultConfig()
	cfg.MaxAttempts = cmp.Or(cfg.MaxAttempts, def.MaxAttempts)
	cfg.Concurrency = cmp.Or(cfg.Concurrency, def.Concurrency)
	cfg.Lease = cmp.Or(cfg.Lease, def.Lease)
	cfg.Tick = cmp.Or(cfg.Tick, def.Tick)
	cfg.Retention = cmp.Or(cfg.Retention, def.Retention)
	cfg.SendTimeout = cmp.Or(cfg.SendTimeout, def.SendTimeout)
	switch {
	case cfg.MaxAttempts < 1, cfg.Concurrency < 1:
		return nil, fmt.Errorf("notifications: invalid config %+v: max attempts and concurrency must be at least 1", cfg)
	case cfg.Tick < 0, cfg.Retention < 0, cfg.SendTimeout < 0:
		return nil, fmt.Errorf("notifications: invalid config %+v: durations must be positive", cfg)
	case cfg.Lease <= cfg.SendTimeout:
		// A lease that can expire during a send lets another deliverer
		// send the same notification again.
		return nil, fmt.Errorf("notifications: invalid config %+v: the lease must be longer than the send timeout", cfg)
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	random := d.Random
	if random == nil {
		random = crand.Reader
	}
	return &Service{
		repo: d.Repo, channels: d.Channels, contacts: d.Contacts, admins: d.Admins, jobs: d.Jobs, clock: d.Clock, ids: d.IDs,
		publicURL: strings.TrimRight(d.PublicURL, "/"), log: log, cfg: cfg, random: random,
		wake: make(chan struct{}, 1),
	}, nil
}

// Run plans, delivers and (every PurgeInterval) purges until ctx ends: at
// once, every Config.Tick, and whenever Wake is called. It returns nil when
// ctx ends, after the attempts in flight were recorded; errors are logged.
func (s *Service) Run(ctx context.Context) error {
	t := time.NewTicker(s.cfg.Tick)
	defer t.Stop()
	var lastPurge time.Time
	for {
		s.pass(ctx, &lastPurge)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-s.wake:
		}
	}
}

// pass runs the loops once.
func (s *Service) pass(ctx context.Context, lastPurge *time.Time) {
	if _, err := s.Plan(ctx); err != nil && ctx.Err() == nil {
		s.log.ErrorContext(ctx, "notification planning failed", "error", err.Error())
	}
	if _, err := s.DeliverDue(ctx); err != nil && ctx.Err() == nil {
		s.log.ErrorContext(ctx, "notification delivery failed", "error", err.Error())
	}
	if now := s.clock.Now(); lastPurge.IsZero() || now.Sub(*lastPurge) >= PurgeInterval {
		*lastPurge = now
		n, err := s.Purge(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			s.log.ErrorContext(ctx, "delivery purge failed", "error", err.Error())
		case n > 0:
			s.log.InfoContext(ctx, "old deliveries purged", "deliveries", n)
		}
	}
}

// Wake asks Run for a pass soon (a job finished, a run ended, a delivery
// was retried). It never blocks.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Plan plans the notifications of every outcome that was not notified yet
// (finished jobs, missed and failed runs) and returns how many deliveries
// it planned. Each outcome is committed with its notified mark in one
// transaction, and the dedupe keys make planning an outcome again insert
// nothing, so concurrent planners and a crash between the steps never
// plan a notification twice. An outcome that fails to commit stays pending
// and is retried by the next pass; its error is returned.
func (s *Service) Plan(ctx context.Context) (int, error) {
	jobs, jerr := s.planJobs(ctx)
	runs, rerr := s.planRuns(ctx)
	return jobs + runs, errors.Join(jerr, rerr)
}

func (s *Service) planJobs(ctx context.Context) (int, error) {
	planned := 0
	for {
		os, err := s.repo.PendingJobOutcomes(ctx, planBatch)
		if err != nil {
			return planned, fmt.Errorf("plan job notifications: %w", err)
		}
		committed := 0
		var errs []error
		for _, o := range os {
			now := s.clock.Now()
			ds, err := s.jobDeliveries(ctx, o, now)
			if err == nil {
				var n int
				if n, err = s.repo.CommitJobOutcome(ctx, o.JobID, ds, now); err == nil {
					committed++
					planned += n
					continue
				}
			}
			errs = append(errs, fmt.Errorf("plan notifications of job %s: %w", o.JobID, err))
		}
		if len(os) < planBatch || committed == 0 || ctx.Err() != nil {
			return planned, errors.Join(errs...)
		}
	}
}

func (s *Service) planRuns(ctx context.Context) (int, error) {
	planned := 0
	for {
		os, err := s.repo.PendingRunOutcomes(ctx, planBatch)
		if err != nil {
			return planned, fmt.Errorf("plan run notifications: %w", err)
		}
		committed := 0
		var errs []error
		for _, o := range os {
			now := s.clock.Now()
			ds, err := s.runDeliveries(o, now)
			if err == nil {
				var n int
				if n, err = s.repo.CommitRunOutcome(ctx, o.RunID, ds, now); err == nil {
					committed++
					planned += n
					continue
				}
			}
			errs = append(errs, fmt.Errorf("plan notifications of run %s: %w", o.RunID, err))
		}
		if len(os) < planBatch || committed == 0 || ctx.Err() != nil {
			return planned, errors.Join(errs...)
		}
	}
}

// jobDeliveries returns the deliveries of a finished job: one per
// subscriber of its event. Cancelled jobs have none. The job is described
// only when it has deliveries.
func (s *Service) jobDeliveries(ctx context.Context, o JobOutcome, now time.Time) ([]Delivery, error) {
	e, ok := domain.EventForJobStatus(o.Status)
	if !ok {
		return nil, nil
	}
	subs := subscribersOf(o.Subscribers, e)
	if len(subs) == 0 {
		return nil, nil
	}
	m := notify.Message{
		Version: notify.MessageVersion, Event: e, OccurredAt: o.FinishedAt,
		Job: &notify.JobInfo{
			ID: o.JobID.String(), Kind: o.Kind, Status: o.Status, WorkerID: o.WorkerID.String(), WorkerName: o.WorkerName,
			ErrorCode: o.ErrorCode, ErrorMessage: truncateRunes(o.ErrorMessage, MaxErrorMessageRunes),
			Bytes: o.Bytes, Files: o.Files, Errors: o.Errors, StartedAt: o.StartedAt, FinishedAt: o.FinishedAt,
		},
	}
	if s.jobs != nil {
		desc, err := s.jobs.DescribeJob(ctx, o.OwnerID, o.Kind, o.Definition)
		if err != nil {
			return nil, fmt.Errorf("describe the job: %w", err)
		}
		m.Job.Description = truncateRunes(desc, notify.MaxFactRunes)
	}
	if m.OccurredAt.IsZero() {
		m.OccurredAt = now
	}
	if !o.StartedAt.IsZero() && o.FinishedAt.After(o.StartedAt) {
		m.Job.DurationSeconds = o.FinishedAt.Sub(o.StartedAt).Seconds()
	}
	if !o.ScheduleID.IsZero() {
		m.Schedule = &notify.ScheduleInfo{ID: o.ScheduleID.String(), Name: o.ScheduleName}
	}
	if s.publicURL != "" {
		m.URL = s.publicURL + "/jobs/" + o.JobID.String()
	}
	m.Title = notify.Title(m)
	return s.deliveries(subs, m, o.OwnerID, JobDedupeKey(o.JobID, e), o.JobID, o.ScheduleID, now)
}

// runDeliveries returns the deliveries of a missed or failed run: one per
// subscriber of its event. A run that failed because its owner is
// disabled has none.
func (s *Service) runDeliveries(o RunOutcome, now time.Time) ([]Delivery, error) {
	var e domain.NotificationEvent
	switch o.Status {
	case "missed":
		e = domain.EventMissed
	case "failed":
		e = domain.EventFailed
	default:
		return nil, nil
	}
	subs := subscribersOf(o.Subscribers, e)
	if len(subs) == 0 || o.Reason == reasonOwnerDisabled {
		return nil, nil
	}
	m := notify.Message{
		Version: notify.MessageVersion, Event: e, OccurredAt: o.UpdatedAt,
		Schedule: &notify.ScheduleInfo{ID: o.ScheduleID.String(), Name: o.ScheduleName},
		Run: &notify.RunInfo{
			ID: o.RunID.String(), ScheduledFor: o.ScheduledFor, Reason: o.Reason,
			Message: truncateRunes(o.Message, MaxErrorMessageRunes), Coalesced: o.Coalesced,
		},
	}
	if m.OccurredAt.IsZero() {
		m.OccurredAt = now
	}
	if s.publicURL != "" {
		m.URL = s.publicURL + "/schedules/" + o.ScheduleID.String()
	}
	m.Title = notify.Title(m)
	return s.deliveries(subs, m, o.OwnerID, RunDedupeKey(o.RunID, e), domain.JobID{}, o.ScheduleID, now)
}

// subscribersOf returns the contacts of subs subscribed to e, each once.
func subscribersOf(subs []Subscriber, e domain.NotificationEvent) []domain.ContactID {
	var out []domain.ContactID
	seen := map[domain.ContactID]bool{}
	for _, sub := range subs {
		if sub.Events.Contains(e) && !seen[sub.ContactID] {
			seen[sub.ContactID] = true
			out = append(out, sub.ContactID)
		}
	}
	return out
}

// deliveries builds one pending delivery of m to each contact.
func (s *Service) deliveries(contacts []domain.ContactID, m notify.Message, owner domain.UserID, key string,
	job domain.JobID, schedule domain.ScheduleID, now time.Time,
) ([]Delivery, error) {
	payload, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode the message: %w", err)
	}
	out := make([]Delivery, 0, len(contacts))
	for _, c := range contacts {
		out = append(out, Delivery{
			ID: s.ids.NewDeliveryID(), ContactID: c, OwnerID: owner, Event: m.Event, DedupeKey: key,
			JobID: job, ScheduleID: schedule, Payload: payload, Status: StatusPending,
			MaxAttempts: s.cfg.MaxAttempts, NextAttemptAt: now, CreatedAt: now,
		})
	}
	return out, nil
}

// DeliverDue sends the deliveries that are due, Config.Concurrency at a
// time, and returns how many attempts it made. Each attempt is claimed with
// a lease first, so concurrent deliverers (and controllers) never send one
// delivery at the same time; an attempt whose deliverer died is reclaimed
// when its lease expires. One call makes a bounded number of rounds and
// wakes Run if more may be due.
func (s *Service) DeliverDue(ctx context.Context) (int, error) {
	attempts := 0
	for range maxDeliverRounds {
		ds, err := s.repo.ClaimDeliveries(ctx, s.clock.Now(), s.cfg.Lease, s.cfg.Concurrency)
		if err != nil {
			return attempts, fmt.Errorf("claim deliveries: %w", err)
		}
		var wg sync.WaitGroup
		for _, d := range ds {
			wg.Go(func() { s.attempt(ctx, d) })
		}
		wg.Wait()
		attempts += len(ds)
		if len(ds) < s.cfg.Concurrency || ctx.Err() != nil {
			return attempts, nil
		}
	}
	s.Wake()
	return attempts, nil
}

// result is the outcome of one send.
type result struct {
	delivered bool
	// gone: the contact (and with it the delivery) was deleted.
	gone bool
	// interrupted: the pass's context ended during the attempt.
	interrupted bool
	permanent   bool
	retryAfter  time.Duration
	status      int
	// reason is the LastError of a failed attempt; never a secret. It is
	// what the owner is shown: detail, or notify.GenericReason when the
	// owner may not see the network details.
	reason string
	// detail, if set, is the full reason for the log (never a secret).
	detail string
}

// attempt sends one claimed delivery and records the outcome.
func (s *Service) attempt(ctx context.Context, d Delivery) {
	log := s.log.With("delivery_id", d.ID.String(), "contact_id", d.ContactID.String(),
		"event", string(d.Event), "attempt", d.Attempts)
	var r result
	if d.Attempts > d.MaxAttempts {
		// The last attempt's lease expired: its deliverer died or hung.
		r = result{permanent: true, reason: "the last attempt did not finish"}
	} else {
		r = s.send(ctx, d, log)
	}
	if r.gone {
		log.DebugContext(ctx, "notification dropped: its contact was deleted")
		return
	}

	now := s.clock.Now()
	c := Completion{ID: d.ID, Attempts: d.Attempts, At: now, LastError: r.reason}
	switch {
	case r.delivered:
		c.Status, c.LastError = StatusDelivered, ""
	case r.interrupted:
		// Our own shutdown, not the target's fault: whatever attempt this
		// was, even the last one, it does not count. Give it back and retry
		// as soon as a deliverer runs again.
		c.Status, c.NextAttemptAt, c.Release = StatusPending, now, true
	case r.permanent || d.Attempts >= d.MaxAttempts:
		c.Status = StatusDead
	default:
		c.Status, c.NextAttemptAt = StatusPending, now.Add(s.backoff(d.Attempts, r.retryAfter))
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), completeTimeout)
	defer cancel()
	_, err := s.repo.CompleteDelivery(cctx, c)
	switch {
	case errors.Is(err, domain.ErrConflict):
		log.WarnContext(ctx, "notification attempt outlived its lease; a later attempt owns the delivery")
		return
	case errors.Is(err, domain.ErrNotFound):
		log.DebugContext(ctx, "notification dropped: its contact was deleted")
		return
	case err != nil:
		log.ErrorContext(ctx, "cannot record a notification attempt", "error", err.Error())
		return
	}
	switch c.Status {
	case StatusDelivered:
		log.InfoContext(ctx, "notification delivered")
	case StatusDead:
		log.WarnContext(ctx, "notification failed for good", "status", r.status, "reason", cmp.Or(r.detail, r.reason))
	default:
		log.WarnContext(ctx, "notification attempt failed; retrying", "status", r.status, "reason", cmp.Or(r.detail, r.reason),
			"next_attempt_at", c.NextAttemptAt.Format(time.RFC3339))
	}
}

// send resolves d's contact and sends its message.
func (s *Service) send(ctx context.Context, d Delivery, log *slog.Logger) result {
	typ, cfg, err := s.contacts.ResolveChannel(ctx, d.ContactID)
	defer clear(cfg.Secrets)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return result{gone: true}
	case errors.Is(err, domain.ErrFailedPrecondition):
		return result{permanent: true, reason: "the contact's credentials cannot be read"}
	case err != nil:
		if ctx.Err() != nil {
			return result{interrupted: true, reason: "the controller stopped during the attempt"}
		}
		log.ErrorContext(ctx, "cannot load a notification's contact", "error", err.Error())
		return result{reason: "the contact could not be loaded"}
	}
	ch, ok := s.channels.Get(typ)
	if !ok {
		return result{permanent: true, reason: fmt.Sprintf("channel type %q is not available", typ)}
	}
	var msg notify.Message
	if err := json.Unmarshal(d.Payload, &msg); err != nil {
		return result{permanent: true, reason: "the stored notification cannot be read"}
	}
	msg.ID = d.ID.String()

	red := notify.NewRedactor(cfg)
	sendCtx, cancel := context.WithTimeout(ctx, s.cfg.SendTimeout)
	defer cancel()
	err = ch.Send(sendCtx, cfg, msg)
	var serr *notify.SendError
	// answered: the target replied, whatever our context did afterwards.
	answered := errors.As(err, &serr) && serr.Status != 0
	switch {
	case err == nil:
		return result{delivered: true}
	case ctx.Err() != nil && !answered:
		// Our own shutdown cut the send short (a receiver's timeout is the
		// send context's, not ours). A reply the target gave counts even if
		// the shutdown came right after it.
		return result{interrupted: true, reason: "the controller stopped during the attempt"}
	case errors.As(err, &serr):
		reason := withStatus(serr.Status, cmp.Or(serr.Reason, "the target rejected the notification"))
		// The channel redacted the reply; this is the safety net for what it
		// did not think of.
		reason = red.Redact(reason)
		r := result{permanent: serr.Permanent, retryAfter: serr.RetryAfter, status: serr.Status, reason: reason, detail: reason}
		if !serr.Public && !s.isAdmin(ctx, d.OwnerID, log) {
			// What a destination answered (refused, filtered, TLS or HTTP
			// details) would let any user probe the controller's network
			// through a contact, and the delivery log shows reasons to the
			// owner. Administrators control the network policy and see it.
			r.reason = notify.GenericReason
		}
		return r
	case errors.Is(sendCtx.Err(), context.DeadlineExceeded):
		return result{reason: fmt.Sprintf("no answer within %s", s.cfg.SendTimeout)}
	default:
		// Only a *SendError is known to be free of secrets.
		return result{reason: "the notification could not be sent"}
	}
}

// isAdmin reports whether owner is an administrator; false when unknown.
func (s *Service) isAdmin(ctx context.Context, owner domain.UserID, log *slog.Logger) bool {
	if s.admins == nil {
		return false
	}
	// The send is over; the answer must not depend on a shutdown that came
	// meanwhile.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), completeTimeout)
	defer cancel()
	admin, err := s.admins.IsAdmin(ctx, owner)
	if err != nil {
		log.ErrorContext(ctx, "cannot look up a contact owner's role", "error", err.Error())
		return false
	}
	return admin
}

// withStatus prefixes reason with "status <status>: " unless status is 0
// or reason already names it (the channels say "Discord answered 404 Not
// Found: …"), so a stored error never repeats the code.
func withStatus(status int, reason string) string {
	if status == 0 {
		return reason
	}
	code := strconv.Itoa(status)
	isDigit := func(r rune) bool { return r >= '0' && r <= '9' }
	if slices.Contains(strings.FieldsFunc(reason, func(r rune) bool { return !isDigit(r) }), code) {
		return reason
	}
	return "status " + code + ": " + reason
}

// backoff returns how long to wait after failed attempt number attempt
// (1-based): retryAfter if the target asked for it (capped at MaxBackoff),
// else the jittered exponential delay.
func (s *Service) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return min(retryAfter, MaxBackoff)
	}
	return jitter(retryDelay(attempt), s.randUint64)
}

// retryDelay is min(MaxBackoff, BaseBackoff * 2^(attempt-1)).
func retryDelay(attempt int) time.Duration {
	d := BaseBackoff
	for i := 1; i < attempt && d < MaxBackoff; i++ {
		d *= 2
	}
	return min(d, MaxBackoff)
}

// jitter returns a delay in [d/2, d], drawn with random; d itself if
// random fails.
func jitter(d time.Duration, random func() (uint64, bool)) time.Duration {
	half := d / 2
	r, ok := random()
	if !ok {
		return d
	}
	return half + time.Duration(r%uint64(d-half+1)) //nolint:gosec // d-half+1 is positive: no overflow, and the result is at most d
}

// randUint64 reads a random number from Deps.Random.
func (s *Service) randUint64() (uint64, bool) {
	var b [8]byte
	s.randMu.Lock()
	_, err := io.ReadFull(s.random, b[:])
	s.randMu.Unlock()
	if err != nil {
		return 0, false
	}
	return binary.LittleEndian.Uint64(b[:]), true
}

// Purge deletes the delivered and dead deliveries created more than
// Config.Retention ago and returns how many it deleted.
func (s *Service) Purge(ctx context.Context) (int, error) {
	n, err := s.repo.PurgeDeliveries(ctx, s.clock.Now().Add(-s.cfg.Retention))
	if err != nil {
		return 0, fmt.Errorf("purge deliveries: %w", err)
	}
	return n, nil
}

// List returns a page of the deliveries of actor's contacts matching f,
// newest first: f.OwnerID is forced to the actor, f.Limit 0 means
// DefaultPageSize and is capped at MaxPageSize, f.Before is the cursor of
// the previous page.
func (s *Service) List(ctx context.Context, actor domain.Actor, f DeliveryFilter) (Page, error) {
	if actor.UserID.IsZero() {
		return Page{Deliveries: []Delivery{}}, nil
	}
	for _, st := range f.Statuses {
		if !st.Valid() {
			return Page{}, fmt.Errorf("list deliveries: %w: unknown status %q", domain.ErrInvalidArgument, st)
		}
	}
	limit := f.Limit
	switch {
	case limit < 0:
		return Page{}, fmt.Errorf("list deliveries: %w: the limit must not be negative", domain.ErrInvalidArgument)
	case limit == 0:
		limit = DefaultPageSize
	case limit > MaxPageSize:
		limit = MaxPageSize
	}
	f.OwnerID, f.Limit = actor.UserID, limit+1
	ds, err := s.repo.ListDeliveries(ctx, f)
	if err != nil {
		return Page{}, fmt.Errorf("list deliveries: %w", err)
	}
	page := Page{Deliveries: ds}
	if len(ds) > limit {
		page.Deliveries = ds[:limit]
		page.Next = ds[limit-1].ID
	}
	return page, nil
}

// Retry queues one of actor's dead deliveries again, with its attempts
// reset, and wakes the deliverer. ErrNotFound for another user's delivery
// (admins included); ErrFailedPrecondition if it is not dead.
func (s *Service) Retry(ctx context.Context, actor domain.Actor, id domain.DeliveryID) (Delivery, error) {
	if id.IsZero() {
		return Delivery{}, fmt.Errorf("retry delivery: %w", domain.ErrInvalidID)
	}
	d, err := s.repo.GetDelivery(ctx, id)
	if err != nil {
		return Delivery{}, fmt.Errorf("retry delivery %s: %w", id, err)
	}
	if !actor.Owns(d.OwnerID) {
		return Delivery{}, fmt.Errorf("retry delivery %s: %w", id, domain.ErrNotFound)
	}
	stored, err := s.repo.RetryDelivery(ctx, id, s.clock.Now())
	if errors.Is(err, domain.ErrFailedPrecondition) {
		return Delivery{}, fmt.Errorf("%w: only a dead delivery can be retried", domain.ErrFailedPrecondition)
	}
	if err != nil {
		return Delivery{}, fmt.Errorf("retry delivery %s: %w", id, err)
	}
	s.log.InfoContext(ctx, "delivery queued again", "delivery_id", id.String(), "contact_id", d.ContactID.String())
	s.Wake()
	return stored, nil
}

// truncateRunes cuts s to at most n runes, ending in "…" when cut.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
