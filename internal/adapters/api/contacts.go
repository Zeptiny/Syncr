package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/app/contacts"
	"github.com/Zeptiny/Syncr/internal/app/notifications"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/notify"
)

// contactHandler implements syncr.v1.ContactService over the contacts and
// notifications use cases (Deps.Contacts, Deps.Deliveries); the channel
// types are validated by the contacts service against its registry.
// Secret values are never part of a response or a log line: responses
// carry only the names of the secrets that are set, test results and
// delivery errors only the channels' sanitized reasons, and requests (which
// carry secrets) are never logged. Without Deps.Deliveries the delivery log
// procedures are Unimplemented.
type contactHandler struct {
	syncrv1connect.UnimplementedContactServiceHandler

	contacts   ContactService
	deliveries DeliveryService
	channels   *notify.Registry
	errs       errorMapper
}

var _ syncrv1connect.ContactServiceHandler = (*contactHandler)(nil)

func (h *contactHandler) fail(ctx context.Context, procedure string, err error) error {
	return h.errs.toConnect(ctx, procedure, "contact", err)
}

// ListContacts lists the caller's contacts, by name.
func (h *contactHandler) ListContacts(ctx context.Context, req *connect.Request[syncrv1.ListContactsRequest]) (*connect.Response[syncrv1.ListContactsResponse], error) {
	const procedure = syncrv1connect.ContactServiceListContactsProcedure
	actor, _ := ActorFrom(ctx)
	cs, err := h.contacts.List(ctx, actor, contacts.ListFilter{Type: req.Msg.GetChannelType()})
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	ids := make([]domain.ContactID, len(cs))
	for i, c := range cs {
		ids[i] = c.ID
	}
	usedBy, err := h.contacts.UsedBy(ctx, actor, ids)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	out := &syncrv1.ListContactsResponse{Contacts: make([]*syncrv1.Contact, 0, len(cs))}
	for _, c := range cs {
		out.Contacts = append(out.Contacts, contactToProto(c, usedBy[c.ID]))
	}
	return connect.NewResponse(out), nil
}

