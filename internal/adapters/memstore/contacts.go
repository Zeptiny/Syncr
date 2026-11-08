package memstore

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/app/contacts"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// This file implements contacts.Repository with the semantics of the
// Postgres schema (migration 00003): the owner foreign key, the CHECK
// constraints, the case-insensitive name index and the cascades of a
// delete (schedule links and deliveries).

func cloneContact(c contacts.Contact) contacts.Contact {
	c.Options = maps.Clone(c.Options)
	c.SecretsEnc = slices.Clone(c.SecretsEnc)
	c.SecretKeys = slices.Clone(c.SecretKeys)
	c.JobEvents = slices.Clone(c.JobEvents)
	return c
}

// checkContact enforces the CHECK constraints of a contact row.
func checkContact(c contacts.Contact) error {
	invalid := func(what string) error {
		return fmt.Errorf("contact %s: %w: %s", c.ID, domain.ErrInvalidArgument, what)
	}
	switch n := utf8.RuneCountInString(c.Name); {
	case n < 1 || n > contacts.MaxNameLen:
		return invalid("name length")
	case !c.JobEvents.Within(domain.JobEvents()):
		return invalid("job events")
	case c.Version < 1:
		return invalid("version")
	}
	return nil
}

// contactNameFreeLocked returns ErrAlreadyExists if another contact of
// owner has name, case-insensitively.
func (s *Store) contactNameFreeLocked(id domain.ContactID, owner domain.UserID, name string) error {
	for _, o := range s.contacts {
		if o.ID != id && o.OwnerID == owner && strings.EqualFold(o.Name, name) {
			return fmt.Errorf("contact name %q is taken: %w", name, domain.ErrAlreadyExists)
		}
	}
	return nil
}

// CreateContact implements contacts.Repository.
func (s *Store) CreateContact(_ context.Context, c contacts.Contact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.contacts[c.ID]; ok {
		return fmt.Errorf("contact %s: %w", c.ID, domain.ErrAlreadyExists)
	}
	if _, ok := s.users[c.OwnerID]; !ok {
		return fmt.Errorf("owner %s of contact %s: %w", c.OwnerID, c.ID, domain.ErrNotFound)
	}
	if err := checkContact(c); err != nil {
		return err
	}
	if err := s.contactNameFreeLocked(c.ID, c.OwnerID, c.Name); err != nil {
		return err
	}
	s.contacts[c.ID] = cloneContact(c)
	return nil
}

// GetContact implements contacts.Repository.
func (s *Store) GetContact(_ context.Context, id domain.ContactID) (contacts.Contact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.contacts[id]
	if !ok {
		return contacts.Contact{}, fmt.Errorf("contact %s: %w", id, domain.ErrNotFound)
	}
	return cloneContact(c), nil
}

// GetContacts implements contacts.Repository.
func (s *Store) GetContacts(_ context.Context, ids []domain.ContactID) ([]contacts.Contact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids = slices.Clone(ids)
	slices.SortFunc(ids, compareIDs)
	var out []contacts.Contact
	for _, id := range slices.Compact(ids) {
		if c, ok := s.contacts[id]; ok {
			out = append(out, cloneContact(c))
		}
	}
	return out, nil
}

// ListContacts implements contacts.Repository.
func (s *Store) ListContacts(_ context.Context, f contacts.ListFilter) ([]contacts.Contact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []contacts.Contact
	for _, c := range s.contacts {
		if (f.OwnerID.IsZero() || c.OwnerID == f.OwnerID) && (f.Type == "" || c.Type == f.Type) {
			out = append(out, cloneContact(c))
		}
	}
	slices.SortFunc(out, func(a, b contacts.Contact) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), compareIDs(a.ID, b.ID))
	})
	return out, nil
}

// UpdateContact implements contacts.Repository.
func (s *Store) UpdateContact(_ context.Context, c contacts.Contact) (contacts.Contact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.contacts[c.ID]
	if !ok {
		return contacts.Contact{}, fmt.Errorf("contact %s: %w", c.ID, domain.ErrNotFound)
	}
	if cur.Version != c.Version {
		return contacts.Contact{}, fmt.Errorf("contact %s is at version %d, not %d: %w", c.ID, cur.Version, c.Version, domain.ErrConflict)
	}
	next := cloneContact(c)
	next.OwnerID, next.Type, next.CreatedAt = cur.OwnerID, cur.Type, cur.CreatedAt
	next.Version = cur.Version + 1
	if err := checkContact(next); err != nil {
		return contacts.Contact{}, err
	}
	if err := s.contactNameFreeLocked(next.ID, next.OwnerID, next.Name); err != nil {
		return contacts.Contact{}, err
	}
	s.contacts[next.ID] = next
	return cloneContact(next), nil
}

// DeleteContact implements contacts.Repository.
func (s *Store) DeleteContact(_ context.Context, id domain.ContactID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.contacts[id]; !ok {
		return fmt.Errorf("contact %s: %w", id, domain.ErrNotFound)
	}
	for sid, sc := range s.schedules {
		if i := slices.IndexFunc(sc.Contacts, func(l schedules.ContactLink) bool { return l.ContactID == id }); i >= 0 {
			sc = cloneSchedule(sc)
			sc.Contacts = slices.Delete(sc.Contacts, i, i+1)
			s.schedules[sid] = sc
		}
	}
	for did, d := range s.deliveries {
		if d.ContactID == id {
			delete(s.deliveries, did)
		}
	}
	delete(s.contacts, id)
	return nil
}

// ContactSchedules implements contacts.Repository.
func (s *Store) ContactSchedules(_ context.Context, ids []domain.ContactID) (map[domain.ContactID][]contacts.ScheduleRef, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[domain.ContactID][]contacts.ScheduleRef)
	for _, sc := range s.schedules {
		for _, l := range sc.Contacts {
			if slices.Contains(ids, l.ContactID) {
				out[l.ContactID] = append(out[l.ContactID], contacts.ScheduleRef{ID: sc.ID, Name: sc.Name})
			}
		}
	}
	for _, refs := range out {
		slices.SortFunc(refs, func(a, b contacts.ScheduleRef) int {
			return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), compareIDs(a.ID, b.ID))
		})
	}
	return out, nil
}
