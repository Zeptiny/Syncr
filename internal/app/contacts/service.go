package contacts

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/descriptor"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/notify"
)

// The SendTest rate limit the composition root configures its Limiter with
// (platform/ratelimit): a burst of TestBurst tests per user, then one every
// TestEvery.
const (
	TestBurst = 3
	TestEvery = 10 * time.Second
)

// Config tunes the service. Zero fields take the DefaultConfig values.
type Config struct {
	// SendTimeout bounds one SendTest (notify.send_timeout).
	SendTimeout time.Duration
}

// DefaultConfig returns the default configuration.
func DefaultConfig() Config {
	return Config{SendTimeout: 15 * time.Second}
}

// Deps are the service's collaborators. All are required except Logger.
type Deps struct {
	Contacts Repository
	// Channels are the channel types contacts can have.
	Channels *notify.Registry
	Cipher   Cipher
	// Limiter limits SendTest per user (see TestBurst and TestEvery).
	Limiter Limiter
	Clock   Clock
	IDs     IDGenerator
	// Logger defaults to discarding. The service never logs an Input or a
	// configuration value.
	Logger *slog.Logger
}

// Service implements the contact use cases (see the package
// documentation). It is stateless apart from its dependencies and safe for
// concurrent use.
type Service struct {
	repo     Repository
	channels *notify.Registry
	cipher   Cipher
	limiter  Limiter
	clock    Clock
	ids      IDGenerator
	log      *slog.Logger
	cfg      Config
}

