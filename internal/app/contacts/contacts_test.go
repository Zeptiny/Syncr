package contacts_test

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/adapters/memstore"
	"github.com/Zeptiny/Syncr/internal/adapters/secrets"
	"github.com/Zeptiny/Syncr/internal/app/contacts"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/descriptor"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/notify"
	"github.com/Zeptiny/Syncr/internal/notify/notifytest"
	"github.com/Zeptiny/Syncr/internal/platform/clock"
)

var t0 = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// The secret values of the tests: distinctive, so a leak is found.
const (
	secretURL   = "https://hooks.example/secret-url-token-0001"
	secretToken = "secret-token-value-0002"
)

// fakeChannel is a channel type of the tests: a required secret URL, an
// optional secret token, a required room and an advanced level.
type fakeChannel struct {
	mu   sync.Mutex
	sent []sent
	// fail is what Send returns; block makes Send wait for its context.
	fail  error
	block bool
}

type sent struct {
	cfg descriptor.Values
	msg notify.Message
}

func (*fakeChannel) ID() string          { return "fake" }
func (*fakeChannel) Label() string       { return "Fake" }
func (*fakeChannel) Description() string { return "A channel type of the tests." }

func (*fakeChannel) Fields() []descriptor.Field {
	return []descriptor.Field{
		{Name: "url", Label: "URL", Kind: descriptor.KindString, Required: true, Secret: true},
		{Name: "token", Label: "Token", Kind: descriptor.KindString, Secret: true},
		{Name: "room", Label: "Room", Kind: descriptor.KindString, Required: true},
		{Name: "level", Label: "Level", Kind: descriptor.KindEnum, Default: "info", Advanced: true,
			Choices: []descriptor.Choice{{Value: "info", Label: "Info"}, {Value: "loud", Label: "Loud"}}},
	}
}

func (c *fakeChannel) Validate(cfg descriptor.Values) error {
	var v domain.Validation
	v.AddAll("", descriptor.CheckFields(c.Fields(), cfg))
	if u := cfg.Secrets["url"]; u != "" && !strings.HasPrefix(u, "https://") {
		v.Add("secrets.url", "must be an https URL")
	}
	return v.Err()
}

func (*fakeChannel) Summary(cfg descriptor.Values) string { return "room " + cfg.Options["room"] }

func (*fakeChannel) Examples() []descriptor.Values {
	return []descriptor.Values{{
		Options: map[string]string{"room": "ops", "level": "loud"},
		Secrets: map[string]string{"url": secretURL, "token": secretToken},
	}}
}

func (c *fakeChannel) Send(ctx context.Context, cfg descriptor.Values, msg notify.Message) error {
	c.mu.Lock()
	c.sent = append(c.sent, sent{cfg: cfg.Clone(), msg: msg})
	fail, block := c.fail, c.block
	c.mu.Unlock()
	if block {
		<-ctx.Done()
	}
	if ctx.Err() != nil {
		return notify.Temporary(0, "the send was cancelled", 0)
	}
	return fail
}

func (c *fakeChannel) calls() []sent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.sent)
}

func TestFakeChannelContract(t *testing.T) {
	t.Parallel()
	notifytest.Run(t, &fakeChannel{fail: notify.Permanent(404, "unknown room")},
		notifytest.WithFailingTarget((&fakeChannel{}).Examples()[0]))
}

// fakeLimiter allows a fixed number of calls per key.
type fakeLimiter struct {
	mu    sync.Mutex
	left  int
	calls []string
}

func (l *fakeLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, key)
	if l.left <= 0 {
		return false
	}
	l.left--
	return true
}

// flakyCipher is the real keyring, which can be made to fail decrypting.
type flakyCipher struct {
	*secrets.Keyring
	failDecrypt bool
}

func (c *flakyCipher) Decrypt(ct, ad []byte) ([]byte, error) {
	if c.failDecrypt {
		return nil, errors.New("decrypt failed")
	}
	return c.Keyring.Decrypt(ct, ad)
}

type env struct {
	svc     *contacts.Service
	store   *memstore.Store
	clock   *clock.Fake
	cipher  *flakyCipher
	ch      *fakeChannel
	limiter *fakeLimiter
	logs    *bytes.Buffer
	ids     *idgen.Sequential
	alice   domain.Actor
	bob     domain.Actor
	admin   domain.Actor
}

