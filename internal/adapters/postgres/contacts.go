package postgres

// This file is the contacts.Repository of the contracts step: every method
// returns a wrapped errors.ErrUnsupported (the storetest suite skips) until
// the contacts track implements it with queries/contacts.sql.

import (
	"context"
	"errors"
	"fmt"

	"github.com/Zeptiny/Syncr/internal/app/contacts"
	"github.com/Zeptiny/Syncr/internal/domain"
)

var _ contacts.Repository = (*Store)(nil)

// errContactsPending is the error of the contacts stubs.
func errContactsPending(op string) error {
	return fmt.Errorf("postgres: %s: %w (contacts are not stored in Postgres yet)", op, errors.ErrUnsupported)
}

// CreateContact implements contacts.Repository (stub).
func (s *Store) CreateContact(context.Context, contacts.Contact) error {
	return errContactsPending("create contact")
}

// GetContact implements contacts.Repository (stub).
func (s *Store) GetContact(context.Context, domain.ContactID) (contacts.Contact, error) {
	return contacts.Contact{}, errContactsPending("get contact")
}

// GetContacts implements contacts.Repository (stub).
func (s *Store) GetContacts(context.Context, []domain.ContactID) ([]contacts.Contact, error) {
	return nil, errContactsPending("get contacts")
}

// ListContacts implements contacts.Repository (stub).
func (s *Store) ListContacts(context.Context, contacts.ListFilter) ([]contacts.Contact, error) {
	return nil, errContactsPending("list contacts")
}

// UpdateContact implements contacts.Repository (stub).
func (s *Store) UpdateContact(context.Context, contacts.Contact) (contacts.Contact, error) {
	return contacts.Contact{}, errContactsPending("update contact")
}

// DeleteContact implements contacts.Repository (stub).
func (s *Store) DeleteContact(context.Context, domain.ContactID) error {
	return errContactsPending("delete contact")
}

// ContactSchedules implements contacts.Repository (stub).
func (s *Store) ContactSchedules(context.Context, []domain.ContactID) (map[domain.ContactID][]contacts.ScheduleRef, error) {
	return nil, errContactsPending("contact schedules")
}