// New returns a Service.
func New(d Deps, cfg Config) (*Service, error) {
	if d.Contacts == nil || d.Channels == nil || d.Cipher == nil || d.Limiter == nil || d.Clock == nil || d.IDs == nil {
		return nil, errors.New("contacts: missing dependency")
	}
	cfg.SendTimeout = cmp.Or(cfg.SendTimeout, DefaultConfig().SendTimeout)
	if cfg.SendTimeout < 0 {
		return nil, fmt.Errorf("contacts: invalid config %+v: the send timeout must be positive", cfg)
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{
		repo: d.Contacts, channels: d.Channels, cipher: d.Cipher, limiter: d.Limiter,
		clock: d.Clock, ids: d.IDs, log: log, cfg: cfg,
	}, nil
}

// Create validates and stores a new contact of actor. Empty option and
// secret values count as unset.
func (s *Service) Create(ctx context.Context, actor domain.Actor, in Input) (Contact, error) {
	if actor.UserID.IsZero() {
		return Contact{}, fmt.Errorf("create contact: %w: contacts belong to a user account", domain.ErrPermissionDenied)
	}
	var v domain.Validation
	name := checkName(&v, in.Name)
	events := checkJobEvents(&v, in.JobEvents)
	ch, ok := s.newChannel(&v, in)
	if !ok {
		return Contact{}, fmt.Errorf("create contact: %w", v.Err())
	}
	cfg := descriptor.Values{Options: nonEmpty(in.Options), Secrets: nonEmpty(in.Secrets)}
	addConfigErrors(&v, ch.Validate(cfg.Clone()))
	if err := v.Err(); err != nil {
		return Contact{}, fmt.Errorf("create contact: %w", err)
	}

	id := s.ids.NewContactID()
	now := s.clock.Now()
	c := Contact{
		ID: id, OwnerID: actor.UserID, Name: name, Type: ch.ID(), Options: cfg.Options,
		Summary: ch.Summary(cfg.Clone()), Enabled: in.Enabled, JobEvents: events,
		CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	var err error
	if c.SecretsEnc, c.SecretKeys, err = s.seal(id, cfg.Secrets); err != nil {
		return Contact{}, fmt.Errorf("create contact: %w", err)
	}
	if err := s.repo.CreateContact(ctx, c); err != nil {
		return Contact{}, fmt.Errorf("create contact: %w", err)
	}
	s.log.InfoContext(ctx, "contact created", "contact_id", id.String(), "owner_id", actor.UserID.String(), "channel_type", c.Type)
	return c, nil
}

// newChannel checks the type and clear_secrets of a contact that is not
// stored yet (Create, or SendTest of unsaved values) and returns its
// channel; ok is false when the type is missing or unknown.
func (s *Service) newChannel(v *domain.Validation, in Input) (notify.Channel, bool) {
	ch, ok := s.channels.Get(in.Type)
	switch {
	case in.Type == "":
		v.Add("type", "is required")
	case !ok:
		v.Add("type", "is not a known channel type")
	}
	for i := range in.ClearSecrets {
		v.Add(fmt.Sprintf("clear_secrets[%d]", i), "is only used with a stored contact")
	}
	return ch, ok
}

// Get returns one of actor's contacts: ErrNotFound for another user's,
// admins included.
func (s *Service) Get(ctx context.Context, actor domain.Actor, id domain.ContactID) (Contact, error) {
	c, err := s.owned(ctx, actor, id)
	if err != nil {
		return Contact{}, fmt.Errorf("get contact: %w", err)
	}
	return c, nil
}

// owned returns contact id of actor, or ErrNotFound.
func (s *Service) owned(ctx context.Context, actor domain.Actor, id domain.ContactID) (Contact, error) {
	if id.IsZero() {
		return Contact{}, fmt.Errorf("contact: %w", domain.ErrInvalidID)
	}
	c, err := s.repo.GetContact(ctx, id)
	if err != nil {
		return Contact{}, fmt.Errorf("contact %s: %w", id, err)
	}
	if !actor.Owns(c.OwnerID) {
		return Contact{}, fmt.Errorf("contact %s: %w", id, domain.ErrNotFound)
	}
	return c, nil
}

// List returns actor's contacts matching f, by name; f.OwnerID is forced to
// the actor.
func (s *Service) List(ctx context.Context, actor domain.Actor, f ListFilter) ([]Contact, error) {
	if actor.UserID.IsZero() {
		return []Contact{}, nil
	}
	f.OwnerID = actor.UserID
	cs, err := s.repo.ListContacts(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("list contacts: %w", err)
	}
	return cs, nil
}

// Update changes one of actor's contacts. version is the one the caller
// read (ErrConflict if the contact changed since). The type cannot change
// (an empty in.Type means the contact's). Options are replaced; secrets are
// merged: a non-empty value replaces the stored one, an absent or empty one
// keeps it, and in.ClearSecrets removes optional ones. The merged
// configuration is validated by the channel, and its summary stored.
func (s *Service) Update(ctx context.Context, actor domain.Actor, id domain.ContactID, version int64, in Input) (Contact, error) {
	cur, err := s.owned(ctx, actor, id)
	if err != nil {
		return Contact{}, fmt.Errorf("update contact: %w", err)
	}
	if version != cur.Version {
		// Merging secrets onto another version than the caller's would keep
		// or drop secrets the caller never saw.
		return Contact{}, fmt.Errorf("update contact %s: at version %d, not %d: %w", id, cur.Version, version, domain.ErrConflict)
	}
	ch, err := s.storedChannel(cur)
	if err != nil {
		return Contact{}, fmt.Errorf("update contact %s: %w", id, err)
	}
	var v domain.Validation
	name := checkName(&v, in.Name)
	events := checkJobEvents(&v, in.JobEvents)
	cfg, err := s.merge(&v, cur, ch, in)
	if err != nil {
		return Contact{}, fmt.Errorf("update contact %s: %w", id, err)
	}
	addConfigErrors(&v, ch.Validate(cfg.Clone()))
	if err := v.Err(); err != nil {
		return Contact{}, fmt.Errorf("update contact %s: %w", id, err)
	}

	next := cur
	next.Name, next.Options, next.Summary = name, cfg.Options, ch.Summary(cfg.Clone())
	next.Enabled, next.JobEvents, next.UpdatedAt = in.Enabled, events, s.clock.Now()
	if next.SecretsEnc, next.SecretKeys, err = s.seal(id, cfg.Secrets); err != nil {
		return Contact{}, fmt.Errorf("update contact %s: %w", id, err)
	}
	stored, err := s.repo.UpdateContact(ctx, next)
	if err != nil {
		return Contact{}, fmt.Errorf("update contact %s: %w", id, err)
	}
	s.log.InfoContext(ctx, "contact updated", "contact_id", id.String(), "version", stored.Version)
	return stored, nil
}

// storedChannel returns the channel of a stored contact:
// ErrFailedPrecondition if its type is no longer available.
func (s *Service) storedChannel(c Contact) (notify.Channel, error) {
	ch, ok := s.channels.Get(c.Type)
	if !ok {
		return nil, fmt.Errorf("%w: channel type %q is no longer available", domain.ErrFailedPrecondition, c.Type)
	}
	return ch, nil
}

// merge returns the configuration of cur with in applied (see Update),
// adding problems with in to v. Stored secrets that cannot be read are
// ErrFailedPrecondition.
func (s *Service) merge(v *domain.Validation, cur Contact, ch notify.Channel, in Input) (descriptor.Values, error) {
	if in.Type != "" && in.Type != cur.Type {
		v.Add("type", "cannot be changed")
	}
	secrets, err := s.open(cur)
	if err != nil {
		// The cause names no secret (see open).
		return descriptor.Values{}, fmt.Errorf("%w: the contact's stored secrets cannot be read (%w); delete it and create it again",
			domain.ErrFailedPrecondition, err)
	}
	fields := map[string]descriptor.Field{}
	for _, f := range ch.Fields() {
		fields[f.Name] = f
	}
	for i, key := range in.ClearSecrets {
		path := fmt.Sprintf("clear_secrets[%d]", i)
		switch f, ok := fields[key]; {
		case !ok || !f.Secret:
			v.Add(path, "is not a secret field of this channel type")
		case f.Required:
			v.Add(path, "is required and cannot be cleared")
		case in.Secrets[key] != "":
			v.Add(path, "is also set in secrets")
		default:
			delete(secrets, key)
		}
	}
	maps.Copy(secrets, nonEmpty(in.Secrets))
	return descriptor.Values{Options: nonEmpty(in.Options), Secrets: secrets}, nil
}

// Delete deletes one of actor's contacts, with its schedule links and its
// delivery log.
func (s *Service) Delete(ctx context.Context, actor domain.Actor, id domain.ContactID) error {
	if _, err := s.owned(ctx, actor, id); err != nil {
		return fmt.Errorf("delete contact: %w", err)
	}
	if err := s.repo.DeleteContact(ctx, id); err != nil {
		return fmt.Errorf("delete contact %s: %w", id, err)
	}
	s.log.InfoContext(ctx, "contact deleted", "contact_id", id.String())
	return nil
}

// UsedBy returns the schedules linked to each of ids that is one of actor's
// contacts (others are left out), by name.
func (s *Service) UsedBy(ctx context.Context, actor domain.Actor, ids []domain.ContactID) (map[domain.ContactID][]ScheduleRef, error) {
	out := map[domain.ContactID][]ScheduleRef{}
	if len(ids) == 0 || actor.UserID.IsZero() {
		return out, nil
	}
	cs, err := s.repo.GetContacts(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("contact schedules: %w", err)
	}
	mine := make([]domain.ContactID, 0, len(cs))
	for _, c := range cs {
		if actor.Owns(c.OwnerID) {
			mine = append(mine, c.ID)
		}
	}
	if len(mine) == 0 {
		return out, nil
	}
	used, err := s.repo.ContactSchedules(ctx, mine)
	if err != nil {
		return nil, fmt.Errorf("contact schedules: %w", err)
	}
	return used, nil
}

// SendTest sends a test notification right away (not through the outbox)
// and reports the outcome. With id it tests one of actor's stored contacts
// (ErrNotFound otherwise), with in, if given, applied to it as Update would
// (options replaced, secrets merged), so an edit form can be tested before
// it is saved; with a zero id it tests in alone, which must name the type.
// The configuration is validated first (a *domain.ValidationError). Tests
// are rate limited per user (ErrRateLimited) and bounded by
// Config.SendTimeout. A failure of the channel is a TestResult with OK
// false, not an error; its Reason never contains a secret. Only
// administrators see what the destination or the network answered (see
// notify.SendError.Public): other users get notify.GenericReason, without
// status or latency, so test sends cannot probe the controller's network.
// The detail is logged.
func (s *Service) SendTest(ctx context.Context, actor domain.Actor, id domain.ContactID, in *Input) (TestResult, error) {
	if actor.UserID.IsZero() {
		return TestResult{}, fmt.Errorf("send test notification: %w: contacts belong to a user account", domain.ErrPermissionDenied)
	}
	ch, cfg, err := s.testConfig(ctx, actor, id, in)
	if err != nil {
		return TestResult{}, fmt.Errorf("send test notification: %w", err)
	}
	if !s.limiter.Allow(actor.UserID.String()) {
		return TestResult{}, fmt.Errorf("send test notification: %w", domain.ErrRateLimited)
	}

	now := s.clock.Now()
	msg := notify.Message{
		Version: notify.MessageVersion, ID: s.ids.NewDeliveryID().String(),
		Event: domain.EventTest, OccurredAt: now, Test: true,
	}
	msg.Title = notify.Title(msg)
	sendCtx, cancel := context.WithTimeout(ctx, s.cfg.SendTimeout)
	defer cancel()
	err = ch.Send(sendCtx, cfg, msg)
	res := TestResult{OK: err == nil, Latency: max(s.clock.Now().Sub(now), 0)}
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			// The caller gave up; there is nobody to show a result to.
			return TestResult{}, fmt.Errorf("send test notification: %w", cerr)
		}
		detail := failure(err, errors.Is(sendCtx.Err(), context.DeadlineExceeded), s.cfg.SendTimeout)
		// A safety net: the channel redacted the reply already.
		detail.reason = notify.NewRedactor(cfg).Redact(detail.reason)
		res.Reason, res.Status = detail.reason, detail.status
		if !actor.IsAdmin() && !detail.public {
			// Anyone may create a contact, so a detailed answer (refused,
			// filtered, TLS or HTTP details, how long it took) would let
			// any user scan the networks the controller can reach.
			// Administrators control the network policy and see it.
			res.Reason, res.Status, res.Latency = notify.GenericReason, 0, 0
		}
		s.log.InfoContext(ctx, "test notification sent", "contact_id", optID(id), "owner_id", actor.UserID.String(),
			"channel_type", ch.ID(), "ok", false, "status", detail.status, "reason", detail.reason)
		return res, nil
	}
	s.log.InfoContext(ctx, "test notification sent", "contact_id", optID(id), "owner_id", actor.UserID.String(),
		"channel_type", ch.ID(), "ok", true)
	return res, nil
}