// GetContact returns one of the caller's contacts.
func (h *contactHandler) GetContact(ctx context.Context, req *connect.Request[syncrv1.GetContactRequest]) (*connect.Response[syncrv1.GetContactResponse], error) {
	const procedure = syncrv1connect.ContactServiceGetContactProcedure
	id, err := parseContactID("contact_id", req.Msg.GetContactId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	c, err := h.contacts.Get(ctx, actor, id)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	msg, err := h.contact(ctx, actor, c)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	return connect.NewResponse(&syncrv1.GetContactResponse{Contact: msg}), nil
}

// CreateContact creates a contact for the caller.
func (h *contactHandler) CreateContact(ctx context.Context, req *connect.Request[syncrv1.CreateContactRequest]) (*connect.Response[syncrv1.CreateContactResponse], error) {
	const procedure = syncrv1connect.ContactServiceCreateContactProcedure
	in, err := contactInputFromProto(req.Msg.GetInput())
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	actor, _ := ActorFrom(ctx)
	c, err := h.contacts.Create(ctx, actor, in)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	return connect.NewResponse(&syncrv1.CreateContactResponse{Contact: contactToProto(c, nil)}), nil
}

// UpdateContact changes one of the caller's contacts.
func (h *contactHandler) UpdateContact(ctx context.Context, req *connect.Request[syncrv1.UpdateContactRequest]) (*connect.Response[syncrv1.UpdateContactResponse], error) {
	const procedure = syncrv1connect.ContactServiceUpdateContactProcedure
	id, err := parseContactID("contact_id", req.Msg.GetContactId())
	if err != nil {
		return nil, err
	}
	if req.Msg.GetVersion() < 1 {
		return nil, invalidField("version", "is required (the version of the contact that was read)")
	}
	in, err := contactInputFromProto(req.Msg.GetInput())
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	actor, _ := ActorFrom(ctx)
	c, err := h.contacts.Update(ctx, actor, id, req.Msg.GetVersion(), in)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	msg, err := h.contact(ctx, actor, c)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	return connect.NewResponse(&syncrv1.UpdateContactResponse{Contact: msg}), nil
}

// DeleteContact deletes one of the caller's contacts, its schedule links
// and its delivery log.
func (h *contactHandler) DeleteContact(ctx context.Context, req *connect.Request[syncrv1.DeleteContactRequest]) (*connect.Response[syncrv1.DeleteContactResponse], error) {
	id, err := parseContactID("contact_id", req.Msg.GetContactId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	if err := h.contacts.Delete(ctx, actor, id); err != nil {
		return nil, h.fail(ctx, syncrv1connect.ContactServiceDeleteContactProcedure, err)
	}
	return connect.NewResponse(&syncrv1.DeleteContactResponse{}), nil
}

// SendTestNotification sends a test notification now and reports how it
// went: a failed send is a response with ok false, not an error.
func (h *contactHandler) SendTestNotification(ctx context.Context, req *connect.Request[syncrv1.SendTestNotificationRequest]) (*connect.Response[syncrv1.SendTestNotificationResponse], error) {
	const procedure = syncrv1connect.ContactServiceSendTestNotificationProcedure
	var id domain.ContactID
	if req.Msg.GetContactId() != "" {
		var err error
		if id, err = parseContactID("contact_id", req.Msg.GetContactId()); err != nil {
			return nil, err
		}
	}
	var in *contacts.Input
	if req.Msg.GetInput() != nil {
		v, err := contactInputFromProto(req.Msg.GetInput())
		if err != nil {
			return nil, h.fail(ctx, procedure, err)
		}
		in = &v
	}
	if id.IsZero() && in == nil {
		return nil, invalid("contact_id or input is required")
	}
	actor, _ := ActorFrom(ctx)
	res, err := h.contacts.SendTest(ctx, actor, id, in)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	return connect.NewResponse(&syncrv1.SendTestNotificationResponse{
		Ok: res.OK, Error: res.Reason, Status: int32(min(max(res.Status, 0), 1<<31-1)),
		Latency: durationpb.New(res.Latency),
	}), nil
}

// ListDeliveries lists the notifications planned for the caller's
// contacts, newest first. A contact_id that is not one of the caller's
// contacts is NotFound.
func (h *contactHandler) ListDeliveries(ctx context.Context, req *connect.Request[syncrv1.ListDeliveriesRequest]) (*connect.Response[syncrv1.ListDeliveriesResponse], error) {
	const procedure = syncrv1connect.ContactServiceListDeliveriesProcedure
	if h.deliveries == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("the delivery log is not available"))
	}
	m := req.Msg
	actor, _ := ActorFrom(ctx)
	var f notifications.DeliveryFilter
	if m.GetContactId() != "" {
		id, err := parseContactID("contact_id", m.GetContactId())
		if err != nil {
			return nil, err
		}
		if _, err := h.contacts.Get(ctx, actor, id); err != nil {
			return nil, h.fail(ctx, procedure, err)
		}
		f.ContactID = id
	}
	for _, s := range m.GetStatuses() {
		st, ok := deliveryStatusFromProto(s)
		if !ok {
			return nil, invalid("statuses: unknown or unspecified delivery status")
		}
		f.Statuses = append(f.Statuses, st)
	}
	limit, err := clampPageSize(m.GetPageSize(), notifications.DefaultPageSize, notifications.MaxPageSize)
	if err != nil {
		return nil, invalid(err.Error())
	}
	f.Limit = limit
	if f.Before, err = decodeDeliveryPageToken(m.GetPageToken()); err != nil {
		return nil, invalid(err.Error())
	}
	page, err := h.deliveries.List(ctx, actor, f)
	if err != nil {
		return nil, h.errs.toConnect(ctx, procedure, "delivery", err)
	}
	out := &syncrv1.ListDeliveriesResponse{
		Deliveries:    make([]*syncrv1.Delivery, 0, len(page.Deliveries)),
		NextPageToken: encodeDeliveryPageToken(page.Next),
	}
	for _, d := range page.Deliveries {
		out.Deliveries = append(out.Deliveries, deliveryToProto(d))
	}
	return connect.NewResponse(out), nil
}

// RetryDelivery queues one of the caller's dead deliveries again.
func (h *contactHandler) RetryDelivery(ctx context.Context, req *connect.Request[syncrv1.RetryDeliveryRequest]) (*connect.Response[syncrv1.RetryDeliveryResponse], error) {
	const procedure = syncrv1connect.ContactServiceRetryDeliveryProcedure
	if h.deliveries == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("the delivery log is not available"))
	}
	id, err := domain.ParseDeliveryID(req.Msg.GetDeliveryId())
	switch {
	case req.Msg.GetDeliveryId() == "":
		return nil, invalidField("delivery_id", "is required")
	case err != nil || id.IsZero():
		return nil, invalidField("delivery_id", "is not a valid ID")
	}
	actor, _ := ActorFrom(ctx)
	d, err := h.deliveries.Retry(ctx, actor, id)
	if err != nil {
		return nil, h.errs.toConnect(ctx, procedure, "delivery", err)
	}
	return connect.NewResponse(&syncrv1.RetryDeliveryResponse{Delivery: deliveryToProto(d)}), nil
}

