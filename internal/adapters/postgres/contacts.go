package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Zeptiny/Syncr/internal/adapters/postgres/db"
	"github.com/Zeptiny/Syncr/internal/app/contacts"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Compile-time check that Store implements the contacts port.
var _ contacts.Repository = (*Store)(nil)

// Contacts live in the contacts table. Deleting one removes its schedule
// links (schedule_contacts) and its deliveries through ON DELETE CASCADE;
// the outbox's inserts lock the contact FOR KEY SHARE, so a delivery is
// never inserted for a contact that is being deleted.

// CreateContact implements contacts.Repository. A taken ID or name is a
// unique violation (ErrAlreadyExists), an unknown owner a foreign key
// violation (ErrNotFound), and a name or job events the CHECK constraints
// reject ErrInvalidArgument.
func (s *Store) CreateContact(ctx context.Context, c contacts.Contact) error {
	op := fmt.Sprintf("create contact %s", c.ID)
	options, err := encodeOptions(c.Options)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return mapErr(op, s.q.CreateContact(ctx, db.CreateContactParams{
		ID:          pgID(c.ID),
		OwnerID:     pgID(c.OwnerID),
		Name:        c.Name,
		ChannelType: c.Type,
		Options:     options,
		SecretsEnc:  c.SecretsEnc,
		SecretKeys:  textArray(c.SecretKeys),
		Summary:     cleanText(c.Summary),
		Enabled:     c.Enabled,
		JobEvents:   outboxEventStrings(c.JobEvents),
		CreatedAt:   pgTime(c.CreatedAt),
		UpdatedAt:   pgTime(c.UpdatedAt),
		Version:     c.Version,
	}))
}

// GetContact implements contacts.Repository.
func (s *Store) GetContact(ctx context.Context, id domain.ContactID) (contacts.Contact, error) {
	op := fmt.Sprintf("get contact %s", id)
	row, err := s.q.GetContact(ctx, pgID(id))
	if err != nil {
		return contacts.Contact{}, mapErr(op, err)
	}
	c, err := contactFrom(row)
	if err != nil {
		return contacts.Contact{}, fmt.Errorf("%s: %w", op, err)
	}
	return c, nil
}

// GetContacts implements contacts.Repository.
func (s *Store) GetContacts(ctx context.Context, ids []domain.ContactID) ([]contacts.Contact, error) {
	const op = "get contacts"
	if len(ids) == 0 {
		return []contacts.Contact{}, nil
	}
	rows, err := s.q.GetContacts(ctx, outboxUUIDs(ids))
	if err != nil {
		return nil, mapErr(op, err)
	}
	return contactsFrom(op, rows)
}

// ListContacts implements contacts.Repository.
func (s *Store) ListContacts(ctx context.Context, f contacts.ListFilter) ([]contacts.Contact, error) {
	const op = "list contacts"
	var typ *string
	if f.Type != "" {
		typ = &f.Type
	}
	rows, err := s.q.ListContacts(ctx, db.ListContactsParams{OwnerID: pgOptID(f.OwnerID), ChannelType: typ})
	if err != nil {
		return nil, mapErr(op, err)
	}
	return contactsFrom(op, rows)
}

