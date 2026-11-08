package api

import (
	"context"

	"github.com/Zeptiny/Syncr/internal/app/contacts"
	"github.com/Zeptiny/Syncr/internal/app/notifications"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// ContactService is the part of the contacts use cases the API calls
// (implemented by *contacts.Service; see internal/app/contacts). Every use
// case taking an actor scopes by it: the actor's own contacts only, admins
// included, and domain.ErrNotFound otherwise. Channel descriptors come from
// Deps.Channels.
type ContactService interface {
	Create(ctx context.Context, actor domain.Actor, in contacts.Input) (contacts.Contact, error)
	Get(ctx context.Context, actor domain.Actor, id domain.ContactID) (contacts.Contact, error)
	List(ctx context.Context, actor domain.Actor, f contacts.ListFilter) ([]contacts.Contact, error)
	Update(ctx context.Context, actor domain.Actor, id domain.ContactID, version int64, in contacts.Input) (contacts.Contact, error)
	Delete(ctx context.Context, actor domain.Actor, id domain.ContactID) error
	// UsedBy returns the schedules linked to each of the actor's contacts
	// ids (Contact.used_by).
	UsedBy(ctx context.Context, actor domain.Actor, ids []domain.ContactID) (map[domain.ContactID][]contacts.ScheduleRef, error)
	// SendTest sends a test notification (SendTestNotification): id may be
	// zero, in may be nil, not both.
	SendTest(ctx context.Context, actor domain.Actor, id domain.ContactID, in *contacts.Input) (contacts.TestResult, error)
	// Names returns display references of any contacts (a schedule's
	// contact links).
	Names(ctx context.Context, ids []domain.ContactID) (map[domain.ContactID]contacts.Ref, error)
}

// DeliveryService is the part of the notifications use cases the API calls
// (implemented by *notifications.Service): the delivery log of the actor's
// contacts.
type DeliveryService interface {
	List(ctx context.Context, actor domain.Actor, f notifications.DeliveryFilter) (notifications.Page, error)
	Retry(ctx context.Context, actor domain.Actor, id domain.DeliveryID) (notifications.Delivery, error)
}
