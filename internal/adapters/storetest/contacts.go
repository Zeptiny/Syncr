package storetest

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/contacts"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// This file is the contract suite of contacts.Repository.

func newContact(t *testing.T, n int, owner domain.UserID, name string) contacts.Contact {
	return contacts.Contact{
		ID: contactID(t, n), OwnerID: owner, Name: name, Type: "webhook",
		Options:    map[string]string{"format": "syncr"},
		SecretsEnc: []byte("encrypted secrets"), SecretKeys: []string{"url"}, Summary: "hooks.example webhook",
		Enabled: true, CreatedAt: t0(), UpdatedAt: t0(), Version: 1,
	}
}

func mustCreateContact(t *testing.T, s Store, c contacts.Contact) {
	t.Helper()
	if err := s.CreateContact(context.Background(), c); err != nil {
		t.Fatalf("CreateContact(%s): %v", c.Name, err)
	}
}

func mustGetContact(t *testing.T, s Store, id domain.ContactID) contacts.Contact {
	t.Helper()
	c, err := s.GetContact(context.Background(), id)
	if err != nil {
		t.Fatalf("GetContact: %v", err)
	}
	return c
}

// requireContacts creates the users of the test and skips it while the
// contacts port is a stub.
func requireContacts(t *testing.T, s Store, owners ...domain.UserID) {
	t.Helper()
	for _, o := range owners {
		ensureUser(t, s, o)
	}
	_, err := s.ListContacts(context.Background(), contacts.ListFilter{})
	skipIfUnsupported(t, "ListContacts", "track B (contacts)", err)
}

// schedulesSupported reports whether the store implements the schedules
// port (the contacts adapter may be finished before the schedules one).
func schedulesSupported(t *testing.T, s Store) bool {
	t.Helper()
	_, err := s.ListSchedules(context.Background(), schedules.ListFilter{})
	if err != nil && !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("ListSchedules: %v", err)
	}
	return err == nil
}