// testConfig returns the channel and configuration SendTest sends with.
func (s *Service) testConfig(ctx context.Context, actor domain.Actor, id domain.ContactID, in *Input) (notify.Channel, descriptor.Values, error) {
	var v domain.Validation
	var ch notify.Channel
	var cfg descriptor.Values
	switch {
	case id.IsZero() && in == nil:
		return nil, descriptor.Values{}, fmt.Errorf("%w: a stored contact or a configuration to test is required", domain.ErrInvalidArgument)
	case id.IsZero():
		var ok bool
		if ch, ok = s.newChannel(&v, *in); !ok {
			return nil, descriptor.Values{}, v.Err()
		}
		cfg = descriptor.Values{Options: nonEmpty(in.Options), Secrets: nonEmpty(in.Secrets)}
	default:
		cur, err := s.owned(ctx, actor, id)
		if err != nil {
			return nil, descriptor.Values{}, err
		}
		if ch, err = s.storedChannel(cur); err != nil {
			return nil, descriptor.Values{}, err
		}
		if in == nil {
			secrets, err := s.open(cur)
			if err != nil {
				return nil, descriptor.Values{}, fmt.Errorf("%w: the contact's stored secrets cannot be read (%w)", domain.ErrFailedPrecondition, err)
			}
			cfg = descriptor.Values{Options: maps.Clone(cur.Options), Secrets: secrets}
		} else if cfg, err = s.merge(&v, cur, ch, *in); err != nil {
			return nil, descriptor.Values{}, err
		}
	}
	addConfigErrors(&v, ch.Validate(cfg.Clone()))
	if err := v.Err(); err != nil {
		return nil, descriptor.Values{}, err
	}
	return ch, cfg, nil
}