func newEnv(t *testing.T) *env {
	t.Helper()
	key := secrets.Key{ID: 1, Bytes: make([]byte, secrets.KeySize)}
	if _, err := crand.Read(key.Bytes); err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		store: memstore.New(), clock: clock.NewFake(t0), cipher: &flakyCipher{Keyring: ring}, ch: &fakeChannel{},
		limiter: &fakeLimiter{left: 100}, logs: &bytes.Buffer{}, ids: &idgen.Sequential{},
	}
	reg, err := notify.NewRegistry(e.ch)
	if err != nil {
		t.Fatal(err)
	}
	for i, a := range []*domain.Actor{&e.alice, &e.bob, &e.admin} {
		role := domain.RoleUser
		if i == 2 {
			role = domain.RoleAdmin
		}
		*a = domain.Actor{UserID: e.ids.NewUserID(), Role: role}
		u := users.User{ID: a.UserID, Username: []string{"alice", "bob", "admin"}[i], Role: role, PasswordHash: "x",
			CreatedAt: t0, UpdatedAt: t0, PasswordChangedAt: t0, Version: 1}
		if err := e.store.CreateUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	e.svc, err = contacts.New(contacts.Deps{
		Contacts: e.store, Channels: reg, Cipher: e.cipher, Limiter: e.limiter, Clock: e.clock, IDs: e.ids,
		Logger: slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}, contacts.Config{SendTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func fakeInput(name string) contacts.Input {
	return contacts.Input{
		Name: name, Type: "fake", Enabled: true,
		Options: map[string]string{"room": "ops", "level": "loud"},
		Secrets: map[string]string{"url": secretURL, "token": secretToken},
	}
}

func (e *env) mustCreate(t *testing.T, a domain.Actor, in contacts.Input) contacts.Contact {
	t.Helper()
	c, err := e.svc.Create(context.Background(), a, in)
	if err != nil {
		t.Fatalf("Create(%s): %v", in.Name, err)
	}
	return c
}

// secretsOf decrypts c's stored secrets.
func (e *env) secretsOf(t *testing.T, id domain.ContactID) map[string]string {
	t.Helper()
	c, err := e.store.GetContact(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.SecretsEnc) == 0 {
		return map[string]string{}
	}
	plain, err := e.cipher.Keyring.Decrypt(c.SecretsEnc, contacts.SecretsAD(id))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	if err := json.Unmarshal(plain, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// noSecrets fails if text contains a test secret.
func noSecrets(t *testing.T, what, text string) {
	t.Helper()
	for _, s := range []string{secretURL, secretToken, "secret-url-token", "new-secret"} {
		if strings.Contains(text, s) {
			t.Errorf("%s contains the secret %q: %s", what, s, text)
		}
	}
}

func fields(t *testing.T, err error) []string {
	t.Helper()
	var verr *domain.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("err = %v, want a *domain.ValidationError", err)
	}
	var out []string
	for _, f := range verr.Fields {
		out = append(out, f.Field)
	}
	return out
}

func wantFields(t *testing.T, what string, err error, want ...string) {
	t.Helper()
	got := fields(t, err)
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("%s: fields %v, want %q", what, got, w)
		}
	}
}

func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: err = %v, want %v", what, err, want)
	}
}

func TestNewChecksDeps(t *testing.T) {
	t.Parallel()
	if _, err := contacts.New(contacts.Deps{}, contacts.Config{}); err == nil {
		t.Error("New accepted missing dependencies")
	}
}