func contactIDs(cs []contacts.Contact) []domain.ContactID {
	out := make([]domain.ContactID, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

func testContactCreateGetList(t *testing.T, s Store) {
	ctx := context.Background()
	alice, bob := userID(t, 1), userID(t, 2)
	requireContacts(t, s, alice, bob)
	ops := newContact(t, 1, alice, "Ops")
	ops.JobEvents = domain.EventSet{domain.EventFailed, domain.EventInterrupted}
	mail := newContact(t, 2, alice, "admin mail")
	mail.Type, mail.SecretsEnc, mail.SecretKeys, mail.Enabled = "email", nil, nil, false
	theirs := newContact(t, 3, bob, "OPS")
	for _, c := range []contacts.Contact{ops, mail, theirs} {
		mustCreateContact(t, s, c)
	}
	wantErr(t, "CreateContact(duplicate id)", s.CreateContact(ctx, ops), domain.ErrAlreadyExists)
	wantErr(t, "CreateContact(name taken, other case)", s.CreateContact(ctx, newContact(t, 4, alice, "ops")), domain.ErrAlreadyExists)
	wantErr(t, "CreateContact(unknown owner)", s.CreateContact(ctx, newContact(t, 5, userID(t, 99), "x")), domain.ErrNotFound)
	missed := newContact(t, 6, alice, "missed")
	missed.JobEvents = domain.EventSet{domain.EventMissed} // manual jobs have no missed runs
	wantErr(t, "CreateContact(missed job event)", s.CreateContact(ctx, missed), domain.ErrInvalidArgument)
	wantErr(t, "CreateContact(empty name)", s.CreateContact(ctx, newContact(t, 7, alice, "")), domain.ErrInvalidArgument)

	got := mustGetContact(t, s, ops.ID)
	if got.OwnerID != alice || got.Name != "Ops" || got.Type != "webhook" || !maps.Equal(got.Options, ops.Options) ||
		string(got.SecretsEnc) != "encrypted secrets" || !slices.Equal(got.SecretKeys, []string{"url"}) ||
		got.Summary != ops.Summary || !got.Enabled || !slices.Equal(got.JobEvents, ops.JobEvents) ||
		!got.CreatedAt.Equal(t0()) || got.Version != 1 {
		t.Errorf("GetContact = %+v", got)
	}
	if got := mustGetContact(t, s, mail.ID); got.Enabled || got.SecretsEnc != nil || len(got.SecretKeys) != 0 || len(got.JobEvents) != 0 {
		t.Errorf("GetContact(no secrets, disabled) = %+v", got)
	}
	_, err := s.GetContact(ctx, contactID(t, 99))
	wantErr(t, "GetContact(missing)", err, domain.ErrNotFound)
	if some, err := s.GetContacts(ctx, []domain.ContactID{theirs.ID, ops.ID, contactID(t, 99), ops.ID}); err != nil ||
		!slices.Equal(contactIDs(some), []domain.ContactID{ops.ID, theirs.ID}) {
		t.Errorf("GetContacts = (%v, %v)", contactIDs(some), err)
	}
	if mine, err := s.ListContacts(ctx, contacts.ListFilter{OwnerID: alice}); err != nil ||
		!slices.Equal(contactIDs(mine), []domain.ContactID{mail.ID, ops.ID}) {
		t.Errorf("ListContacts(alice) = (%v, %v), want by lower-cased name", contactIDs(mine), err)
	}
	if hooks, _ := s.ListContacts(ctx, contacts.ListFilter{Type: "webhook"}); !slices.Equal(contactIDs(hooks), []domain.ContactID{ops.ID, theirs.ID}) {
		t.Errorf("ListContacts(webhook) = %v", contactIDs(hooks))
	}
}

func testContactUpdateVersioned(t *testing.T, s Store) {
	ctx := context.Background()
	alice := userID(t, 1)
	requireContacts(t, s, alice)
	c := newContact(t, 1, alice, "Ops")
	mustCreateContact(t, s, c)
	mustCreateContact(t, s, newContact(t, 2, alice, "Taken"))

	next := c
	next.Name, next.Options, next.SecretsEnc, next.SecretKeys = "Renamed", map[string]string{"format": "slack"}, nil, nil
	next.Summary, next.Enabled, next.JobEvents, next.UpdatedAt = "other", false, domain.EventSet{domain.EventSucceeded}, t0().Add(time.Minute)
	next.Type, next.OwnerID, next.CreatedAt = "email", userID(t, 99), t0().Add(time.Hour) // never written
	stored, err := s.UpdateContact(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != 2 || stored.Name != "Renamed" || stored.Options["format"] != "slack" || stored.SecretsEnc != nil ||
		stored.Summary != "other" || stored.Enabled || !slices.Equal(stored.JobEvents, next.JobEvents) || !stored.UpdatedAt.Equal(next.UpdatedAt) {
		t.Errorf("UpdateContact returned %+v", stored)
	}
	if stored.Type != "webhook" || stored.OwnerID != alice || !stored.CreatedAt.Equal(t0()) {
		t.Errorf("UpdateContact wrote immutable fields: %+v", stored)
	}
	_, err = s.UpdateContact(ctx, c)
	wantErr(t, "UpdateContact(stale version)", err, domain.ErrConflict)
	_, err = s.UpdateContact(ctx, newContact(t, 9, alice, "missing"))
	wantErr(t, "UpdateContact(missing)", err, domain.ErrNotFound)
	taken := stored
	taken.Name = "taken"
	_, err = s.UpdateContact(ctx, taken)
	wantErr(t, "UpdateContact(name taken)", err, domain.ErrAlreadyExists)
	badEvents := stored
	badEvents.JobEvents = domain.EventSet{domain.EventTest}
	_, err = s.UpdateContact(ctx, badEvents)
	wantErr(t, "UpdateContact(test job event)", err, domain.ErrInvalidArgument)
}

func testContactSchedulesAndDelete(t *testing.T, s Store) {
	ctx := context.Background()
	alice := userID(t, 1)
	requireContacts(t, s, alice)
	c1, c2 := newContact(t, 1, alice, "Ops"), newContact(t, 2, alice, "Mail")
	mustCreateContact(t, s, c1)
	mustCreateContact(t, s, c2)
	u := newUser(t, 10)
	mustCreateUser(t, s, u)
	mustCreateContact(t, s, newContact(t, 3, u.ID, "Mine"))
	wantErr(t, "DeleteUser(owns a contact)", s.DeleteUser(ctx, u.ID), domain.ErrFailedPrecondition)

	if !schedulesSupported(t, s) {
		if err := s.DeleteContact(ctx, c1.ID); err != nil {
			t.Fatal(err)
		}
		t.Skip("m4: pending track A (schedules): the schedule links of contacts are not checked")
	}
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	events := domain.EventSet{domain.EventFailed}
	nightly := newSchedule(t, 1, alice, "nightly", w.ID)
	nightly.Contacts = []schedules.ContactLink{{ContactID: c1.ID, Events: events}, {ContactID: c2.ID, Events: events}}
	archive := newSchedule(t, 2, alice, "Archive", w.ID)
	archive.Contacts = []schedules.ContactLink{{ContactID: c1.ID, Events: events}}
	mustCreateSchedule(t, s, nightly)
	mustCreateSchedule(t, s, archive)

	used, err := s.ContactSchedules(ctx, []domain.ContactID{c1.ID, c2.ID, contactID(t, 99)})
	if err != nil || len(used) != 2 || len(used[c1.ID]) != 2 || used[c1.ID][0].ID != archive.ID || used[c1.ID][1].Name != "nightly" ||
		len(used[c2.ID]) != 1 || used[c2.ID][0].ID != nightly.ID {
		t.Errorf("ContactSchedules = (%+v, %v), want by lower-cased name", used, err)
	}

	// A hard delete removes the contact's links (and its deliveries, see
	// the outbox suite).
	if err := s.DeleteContact(ctx, c1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetContact(ctx, c1.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetContact(deleted) = %v", err)
	}
	if got := mustGetSchedule(t, s, nightly.ID); len(got.Contacts) != 1 || got.Contacts[0].ContactID != c2.ID {
		t.Errorf("links after the delete = %+v", got.Contacts)
	}
	if got := mustGetSchedule(t, s, archive.ID); len(got.Contacts) != 0 {
		t.Errorf("links after the delete = %+v", got.Contacts)
	}
	if used, _ := s.ContactSchedules(ctx, []domain.ContactID{c1.ID}); len(used) != 0 {
		t.Errorf("ContactSchedules(deleted) = %v", used)
	}
	wantErr(t, "DeleteContact(deleted)", s.DeleteContact(ctx, c1.ID), domain.ErrNotFound)
}

func testContactNoAliasing(t *testing.T, s Store) {
	ctx := context.Background()
	alice := userID(t, 1)
	requireContacts(t, s, alice)
	c := newContact(t, 1, alice, "Ops")
	c.JobEvents = domain.EventSet{domain.EventFailed}
	mustCreateContact(t, s, c)
	c.Options["format"], c.SecretKeys[0], c.SecretsEnc[0], c.JobEvents[0] = "x", "x", 'x', domain.EventSucceeded
	got := mustGetContact(t, s, c.ID)
	if got.Options["format"] != "syncr" || got.SecretKeys[0] != "url" || got.SecretsEnc[0] != 'e' || got.JobEvents[0] != domain.EventFailed {
		t.Fatalf("CreateContact aliases its argument: %+v", got)
	}
	got.Options["format"], got.SecretKeys[0], got.JobEvents[0] = "y", "y", domain.EventSucceeded
	if again := mustGetContact(t, s, c.ID); again.Options["format"] != "syncr" || again.SecretKeys[0] != "url" || again.JobEvents[0] != domain.EventFailed {
		t.Error("GetContact returns storage")
	}
	list, _ := s.ListContacts(ctx, contacts.ListFilter{})
	list[0].Options["format"] = "z"
	if again := mustGetContact(t, s, c.ID); again.Options["format"] != "syncr" {
		t.Error("ListContacts returns storage")
	}
}