// failure describes a failed Send for people: the *notify.SendError's
// sanitized reason and status, or a generic text for anything else (whose
// message could quote the configuration). timedOut says whether the send's
// deadline passed.
func failure(err error, timedOut bool, timeout time.Duration) failed {
	var serr *notify.SendError
	if errors.As(err, &serr) {
		reason := serr.Reason
		if reason == "" {
			reason = "the target rejected the notification"
		}
		return failed{reason: reason, status: serr.Status, public: serr.Public}
	}
	if timedOut {
		return failed{reason: fmt.Sprintf("no answer within %s", timeout)}
	}
	return failed{reason: "the notification could not be sent"}
}

// failed is a failed send as administrators see it. public says that
// everybody may be shown it (notify.SendError.Public).
type failed struct {
	reason string
	status int
	public bool
}

// Usable returns the names of ids, which must all be contacts of owner:
// ErrNotFound otherwise. It implements schedules.ContactChecker.
func (s *Service) Usable(ctx context.Context, owner domain.UserID, ids []domain.ContactID) (map[domain.ContactID]string, error) {
	out := make(map[domain.ContactID]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	cs, err := s.repo.GetContacts(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("check contacts: %w", err)
	}
	for _, c := range cs {
		if !owner.IsZero() && c.OwnerID == owner {
			out[c.ID] = c.Name
		}
	}
	for _, id := range ids {
		if _, ok := out[id]; !ok {
			return nil, fmt.Errorf("check contacts: contact %s: %w", id, domain.ErrNotFound)
		}
	}
	return out, nil
}

// Names returns display references of ids, of any owner; missing ones are
// omitted. It is for listings of objects that refer to contacts (a
// schedule's links), never for using them.
func (s *Service) Names(ctx context.Context, ids []domain.ContactID) (map[domain.ContactID]Ref, error) {
	out := make(map[domain.ContactID]Ref, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	cs, err := s.repo.GetContacts(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("contact names: %w", err)
	}
	for _, c := range cs {
		out[c.ID] = Ref{ID: c.ID, Name: c.Name, Type: c.Type}
	}
	return out, nil
}

// Resolve returns contact id (of any owner: the deliverer sends to the
// contacts planned for) with its decrypted configuration. ErrNotFound if it
// was deleted; ErrFailedPrecondition if its secrets cannot be decrypted or
// decoded (a lost or rotated-out secrets key, corrupt data), which retrying
// cannot fix. The result holds secrets: it must never be logged or stored.
func (s *Service) Resolve(ctx context.Context, id domain.ContactID) (Resolved, error) {
	c, err := s.repo.GetContact(ctx, id)
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve contact %s: %w", id, err)
	}
	secrets, err := s.open(c)
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve contact %s: %w: its credentials cannot be read (%w)", id, domain.ErrFailedPrecondition, err)
	}
	return Resolved{Contact: c, Values: descriptor.Values{Options: maps.Clone(c.Options), Secrets: secrets}}, nil
}