func TestCreateGetList(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	in := fakeInput("  Ops room  ")
	in.Options["empty"] = "" // empty values count as unset
	in.JobEvents = domain.EventSet{domain.EventInterrupted, domain.EventFailed, domain.EventFailed}
	c := e.mustCreate(t, e.alice, in)
	if c.Name != "Ops room" || c.Type != "fake" || c.OwnerID != e.alice.UserID || c.Summary != "room ops" || !c.Enabled ||
		!maps.Equal(c.Options, map[string]string{"room": "ops", "level": "loud"}) ||
		!slices.Equal(c.SecretKeys, []string{"token", "url"}) || c.Version != 1 || !c.CreatedAt.Equal(t0) ||
		!slices.Equal(c.JobEvents, domain.EventSet{domain.EventFailed, domain.EventInterrupted}) {
		t.Errorf("Create = %+v", c)
	}
	if bytes.Contains(c.SecretsEnc, []byte("secret-url-token")) {
		t.Error("secrets are stored in plain text")
	}
	if got := e.secretsOf(t, c.ID); got["url"] != secretURL || got["token"] != secretToken {
		t.Errorf("stored secrets = %v", got)
	}

	got, err := e.svc.Get(ctx, e.alice, c.ID)
	if err != nil || got.ID != c.ID {
		t.Errorf("Get = (%+v, %v)", got, err)
	}
	// Strictly private, admins included.
	for _, a := range []domain.Actor{e.bob, e.admin, {}} {
		_, err := e.svc.Get(ctx, a, c.ID)
		wantErr(t, "Get(other user)", err, domain.ErrNotFound)
	}
	_, err = e.svc.Get(ctx, e.alice, domain.ContactID{})
	wantErr(t, "Get(zero id)", err, domain.ErrInvalidID)

	e.mustCreate(t, e.bob, fakeInput("Ops room")) // names are per owner
	e.mustCreate(t, e.alice, fakeInput("another"))
	_, err = e.svc.Create(ctx, e.alice, fakeInput("OPS ROOM"))
	wantErr(t, "Create(name taken)", err, domain.ErrAlreadyExists)
	mine, err := e.svc.List(ctx, e.alice, contacts.ListFilter{OwnerID: e.bob.UserID})
	if err != nil || len(mine) != 2 || mine[0].Name != "another" || mine[1].Name != "Ops room" {
		t.Errorf("List = (%+v, %v), want alice's by name", mine, err)
	}
	if none, _ := e.svc.List(ctx, e.alice, contacts.ListFilter{Type: "other"}); len(none) != 0 {
		t.Errorf("List(type) = %+v", none)
	}
	if none, err := e.svc.List(ctx, domain.Actor{}, contacts.ListFilter{}); err != nil || len(none) != 0 {
		t.Errorf("List(no user) = (%+v, %v)", none, err)
	}
	_, err = e.svc.Create(ctx, domain.Actor{Role: domain.RoleAdmin}, fakeInput("x"))
	wantErr(t, "Create(no user)", err, domain.ErrPermissionDenied)
	noSecrets(t, "the log", e.logs.String())
}

func TestCreateValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	for _, tt := range []struct {
		name   string
		mutate func(*contacts.Input)
		want   []string
	}{
		{"no name", func(in *contacts.Input) { in.Name = " " }, []string{"name"}},
		{"long name", func(in *contacts.Input) { in.Name = strings.Repeat("x", 65) }, []string{"name"}},
		{"control in name", func(in *contacts.Input) { in.Name = "a\nb" }, []string{"name"}},
		{"no type", func(in *contacts.Input) { in.Type = "" }, []string{"type"}},
		{"unknown type", func(in *contacts.Input) { in.Type = "pager" }, []string{"type"}},
		{"clear on create", func(in *contacts.Input) { in.ClearSecrets = []string{"token"} }, []string{"clear_secrets[0]"}},
		{"missed job event", func(in *contacts.Input) { in.JobEvents = domain.EventSet{domain.EventFailed, domain.EventMissed} }, []string{"job_events[1]"}},
		{"test job event", func(in *contacts.Input) { in.JobEvents = domain.EventSet{domain.EventTest} }, []string{"job_events[0]"}},
		{"missing required", func(in *contacts.Input) { delete(in.Options, "room"); delete(in.Secrets, "url") }, []string{"options.room", "secrets.url"}},
		{"secret in options", func(in *contacts.Input) { in.Options["token"] = "x" }, []string{"options.token"}},
		{"channel rule", func(in *contacts.Input) { in.Secrets["url"] = "http://plain.example/secret-url-token" }, []string{"secrets.url"}},
	} {
		in := fakeInput("ok")
		tt.mutate(&in)
		_, err := e.svc.Create(ctx, e.alice, in)
		wantFields(t, tt.name, err, tt.want...)
		if err != nil {
			noSecrets(t, tt.name+": error", err.Error())
		}
	}
	if cs, _ := e.store.ListContacts(ctx, contacts.ListFilter{}); len(cs) != 0 {
		t.Errorf("invalid contacts were stored: %+v", cs)
	}
}