// contact converts c with the actor's schedules that notify it.
func (h *contactHandler) contact(ctx context.Context, actor domain.Actor, c contacts.Contact) (*syncrv1.Contact, error) {
	usedBy, err := h.contacts.UsedBy(ctx, actor, []domain.ContactID{c.ID})
	if err != nil {
		return nil, err
	}
	return contactToProto(c, usedBy[c.ID]), nil
}

// parseContactID parses a request's contact ID into an InvalidArgument
// error naming field.
func parseContactID(field, s string) (domain.ContactID, error) {
	if s == "" {
		return domain.ContactID{}, invalidField(field, "is required")
	}
	id, err := domain.ParseContactID(s)
	if err != nil || id.IsZero() {
		return domain.ContactID{}, invalidField(field, "is not a valid ID")
	}
	return id, nil
}

// contactToProto converts c. It carries the names of the secrets that are
// set, never their values (Contact has none: they stay encrypted).
func contactToProto(c contacts.Contact, usedBy []contacts.ScheduleRef) *syncrv1.Contact {
	out := &syncrv1.Contact{
		Id:          c.ID.String(),
		Name:        c.Name,
		ChannelType: c.Type,
		Options:     maps.Clone(c.Options),
		SecretsSet:  slices.Clone(c.SecretKeys),
		Summary:     c.Summary,
		Enabled:     c.Enabled,
		JobEvents:   make([]syncrv1.NotificationEvent, 0, len(c.JobEvents)),
		UsedBy:      make([]*syncrv1.ContactScheduleRef, 0, len(usedBy)),
		CreatedAt:   ts(c.CreatedAt),
		UpdatedAt:   ts(c.UpdatedAt),
		Version:     c.Version,
	}
	for _, e := range c.JobEvents {
		out.JobEvents = append(out.JobEvents, contactEventToProto(e))
	}
	for _, r := range usedBy {
		out.UsedBy = append(out.UsedBy, &syncrv1.ContactScheduleRef{Id: r.ID.String(), Name: r.Name})
	}
	return out
}

// contactInputFromProto converts a ContactInput (nil is empty). Unknown
// events are reported as a *domain.ValidationError; known ones the
// contacts service does not allow (missed, test) are left to it.
func contactInputFromProto(in *syncrv1.ContactInput) (contacts.Input, error) {
	out := contacts.Input{
		Name:         in.GetName(),
		Type:         in.GetChannelType(),
		Options:      maps.Clone(in.GetOptions()),
		Secrets:      maps.Clone(in.GetSecrets()),
		ClearSecrets: slices.Clone(in.GetClearSecrets()),
		Enabled:      in.GetEnabled(),
	}
	var v domain.Validation
	for i, e := range in.GetJobEvents() {
		ev, ok := contactEventFromProto(e)
		if !ok {
			v.Add(fmt.Sprintf("job_events[%d]", i), "is not a known event")
		}
		out.JobEvents = append(out.JobEvents, ev)
	}
	return out, v.Err()
}

func contactEventToProto(e domain.NotificationEvent) syncrv1.NotificationEvent {
	switch e {
	case domain.EventSucceeded:
		return syncrv1.NotificationEvent_NOTIFICATION_EVENT_SUCCEEDED
	case domain.EventFailed:
		return syncrv1.NotificationEvent_NOTIFICATION_EVENT_FAILED
	case domain.EventInterrupted:
		return syncrv1.NotificationEvent_NOTIFICATION_EVENT_INTERRUPTED
	case domain.EventMissed:
		return syncrv1.NotificationEvent_NOTIFICATION_EVENT_MISSED
	case domain.EventTest:
		return syncrv1.NotificationEvent_NOTIFICATION_EVENT_TEST
	}
	return syncrv1.NotificationEvent_NOTIFICATION_EVENT_UNSPECIFIED
}