// ResolveChannel is Resolve as notifications.ContactResolver needs it: the
// contact's channel type and decrypted configuration.
func (s *Service) ResolveChannel(ctx context.Context, id domain.ContactID) (string, descriptor.Values, error) {
	r, err := s.Resolve(ctx, id)
	if err != nil {
		return "", descriptor.Values{}, err
	}
	return r.Contact.Type, r.Values, nil
}

// checkName validates a contact name and returns it without surrounding
// white space.
func checkName(v *domain.Validation, name string) string {
	name = strings.TrimSpace(name)
	switch n := utf8.RuneCountInString(name); {
	case !utf8.ValidString(name):
		v.Add("name", "is not valid UTF-8")
	case n == 0:
		v.Add("name", "is required")
	case n > MaxNameLen:
		v.Add("name", fmt.Sprintf("must be at most %d characters", MaxNameLen))
	case strings.ContainsFunc(name, unicode.IsControl):
		v.Add("name", "must not contain control characters")
	}
	return name
}

// checkJobEvents validates the events of manual jobs a contact subscribes
// to and returns them normalized (duplicates are dropped).
func checkJobEvents(v *domain.Validation, events domain.EventSet) domain.EventSet {
	for i, e := range events {
		if !slices.Contains(domain.JobEvents(), e) {
			v.Add(fmt.Sprintf("job_events[%d]", i), "must be succeeded, failed or interrupted")
		}
	}
	return events.Normalize()
}

// addConfigErrors adds the field errors of a Channel.Validate result to v.
// Any other error is reported as a configuration error without its text,
// which could contain values.
func addConfigErrors(v *domain.Validation, err error) {
	var verr *domain.ValidationError
	switch {
	case err == nil:
	case errors.As(err, &verr):
		v.AddAll("", verr.Fields)
	default:
		v.Add("options", "the configuration is not valid")
	}
}

// nonEmpty returns the entries of m with a non-empty value, as a new map.
func nonEmpty(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// seal encrypts secrets as one JSON object bound to contact id; nil and no
// keys when there are none.
func (s *Service) seal(id domain.ContactID, secrets map[string]string) ([]byte, []string, error) {
	if len(secrets) == 0 {
		return nil, nil, nil
	}
	plain, err := json.Marshal(secrets)
	if err != nil {
		return nil, nil, fmt.Errorf("encode secrets: %w", err)
	}
	enc, err := s.cipher.Encrypt(plain, SecretsAD(id))
	clear(plain)
	if err != nil {
		return nil, nil, fmt.Errorf("encrypt secrets: %w", err)
	}
	return enc, slices.Sorted(maps.Keys(secrets)), nil
}

// open decrypts c's secrets into a new map (empty when it has none). Its
// errors never contain plaintext.
func (s *Service) open(c Contact) (map[string]string, error) {
	out := map[string]string{}
	if len(c.SecretsEnc) == 0 {
		return out, nil
	}
	plain, err := s.cipher.Decrypt(c.SecretsEnc, SecretsAD(c.ID))
	if err != nil {
		return nil, errors.New("decrypt secrets failed")
	}
	defer clear(plain)
	if err := json.Unmarshal(plain, &out); err != nil {
		// The JSON error may quote the plaintext.
		return nil, errors.New("decode secrets: not a JSON object of strings")
	}
	return out, nil
}

// optID is the log value of an optional contact ID.
func optID(id domain.ContactID) string {
	if id.IsZero() {
		return ""
	}
	return id.String()
}
