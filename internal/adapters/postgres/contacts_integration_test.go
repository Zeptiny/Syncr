//go:build integration

package postgres

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/app/contacts"
	"github.com/Zeptiny/Syncr/internal/app/notifications"
	"github.com/Zeptiny/Syncr/internal/domain"
)

func wantDomainErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: err = %v, want %v", what, err, want)
	}
}

// TestContactSchedulesAndCascade covers the schedule links of contacts with
// schedules inserted by plain SQL: ContactSchedules' order, and a delete
// removing the contact's links and deliveries.
func TestContactSchedulesAndCascade(t *testing.T) {
	t.Parallel()
	f := newOutboxFixture(t)
	ctx := t.Context()
	c1, c2 := f.contact("Ops", true, domain.EventFailed), f.contact("Mail", true)
	nightly, archive := f.schedule("nightly"), f.schedule("Archive")
	f.link(nightly, c1, domain.EventFailed)
	f.link(nightly, c2, domain.EventFailed)
	f.link(archive, c1, domain.EventMissed)

	used, err := f.s.ContactSchedules(ctx, []domain.ContactID{c1, c2, domain.NewContactID()})
	if err != nil || len(used) != 2 ||
		!slices.Equal(used[c1], []contacts.ScheduleRef{{ID: archive, Name: "Archive"}, {ID: nightly, Name: "nightly"}}) ||
		!slices.Equal(used[c2], []contacts.ScheduleRef{{ID: nightly, Name: "nightly"}}) {
		t.Errorf("ContactSchedules = (%+v, %v), want by lower-cased name", used, err)
	}
	if used, err := f.s.ContactSchedules(ctx, nil); err != nil || len(used) != 0 {
		t.Errorf("ContactSchedules(none) = (%v, %v)", used, err)
	}

	j := f.job(domain.JobFailed, domain.ScheduleID{}, testTime())
	d := f.delivery(c1, j, "k")
	if n, err := f.s.CommitJobOutcome(ctx, j, []notifications.Delivery{d}, testTime()); err != nil || n != 1 {
		t.Fatalf("CommitJobOutcome = (%d, %v)", n, err)
	}
	if err := f.s.DeleteContact(ctx, c1); err != nil {
		t.Fatal(err)
	}
	var links int
	if err := f.s.pool.QueryRow(ctx, `SELECT count(*) FROM schedule_contacts WHERE contact_id = $1`, pgID(c1)).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if links != 0 {
		t.Errorf("%d links of a deleted contact remain", links)
	}
	_, err = f.s.GetDelivery(ctx, d.ID)
	wantDomainErr(t, "GetDelivery(deleted contact)", err, domain.ErrNotFound)
	if used, _ := f.s.ContactSchedules(ctx, []domain.ContactID{c2}); len(used[c2]) != 1 {
		t.Errorf("the other contact's links = %+v", used)
	}
	wantDomainErr(t, "DeleteContact(again)", f.s.DeleteContact(ctx, c1), domain.ErrNotFound)
}

// TestContactRejectedValues maps the CHECK constraints and bad text to
// ErrInvalidArgument, never echoing the values.
func TestContactRejectedValues(t *testing.T) {
	t.Parallel()
	f := newOutboxFixture(t)
	ctx := t.Context()
	for name, mutate := range map[string]func(*contacts.Contact){
		"long name":        func(c *contacts.Contact) { c.Name = strings.Repeat("x", 65) },
		"nul in name":      func(c *contacts.Contact) { c.Name = "a\x00b" },
		"missed job event": func(c *contacts.Contact) { c.JobEvents = domain.EventSet{domain.EventMissed} },
		"nul in options":   func(c *contacts.Contact) { c.Options = map[string]string{"k": "secret\x00value"} },
		"version 0":        func(c *contacts.Contact) { c.Version = 0 },
	} {
		c := contacts.Contact{
			ID: domain.NewContactID(), OwnerID: f.owner, Name: "ok", Type: "webhook", Enabled: true,
			CreatedAt: testTime(), UpdatedAt: testTime(), Version: 1,
		}
		mutate(&c)
		err := f.s.CreateContact(ctx, c)
		wantDomainErr(t, "CreateContact("+name+")", err, domain.ErrInvalidArgument)
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: the error echoes the value: %v", name, err)
		}
	}
}