// contactEventFromProto maps an event; UNSPECIFIED and unknown values are
// not events.
func contactEventFromProto(e syncrv1.NotificationEvent) (domain.NotificationEvent, bool) {
	switch e {
	case syncrv1.NotificationEvent_NOTIFICATION_EVENT_SUCCEEDED:
		return domain.EventSucceeded, true
	case syncrv1.NotificationEvent_NOTIFICATION_EVENT_FAILED:
		return domain.EventFailed, true
	case syncrv1.NotificationEvent_NOTIFICATION_EVENT_INTERRUPTED:
		return domain.EventInterrupted, true
	case syncrv1.NotificationEvent_NOTIFICATION_EVENT_MISSED:
		return domain.EventMissed, true
	case syncrv1.NotificationEvent_NOTIFICATION_EVENT_TEST:
		return domain.EventTest, true
	}
	return "", false
}

func deliveryStatusToProto(s notifications.Status) syncrv1.DeliveryStatus {
	switch s {
	case notifications.StatusPending:
		return syncrv1.DeliveryStatus_DELIVERY_STATUS_PENDING
	case notifications.StatusSending:
		return syncrv1.DeliveryStatus_DELIVERY_STATUS_SENDING
	case notifications.StatusDelivered:
		return syncrv1.DeliveryStatus_DELIVERY_STATUS_DELIVERED
	case notifications.StatusDead:
		return syncrv1.DeliveryStatus_DELIVERY_STATUS_DEAD
	}
	return syncrv1.DeliveryStatus_DELIVERY_STATUS_UNSPECIFIED
}

func deliveryStatusFromProto(s syncrv1.DeliveryStatus) (notifications.Status, bool) {
	switch s {
	case syncrv1.DeliveryStatus_DELIVERY_STATUS_PENDING:
		return notifications.StatusPending, true
	case syncrv1.DeliveryStatus_DELIVERY_STATUS_SENDING:
		return notifications.StatusSending, true
	case syncrv1.DeliveryStatus_DELIVERY_STATUS_DELIVERED:
		return notifications.StatusDelivered, true
	case syncrv1.DeliveryStatus_DELIVERY_STATUS_DEAD:
		return notifications.StatusDead, true
	}
	return "", false
}

// deliveryToProto converts d. Its title comes from the stored message;
// LastError is the channel's sanitized reason (never a secret).
func deliveryToProto(d notifications.Delivery) *syncrv1.Delivery {
	var payload struct {
		Title string `json:"title"`
	}
	_ = json.Unmarshal(d.Payload, &payload) // a malformed payload only lacks a title
	out := &syncrv1.Delivery{
		Id:            d.ID.String(),
		ContactId:     d.ContactID.String(),
		Event:         contactEventToProto(d.Event),
		Status:        deliveryStatusToProto(d.Status),
		Attempts:      int32(min(max(d.Attempts, 0), 1<<31-1)),
		MaxAttempts:   int32(min(max(d.MaxAttempts, 0), 1<<31-1)),
		LastAttemptAt: ts(d.LastAttemptAt),
		DeliveredAt:   ts(d.DeliveredAt),
		CreatedAt:     ts(d.CreatedAt),
		LastError:     d.LastError,
		Title:         payload.Title,
	}
	if d.Status == notifications.StatusPending {
		out.NextAttemptAt = ts(d.NextAttemptAt)
	}
	if !d.JobID.IsZero() {
		out.JobId = d.JobID.String()
	}
	if !d.ScheduleID.IsZero() {
		out.ScheduleId = d.ScheduleID.String()
	}
	return out
}

// deliveryTokenPrefix starts the delivery log's page tokens.
const deliveryTokenPrefix = "d1:"

// encodeDeliveryPageToken makes the opaque token for the page after
// delivery next (none for the zero ID). Clients must treat it as opaque.
func encodeDeliveryPageToken(next domain.DeliveryID) string {
	if next.IsZero() {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(deliveryTokenPrefix + next.String()))
}

// decodeDeliveryPageToken is the inverse of encodeDeliveryPageToken; the
// empty token is the first page (zero ID).
func decodeDeliveryPageToken(token string) (domain.DeliveryID, error) {
	if token == "" {
		return domain.DeliveryID{}, nil
	}
	malformed := errors.New("page_token is malformed")
	if len(token) > maxPageToken {
		return domain.DeliveryID{}, malformed
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil {
		return domain.DeliveryID{}, malformed
	}
	rest, ok := strings.CutPrefix(string(raw), deliveryTokenPrefix)
	if !ok {
		return domain.DeliveryID{}, malformed
	}
	id, err := domain.ParseDeliveryID(rest)
	if err != nil || id.IsZero() {
		return domain.DeliveryID{}, malformed
	}
	return id, nil
}