func TestUpdate(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	c := e.mustCreate(t, e.alice, fakeInput("Ops"))
	e.clock.Advance(time.Minute)

	// Options are replaced, absent secrets kept, new ones set.
	in := contacts.Input{
		Name: "Ops 2", Options: map[string]string{"room": "infra"}, Secrets: map[string]string{"token": "new-secret-token"},
		JobEvents: domain.EventSet{domain.EventSucceeded},
	}
	got, err := e.svc.Update(ctx, e.alice, c.ID, c.Version, in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Ops 2" || got.Version != 2 || got.Summary != "room infra" || got.Enabled ||
		!maps.Equal(got.Options, map[string]string{"room": "infra"}) || !got.UpdatedAt.Equal(t0.Add(time.Minute)) ||
		!slices.Equal(got.JobEvents, domain.EventSet{domain.EventSucceeded}) || got.Type != "fake" {
		t.Errorf("Update = %+v", got)
	}
	if s := e.secretsOf(t, c.ID); s["url"] != secretURL || s["token"] != "new-secret-token" {
		t.Errorf("secrets after the update = %v", s)
	}

	// Clearing an optional secret.
	got, err = e.svc.Update(ctx, e.alice, c.ID, got.Version, contacts.Input{
		Name: "Ops 2", Type: "fake", Options: map[string]string{"room": "infra"}, ClearSecrets: []string{"token"},
	})
	if err != nil || !slices.Equal(got.SecretKeys, []string{"url"}) {
		t.Fatalf("Update(clear) = (%+v, %v)", got, err)
	}
	if s := e.secretsOf(t, c.ID); len(s) != 1 || s["url"] != secretURL {
		t.Errorf("secrets after clearing = %v", s)
	}

	base := contacts.Input{Name: "Ops 2", Options: map[string]string{"room": "infra"}}
	for _, tt := range []struct {
		name   string
		mutate func(*contacts.Input)
		want   string
	}{
		{"type change", func(in *contacts.Input) { in.Type = "other" }, "type"},
		{"clear required", func(in *contacts.Input) { in.ClearSecrets = []string{"url"} }, "clear_secrets[0]"},
		{"clear option", func(in *contacts.Input) { in.ClearSecrets = []string{"room"} }, "clear_secrets[0]"},
		{"clear and set", func(in *contacts.Input) {
			in.ClearSecrets, in.Secrets = []string{"token"}, map[string]string{"token": "new-secret"}
		}, "clear_secrets[0]"},
		{"invalid merged", func(in *contacts.Input) { in.Options = nil }, "options.room"},
		{"bad events", func(in *contacts.Input) { in.JobEvents = domain.EventSet{"bogus"} }, "job_events[0]"},
	} {
		in := base
		tt.mutate(&in)
		_, err := e.svc.Update(ctx, e.alice, c.ID, got.Version, in)
		wantFields(t, tt.name, err, tt.want)
	}

	_, err = e.svc.Update(ctx, e.alice, c.ID, 1, base)
	wantErr(t, "Update(stale version)", err, domain.ErrConflict)
	_, err = e.svc.Update(ctx, e.bob, c.ID, got.Version, base)
	wantErr(t, "Update(other user)", err, domain.ErrNotFound)
	e.mustCreate(t, e.alice, fakeInput("Taken"))
	taken := base
	taken.Name = "taken"
	_, err = e.svc.Update(ctx, e.alice, c.ID, got.Version, taken)
	wantErr(t, "Update(name taken)", err, domain.ErrAlreadyExists)

	// Unreadable stored secrets cannot be merged.
	e.cipher.failDecrypt = true
	_, err = e.svc.Update(ctx, e.alice, c.ID, got.Version, base)
	wantErr(t, "Update(undecryptable)", err, domain.ErrFailedPrecondition)
	noSecrets(t, "the log", e.logs.String())
}

func TestDelete(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	c := e.mustCreate(t, e.alice, fakeInput("Ops"))
	wantErr(t, "Delete(other user)", e.svc.Delete(ctx, e.bob, c.ID), domain.ErrNotFound)
	wantErr(t, "Delete(admin)", e.svc.Delete(ctx, e.admin, c.ID), domain.ErrNotFound)
	if err := e.svc.Delete(ctx, e.alice, c.ID); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.Get(ctx, e.alice, c.ID)
	wantErr(t, "Get(deleted)", err, domain.ErrNotFound)
	wantErr(t, "Delete(again)", e.svc.Delete(ctx, e.alice, c.ID), domain.ErrNotFound)
}

func TestUsedByUsableNames(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	c1, c2 := e.mustCreate(t, e.alice, fakeInput("one")), e.mustCreate(t, e.alice, fakeInput("two"))
	theirs := e.mustCreate(t, e.bob, fakeInput("theirs"))

	w := workers.Worker{ID: e.ids.NewWorkerID(), Name: "w", Mode: domain.WorkerActive, CreatedAt: t0}
	if err := e.store.CreateWorker(ctx, w); err != nil {
		t.Fatal(err)
	}
	link := func(ids ...domain.ContactID) []schedules.ContactLink {
		var out []schedules.ContactLink
		for _, id := range ids {
			out = append(out, schedules.ContactLink{ContactID: id, Events: domain.EventSet{domain.EventFailed}})
		}
		return out
	}
	mk := func(owner domain.UserID, name string, ids ...domain.ContactID) schedules.Schedule {
		sc := schedules.Schedule{
			ID: e.ids.NewScheduleID(), OwnerID: owner, Name: name, Cron: "0 * * * *", Timezone: "UTC",
			Kind: "rclone_transfer", Definition: []byte(`{}`), Target: schedules.Target{WorkerID: w.ID},
			Missed: schedules.MissedPolicy{Action: schedules.MissedRunLate}, Overlap: schedules.OverlapSkip,
			Contacts: link(ids...), CreatedAt: t0, UpdatedAt: t0, Version: 1,
		}
		if err := e.store.CreateSchedule(ctx, sc); err != nil {
			t.Fatal(err)
		}
		return sc
	}
	nightly := mk(e.alice.UserID, "Nightly", c1.ID, c2.ID)
	archive := mk(e.alice.UserID, "archive", c1.ID)
	mk(e.bob.UserID, "Bob's", theirs.ID)

	used, err := e.svc.UsedBy(ctx, e.alice, []domain.ContactID{c1.ID, c2.ID, theirs.ID})
	if err != nil || len(used) != 2 || len(used[c1.ID]) != 2 || used[c1.ID][0].ID != archive.ID ||
		len(used[c2.ID]) != 1 || used[c2.ID][0].Name != "Nightly" || used[c2.ID][0].ID != nightly.ID {
		t.Errorf("UsedBy = (%+v, %v), want alice's contacts only", used, err)
	}
	if used, _ := e.svc.UsedBy(ctx, e.bob, []domain.ContactID{c1.ID}); len(used) != 0 {
		t.Errorf("UsedBy(other user) = %+v", used)
	}

	names, err := e.svc.Usable(ctx, e.alice.UserID, []domain.ContactID{c1.ID, c2.ID})
	if err != nil || names[c1.ID] != "one" || names[c2.ID] != "two" {
		t.Errorf("Usable = (%v, %v)", names, err)
	}
	for name, ids := range map[string][]domain.ContactID{
		"other user's": {c1.ID, theirs.ID},
		"missing":      {e.ids.NewContactID()},
	} {
		_, err := e.svc.Usable(ctx, e.alice.UserID, ids)
		wantErr(t, "Usable("+name+")", err, domain.ErrNotFound)
	}
	_, err = e.svc.Usable(ctx, domain.UserID{}, []domain.ContactID{c1.ID})
	wantErr(t, "Usable(no owner)", err, domain.ErrNotFound)
	if names, err := e.svc.Usable(ctx, e.alice.UserID, nil); err != nil || len(names) != 0 {
		t.Errorf("Usable(none) = (%v, %v)", names, err)
	}

	refs, err := e.svc.Names(ctx, []domain.ContactID{c1.ID, theirs.ID, e.ids.NewContactID()})
	if err != nil || len(refs) != 2 || refs[theirs.ID].Name != "theirs" || refs[c1.ID].Type != "fake" {
		t.Errorf("Names = (%+v, %v)", refs, err)
	}
}

func TestResolve(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	c := e.mustCreate(t, e.alice, fakeInput("Ops"))
	typ, cfg, err := e.svc.ResolveChannel(ctx, c.ID)
	if err != nil || typ != "fake" || cfg.Options["room"] != "ops" || cfg.Secrets["url"] != secretURL || cfg.Secrets["token"] != secretToken {
		t.Errorf("ResolveChannel = (%q, %+v, %v)", typ, cfg, err)
	}
	r, err := e.svc.Resolve(ctx, c.ID)
	if err != nil || r.Contact.ID != c.ID {
		t.Errorf("Resolve = (%+v, %v)", r.Contact, err)
	}
	r.Values.Options["room"] = "changed"
	if again, _ := e.svc.Get(ctx, e.alice, c.ID); again.Options["room"] != "ops" {
		t.Error("Resolve returns the stored options")
	}
	_, _, err = e.svc.ResolveChannel(ctx, e.ids.NewContactID())
	wantErr(t, "ResolveChannel(missing)", err, domain.ErrNotFound)
	e.cipher.failDecrypt = true
	_, _, err = e.svc.ResolveChannel(ctx, c.ID)
	wantErr(t, "ResolveChannel(undecryptable)", err, domain.ErrFailedPrecondition)
	noSecrets(t, "the error", err.Error())
}

func TestSendTest(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	c := e.mustCreate(t, e.alice, fakeInput("Ops"))

	// The stored contact.
	res, err := e.svc.SendTest(ctx, e.alice, c.ID, nil)
	if err != nil || !res.OK || res.Reason != "" {
		t.Fatalf("SendTest(stored) = (%+v, %v)", res, err)
	}
	calls := e.ch.calls()
	if len(calls) != 1 {
		t.Fatalf("sends = %d", len(calls))
	}
	m := calls[0].msg
	if !m.Test || m.Event != domain.EventTest || m.ID == "" || m.Version != notify.MessageVersion || m.Title == "" ||
		!m.OccurredAt.Equal(t0) || m.Job != nil {
		t.Errorf("test message = %+v", m)
	}
	if calls[0].cfg.Secrets["url"] != secretURL || calls[0].cfg.Options["room"] != "ops" {
		t.Errorf("sent with %+v", calls[0].cfg)
	}

	// Unsaved edits of the stored contact: options replaced, secrets merged.
	_, err = e.svc.SendTest(ctx, e.alice, c.ID, &contacts.Input{
		Options: map[string]string{"room": "draft"}, ClearSecrets: []string{"token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg := e.ch.calls()[1].cfg; cfg.Options["room"] != "draft" || cfg.Secrets["url"] != secretURL || cfg.Secrets["token"] != "" {
		t.Errorf("merged test configuration = %+v", cfg)
	}
	if got, _ := e.svc.Get(ctx, e.alice, c.ID); got.Options["room"] != "ops" || got.Version != 1 {
		t.Errorf("SendTest changed the contact: %+v", got)
	}

	// Unsaved values alone.
	in := fakeInput("")
	in.Options["room"] = "new"
	if res, err := e.svc.SendTest(ctx, e.alice, domain.ContactID{}, &in); err != nil || !res.OK {
		t.Errorf("SendTest(unsaved) = (%+v, %v)", res, err)
	}
	if cfg := e.ch.calls()[2].cfg; cfg.Options["room"] != "new" {
		t.Errorf("unsaved test configuration = %+v", cfg)
	}

	// Invalid requests send nothing.
	before := len(e.ch.calls())
	_, err = e.svc.SendTest(ctx, e.alice, domain.ContactID{}, nil)
	wantErr(t, "SendTest(nothing)", err, domain.ErrInvalidArgument)
	bad := fakeInput("")
	bad.Secrets["url"] = "http://not-https.example/new-secret"
	_, err = e.svc.SendTest(ctx, e.alice, domain.ContactID{}, &bad)
	wantFields(t, "SendTest(invalid)", err, "secrets.url")
	noSecrets(t, "the validation error", err.Error())
	_, err = e.svc.SendTest(ctx, e.alice, c.ID, &contacts.Input{Options: map[string]string{}})
	wantFields(t, "SendTest(invalid merge)", err, "options.room")
	_, err = e.svc.SendTest(ctx, e.alice, domain.ContactID{}, &contacts.Input{Type: "pager"})
	wantFields(t, "SendTest(unknown type)", err, "type")
	for _, a := range []domain.Actor{e.bob, e.admin} {
		_, err := e.svc.SendTest(ctx, a, c.ID, nil)
		wantErr(t, "SendTest(other user)", err, domain.ErrNotFound)
	}
	_, err = e.svc.SendTest(ctx, domain.Actor{}, c.ID, nil)
	wantErr(t, "SendTest(no user)", err, domain.ErrPermissionDenied)
	if got := len(e.ch.calls()); got != before {
		t.Errorf("invalid tests sent %d notifications", got-before)
	}
	noSecrets(t, "the log", e.logs.String())
}

func TestSendTestFailures(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	c := e.mustCreate(t, e.alice, fakeInput("Ops"))

	e.ch.fail = notify.Permanent(404, "unknown room at <url>")
	ac := e.mustCreate(t, e.admin, fakeInput("Admin ops"))
	res, err := e.svc.SendTest(ctx, e.admin, ac.ID, nil)
	if err != nil || res.OK || res.Status != 404 || res.Reason != "unknown room at <url>" {
		t.Errorf("SendTest(permanent, admin) = (%+v, %v)", res, err)
	}
	// Other users are not told what the destination answered.
	res, err = e.svc.SendTest(ctx, e.alice, c.ID, nil)
	if err != nil || res.OK || res.Status != 0 || res.Reason != notify.GenericReason || res.Latency != 0 {
		t.Errorf("SendTest(permanent, user) = (%+v, %v)", res, err)
	}
	// Failures that say nothing about the destination stay readable.
	e.ch.fail = notify.Public(notify.Permanent(0, "the email configuration is not valid"))
	res, err = e.svc.SendTest(ctx, e.alice, c.ID, nil)
	if err != nil || res.OK || res.Reason != "the email configuration is not valid" {
		t.Errorf("SendTest(public, user) = (%+v, %v)", res, err)
	}
	// An error that is not a SendError could quote the configuration: it
	// is never shown.
	e.ch.fail = errors.New("post " + secretURL + ": connection refused")
	res, err = e.svc.SendTest(ctx, e.alice, c.ID, nil)
	if err != nil || res.OK || res.Reason == "" || res.Status != 0 {
		t.Errorf("SendTest(opaque error) = (%+v, %v)", res, err)
	}
	noSecrets(t, "the reason", res.Reason)

	// The send is bounded by the timeout.
	e.ch.fail, e.ch.block = nil, true
	res, err = e.svc.SendTest(ctx, e.alice, c.ID, nil)
	if err != nil || res.OK || res.Reason == "" {
		t.Errorf("SendTest(no answer) = (%+v, %v)", res, err)
	}
	// A caller that gives up gets its context's error.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = e.svc.SendTest(cctx, e.alice, c.ID, nil)
	wantErr(t, "SendTest(cancelled)", err, context.Canceled)
	// Unreadable secrets.
	e.ch.block = false
	e.cipher.failDecrypt = true
	_, err = e.svc.SendTest(ctx, e.alice, c.ID, nil)
	wantErr(t, "SendTest(undecryptable)", err, domain.ErrFailedPrecondition)
	noSecrets(t, "the log", e.logs.String())
}

// A channel that quotes the path of the secret URL in its reason is caught
// by the service's own redaction.
func TestSendTestRedactsSecretsFromReasons(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	// An administrator sees reasons, so the redaction is what is tested.
	c := e.mustCreate(t, e.admin, fakeInput("Ops"))
	e.ch.fail = notify.Permanent(404, `no route for "POST /secret-url-token-0001" (Bearer secret-token-value-0002)`)
	res, err := e.svc.SendTest(context.Background(), e.admin, c.ID, nil)
	if err != nil || res.OK || !strings.Contains(res.Reason, "no route") {
		t.Fatalf("SendTest = (%+v, %v)", res, err)
	}
	noSecrets(t, "the reason", res.Reason)
	noSecrets(t, "the log", e.logs.String())
}

func TestSendTestRateLimited(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	c := e.mustCreate(t, e.alice, fakeInput("Ops"))
	e.limiter.left = 2
	for i := range 2 {
		if _, err := e.svc.SendTest(ctx, e.alice, c.ID, nil); err != nil {
			t.Fatalf("test %d: %v", i, err)
		}
	}
	_, err := e.svc.SendTest(ctx, e.alice, c.ID, nil)
	wantErr(t, "SendTest(over the limit)", err, domain.ErrRateLimited)
	if got := len(e.ch.calls()); got != 2 {
		t.Errorf("sends = %d, want 2", got)
	}
	// The limit is per user, and invalid requests do not take a token.
	if !slices.Equal(slices.Compact(e.limiter.calls), []string{e.alice.UserID.String()}) {
		t.Errorf("limiter keys = %v", e.limiter.calls)
	}
	n := len(e.limiter.calls)
	_, _ = e.svc.SendTest(ctx, e.alice, domain.ContactID{}, &contacts.Input{Type: "fake"})
	if len(e.limiter.calls) != n {
		t.Error("an invalid test consumed a token")
	}
}