// UpdateContact implements contacts.Repository: UPDATE ... WHERE id AND
// version RETURNING *. When it matches nothing, a second query tells a
// missing contact (ErrNotFound) from a version mismatch (ErrConflict).
func (s *Store) UpdateContact(ctx context.Context, c contacts.Contact) (contacts.Contact, error) {
	op := fmt.Sprintf("update contact %s", c.ID)
	options, err := encodeOptions(c.Options)
	if err != nil {
		return contacts.Contact{}, fmt.Errorf("%s: %w", op, err)
	}
	row, err := s.q.UpdateContact(ctx, db.UpdateContactParams{
		ID:              pgID(c.ID),
		ExpectedVersion: c.Version,
		Name:            c.Name,
		Options:         options,
		SecretsEnc:      c.SecretsEnc,
		SecretKeys:      textArray(c.SecretKeys),
		Summary:         cleanText(c.Summary),
		Enabled:         c.Enabled,
		JobEvents:       outboxEventStrings(c.JobEvents),
		UpdatedAt:       pgTime(c.UpdatedAt),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		found, qerr := s.q.ContactExists(ctx, pgID(c.ID))
		switch {
		case qerr != nil:
			return contacts.Contact{}, mapErr(op, qerr)
		case !found:
			return contacts.Contact{}, fmt.Errorf("%s: %w", op, domain.ErrNotFound)
		}
		return contacts.Contact{}, fmt.Errorf("%s: not at version %d: %w", op, c.Version, domain.ErrConflict)
	}
	if err != nil {
		return contacts.Contact{}, mapErr(op, err)
	}
	out, err := contactFrom(row)
	if err != nil {
		return contacts.Contact{}, fmt.Errorf("%s: %w", op, err)
	}
	return out, nil
}

// DeleteContact implements contacts.Repository.
func (s *Store) DeleteContact(ctx context.Context, id domain.ContactID) error {
	op := fmt.Sprintf("delete contact %s", id)
	n, err := s.q.DeleteContact(ctx, pgID(id))
	if err != nil {
		return mapErr(op, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
	}
	return nil
}

// ContactSchedules implements contacts.Repository.
func (s *Store) ContactSchedules(ctx context.Context, ids []domain.ContactID) (map[domain.ContactID][]contacts.ScheduleRef, error) {
	const op = "contact schedules"
	out := map[domain.ContactID][]contacts.ScheduleRef{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.q.ContactSchedules(ctx, outboxUUIDs(ids))
	if err != nil {
		return nil, mapErr(op, err)
	}
	for _, row := range rows {
		var c domain.ContactID
		var ref contacts.ScheduleRef
		if err := errors.Join(decodeID(&c, row.ContactID), decodeID(&ref.ID, row.ScheduleID)); err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		ref.Name = row.ScheduleName
		out[c] = append(out[c], ref)
	}
	return out, nil
}

func contactsFrom(op string, rows []db.Contact) ([]contacts.Contact, error) {
	out := make([]contacts.Contact, 0, len(rows))
	for _, row := range rows {
		c, err := contactFrom(row)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		out = append(out, c)
	}
	return out, nil
}

// contactFrom converts a row.
func contactFrom(row db.Contact) (contacts.Contact, error) {
	c := contacts.Contact{
		Name:       row.Name,
		Type:       row.ChannelType,
		SecretsEnc: row.SecretsEnc,
		SecretKeys: nilIfEmpty(row.SecretKeys),
		Summary:    row.Summary,
		Enabled:    row.Enabled,
		JobEvents:  outboxEvents(row.JobEvents),
		CreatedAt:  timeFrom(row.CreatedAt),
		UpdatedAt:  timeFrom(row.UpdatedAt),
		Version:    row.Version,
	}
	if err := errors.Join(decodeID(&c.ID, row.ID), decodeID(&c.OwnerID, row.OwnerID)); err != nil {
		return contacts.Contact{}, err
	}
	if err := json.Unmarshal(row.Options, &c.Options); err != nil {
		return contacts.Contact{}, fmt.Errorf("decode options of contact %s: %w", c.ID, err)
	}
	return c, nil
}

// outboxUUIDs converts IDs of any kind.
func outboxUUIDs[K any](ids []domain.ID[K]) []pgtype.UUID {
	out := make([]pgtype.UUID, len(ids))
	for i, id := range ids {
		out[i] = pgID(id)
	}
	return out
}

// outboxEventStrings is the stored form of an event set (never NULL).
func outboxEventStrings(s domain.EventSet) []string {
	out := make([]string, len(s))
	for i, e := range s {
		out[i] = string(e)
	}
	return out
}

// outboxEvents converts a stored event set; nil when empty. The CHECK
// constraints admit known events only.
func outboxEvents(s []string) domain.EventSet {
	if len(s) == 0 {
		return nil
	}
	out := make(domain.EventSet, len(s))
	for i, e := range s {
		out[i] = domain.NotificationEvent(e)
	}
	return out
}
