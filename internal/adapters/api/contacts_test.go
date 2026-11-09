package api

import (
	"context"
	crand "crypto/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/adapters/memstore"
	"github.com/Zeptiny/Syncr/internal/adapters/secrets"
	"github.com/Zeptiny/Syncr/internal/app/contacts"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/notifications"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/notify"
	"github.com/Zeptiny/Syncr/internal/platform/clock"
	"github.com/Zeptiny/Syncr/internal/platform/ratelimit"
)

// contactSecret is the secret of the test contacts: no response, log line,
// test result or delivery error may contain it.
const contactSecret = "https://chat.example/hooks/sentinel-contact-secret-7f3a"

// notifyEnv is a test server with the contact, delivery and channel
// services, on a store of their own (the job services of testEnv do not
// notify): the fake channel types, a worker, and the test users.
type notifyEnv struct {
	*testEnv
	store    *memstore.Store
	clock    *clock.Fake
	ids      *idgen.Sequential
	channel  *fakeChannel
	contacts *contacts.Service
	outbox   *notifications.Service
	svcLogs  *logBuffer
	worker   domain.WorkerID
}

// newNotifyEnv serves the fake channel types; mutate may adjust the Deps
// (e.g. to drop the delivery log).
func newNotifyEnv(t *testing.T, mutate ...func(*Deps)) *notifyEnv {
	t.Helper()
	key := secrets.Key{ID: 1, Bytes: make([]byte, secrets.KeySize)}
	if _, err := crand.Read(key.Bytes); err != nil {
		t.Fatal(err)
	}
	cipher, err := secrets.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	e := &notifyEnv{
		store: memstore.New(), clock: clock.NewFake(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)), ids: &idgen.Sequential{},
		channel: &fakeChannel{}, svcLogs: &logBuffer{},
	}
	reg, err := notify.NewRegistry(e.channel, &otherChannel{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := e.clock.Now()
	for _, a := range []domain.Actor{testAdmin, testUser, testOther} {
		u := users.User{ID: a.UserID, Username: "user-" + a.UserID.String()[32:], Role: a.Role, PasswordHash: "x",
			CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now, Version: 1}
		if err := e.store.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	e.worker = e.ids.NewWorkerID()
	if err := e.store.CreateWorker(ctx, workers.Worker{ID: e.worker, Name: "nas-01", Mode: domain.WorkerActive, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	limiter, err := ratelimit.New(ratelimit.Config{Burst: contacts.TestBurst, Every: contacts.TestEvery, MaxKeys: 100}, e.clock)
	if err != nil {
		t.Fatal(err)
	}
	logger := newTestLogger(e.svcLogs)
	e.contacts, err = contacts.New(contacts.Deps{
		Contacts: e.store, Channels: reg, Cipher: cipher, Limiter: limiter, Clock: e.clock, IDs: e.ids, Logger: logger,
	}, contacts.Config{SendTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	e.outbox, err = notifications.New(notifications.Deps{
		Repo: e.store, Channels: reg, Contacts: e.contacts, Admins: storeAdmins{e.store}, Clock: e.clock, IDs: e.ids,
		PublicURL: "https://syncr.example", Logger: logger,
	}, notifications.Config{})
	if err != nil {
		t.Fatal(err)
	}
	e.testEnv = newEnv(t, append([]func(*Deps){func(d *Deps) {
		d.Contacts, d.Deliveries, d.Channels = e.contacts, e.outbox, reg
	}}, mutate...)...)
	return e
}

func (e *notifyEnv) contactsFor(a domain.Actor) syncrv1connect.ContactServiceClient {
	return syncrv1connect.NewContactServiceClient(e.clientWith(testActorHeader, actorHeader(a, "session")), e.srv.URL)
}

func (e *notifyEnv) channelsFor(a domain.Actor) syncrv1connect.ChannelServiceClient {
	return syncrv1connect.NewChannelServiceClient(e.clientWith(testActorHeader, actorHeader(a, "session")), e.srv.URL)
}

func fakeContactInput(name, room string) *syncrv1.ContactInput {
	return &syncrv1.ContactInput{
		Name: name, ChannelType: "fake", Enabled: true,
		Options: map[string]string{"room": room},
		Secrets: map[string]string{"webhook_url": contactSecret},
	}
}

func (e *notifyEnv) createContact(a domain.Actor, in *syncrv1.ContactInput) *syncrv1.Contact {
	e.t.Helper()
	resp, err := e.contactsFor(a).CreateContact(e.t.Context(), connect.NewRequest(&syncrv1.CreateContactRequest{Input: in}))
	if err != nil {
		e.t.Fatalf("CreateContact(%s): %v", in.GetName(), err)
	}
	noContactSecret(e.t, "CreateContact", resp.Msg)
	return resp.Msg.GetContact()
}

func (e *notifyEnv) contactID(c *syncrv1.Contact) domain.ContactID {
	e.t.Helper()
	id, err := domain.ParseContactID(c.GetId())
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// addSchedule stores a schedule of a linked to contacts.
func (e *notifyEnv) addSchedule(a domain.Actor, name string, links ...schedules.ContactLink) schedules.Schedule {
	e.t.Helper()
	now := e.clock.Now()
	sc := schedules.Schedule{
		ID: e.ids.NewScheduleID(), OwnerID: a.UserID, Name: name, Cron: "30 2 * * *", Timezone: "Europe/Berlin",
		Kind: "rclone_transfer", Definition: []byte(`{}`), Target: schedules.Target{WorkerID: e.worker},
		Missed: schedules.MissedPolicy{Action: schedules.MissedRunLate}, Overlap: schedules.OverlapSkip,
		Contacts: links, CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	if err := e.store.CreateSchedule(context.Background(), sc); err != nil {
		e.t.Fatal(err)
	}
	return sc
}

// finishJob stores a job of a that finished with status.
func (e *notifyEnv) finishJob(a domain.Actor, status domain.JobStatus, schedule domain.ScheduleID) jobs.Job {
	e.t.Helper()
	ctx := context.Background()
	now := e.clock.Now()
	j := jobs.Job{
		ID: e.ids.NewJobID(), WorkerID: e.worker, OwnerID: a.UserID, Kind: "rclone_transfer", Status: domain.JobQueued,
		Definition: []byte(`{}`), ScheduleID: schedule, CreatedAt: now, QueuedAt: now, ExitCode: -1, Version: 1,
	}
	if err := e.store.CreateJob(ctx, j); err != nil {
		e.t.Fatal(err)
	}
	j.Status, j.FinishedAt = status, now
	stored, err := e.store.UpdateJob(ctx, j)
	if err != nil {
		e.t.Fatal(err)
	}
	return stored
}

// planAndDeliver runs one planner and deliverer pass.
func (e *notifyEnv) planAndDeliver() {
	e.t.Helper()
	if _, err := e.outbox.Plan(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.outbox.DeliverDue(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *notifyEnv) listDeliveries(a domain.Actor, req *syncrv1.ListDeliveriesRequest) *syncrv1.ListDeliveriesResponse {
	e.t.Helper()
	resp, err := e.contactsFor(a).ListDeliveries(e.t.Context(), connect.NewRequest(req))
	if err != nil {
		e.t.Fatalf("ListDeliveries: %v", err)
	}
	noContactSecret(e.t, "ListDeliveries", resp.Msg)
	return resp.Msg
}

// noContactSecret fails if msg's JSON form contains the contact secret.
func noContactSecret(t *testing.T, what string, msg proto.Message) {
	t.Helper()
	b, err := protojson.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "sentinel-contact-secret") {
		t.Errorf("%s response contains the contact secret: %s", what, b)
	}
}

// noSecretInLogs fails if the API's or the services' logs contain the
// contact secret.
func (e *notifyEnv) noSecretInLogs() {
	e.t.Helper()
	for name, logs := range map[string]string{"api": e.logs.String(), "services": e.svcLogs.String()} {
		if strings.Contains(logs, "sentinel-contact-secret") {
			e.t.Errorf("the %s log contains the contact secret", name)
		}
	}
}

func TestContactCRUD(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t)
	ctx := t.Context()
	client := e.contactsFor(testUser)
	in := fakeContactInput("Team chat", "ops")
	in.JobEvents = []syncrv1.NotificationEvent{syncrv1.NotificationEvent_NOTIFICATION_EVENT_INTERRUPTED, syncrv1.NotificationEvent_NOTIFICATION_EVENT_FAILED}
	c := e.createContact(testUser, in)
	if c.GetName() != "Team chat" || c.GetVersion() != 1 || !c.GetEnabled() || c.GetOptions()["room"] != "ops" ||
		!slices.Equal(c.GetJobEvents(), []syncrv1.NotificationEvent{syncrv1.NotificationEvent_NOTIFICATION_EVENT_FAILED, syncrv1.NotificationEvent_NOTIFICATION_EVENT_INTERRUPTED}) ||
		c.GetCreatedAt() == nil || len(c.GetUsedBy()) != 0 {
		t.Errorf("CreateContact = %v", c)
	}
	e.createContact(testUser, fakeContactInput("another", "infra"))
	other := fakeContactInput("Other chat", "x")
	other.ChannelType = "other"
	e.createContact(testUser, other)

	got, err := client.GetContact(ctx, connect.NewRequest(&syncrv1.GetContactRequest{ContactId: c.GetId()}))
	if err != nil || !proto.Equal(got.Msg.GetContact(), c) {
		t.Errorf("GetContact = (%v, %v)", got, err)
	}
	list, err := client.ListContacts(ctx, connect.NewRequest(&syncrv1.ListContactsRequest{}))
	if err != nil || len(list.Msg.GetContacts()) != 3 || list.Msg.GetContacts()[0].GetName() != "another" {
		t.Errorf("ListContacts = (%v, %v)", list, err)
	}
	noContactSecret(t, "ListContacts", list.Msg)
	if list, _ := client.ListContacts(ctx, connect.NewRequest(&syncrv1.ListContactsRequest{ChannelType: "other"})); len(list.Msg.GetContacts()) != 1 {
		t.Errorf("ListContacts(other) = %v", list.Msg)
	}

	// Validation: field details, never values.
	bad := fakeContactInput("", "")
	bad.Secrets["webhook_url"] = ""
	bad.Options["unknown"] = contactSecret
	bad.JobEvents = []syncrv1.NotificationEvent{syncrv1.NotificationEvent_NOTIFICATION_EVENT_MISSED}
	_, err = client.CreateContact(ctx, connect.NewRequest(&syncrv1.CreateContactRequest{Input: bad}))
	wantCode(t, err, connect.CodeInvalidArgument)
	fields := fieldViolations(t, err)
	for _, f := range []string{"name", "options.room", "options.unknown", "secrets.webhook_url", "job_events[0]"} {
		if _, ok := fields[f]; !ok {
			t.Errorf("violations %v lack %q", fields, f)
		}
	}
	if strings.Contains(err.Error(), "sentinel-contact-secret") {
		t.Error("a validation error echoes a value")
	}
	unspecified := fakeContactInput("x", "y")
	unspecified.JobEvents = []syncrv1.NotificationEvent{syncrv1.NotificationEvent_NOTIFICATION_EVENT_UNSPECIFIED}
	_, err = client.CreateContact(ctx, connect.NewRequest(&syncrv1.CreateContactRequest{Input: unspecified}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = client.CreateContact(ctx, connect.NewRequest(&syncrv1.CreateContactRequest{Input: fakeContactInput("TEAM CHAT", "x")}))
	wantCode(t, err, connect.CodeAlreadyExists)

	// Update: options replaced, secrets kept, type fixed.
	upd := &syncrv1.ContactInput{Name: "Team chat", Options: map[string]string{"room": "infra", "mention": "true"}}
	u, err := client.UpdateContact(ctx, connect.NewRequest(&syncrv1.UpdateContactRequest{ContactId: c.GetId(), Version: 1, Input: upd}))
	if err != nil {
		t.Fatal(err)
	}
	if uc := u.Msg.GetContact(); uc.GetVersion() != 2 || uc.GetSummary() != "room infra" || uc.GetEnabled() || len(uc.GetJobEvents()) != 0 ||
		!slices.Equal(uc.GetSecretsSet(), []string{"webhook_url"}) {
		t.Errorf("UpdateContact = %v", uc)
	}
	noContactSecret(t, "UpdateContact", u.Msg)
	_, err = client.UpdateContact(ctx, connect.NewRequest(&syncrv1.UpdateContactRequest{ContactId: c.GetId(), Version: 1, Input: upd}))
	wantCode(t, err, connect.CodeAborted)
	_, err = client.UpdateContact(ctx, connect.NewRequest(&syncrv1.UpdateContactRequest{ContactId: c.GetId(), Input: upd}))
	wantCode(t, err, connect.CodeInvalidArgument)
	retype := proto.CloneOf(upd)
	retype.ChannelType = "other"
	_, err = client.UpdateContact(ctx, connect.NewRequest(&syncrv1.UpdateContactRequest{ContactId: c.GetId(), Version: 2, Input: retype}))
	wantCode(t, err, connect.CodeInvalidArgument)
	clearReq := proto.CloneOf(upd)
	clearReq.ClearSecrets = []string{"webhook_url"}
	_, err = client.UpdateContact(ctx, connect.NewRequest(&syncrv1.UpdateContactRequest{ContactId: c.GetId(), Version: 2, Input: clearReq}))
	if _, ok := fieldViolations(t, err)["clear_secrets[0]"]; !ok {
		t.Errorf("clearing a required secret: %v", err)
	}

	// Delete.
	if _, err := client.DeleteContact(ctx, connect.NewRequest(&syncrv1.DeleteContactRequest{ContactId: c.GetId()})); err != nil {
		t.Fatal(err)
	}
	_, err = client.GetContact(ctx, connect.NewRequest(&syncrv1.GetContactRequest{ContactId: c.GetId()}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = client.GetContact(ctx, connect.NewRequest(&syncrv1.GetContactRequest{ContactId: "nope"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	e.noSecretInLogs()
}

func TestContactsAreOwnerScoped(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t)
	ctx := t.Context()
	in := fakeContactInput("Team chat", "nowhere") // fails for good: a dead delivery to retry
	in.JobEvents = []syncrv1.NotificationEvent{syncrv1.NotificationEvent_NOTIFICATION_EVENT_FAILED}
	c := e.createContact(testUser, in)
	e.finishJob(testUser, domain.JobFailed, domain.ScheduleID{})
	e.planAndDeliver()
	ds := e.listDeliveries(testUser, &syncrv1.ListDeliveriesRequest{})
	if len(ds.GetDeliveries()) != 1 || ds.GetDeliveries()[0].GetStatus() != syncrv1.DeliveryStatus_DELIVERY_STATUS_DEAD {
		t.Fatalf("deliveries = %v", ds)
	}
	dead := ds.GetDeliveries()[0].GetId()

	// Another user, and an admin: their own contacts only.
	for _, a := range []domain.Actor{testOther, testAdmin} {
		client := e.contactsFor(a)
		_, err := client.GetContact(ctx, connect.NewRequest(&syncrv1.GetContactRequest{ContactId: c.GetId()}))
		wantCode(t, err, connect.CodeNotFound)
		_, err = client.UpdateContact(ctx, connect.NewRequest(&syncrv1.UpdateContactRequest{ContactId: c.GetId(), Version: 1, Input: in}))
		wantCode(t, err, connect.CodeNotFound)
		_, err = client.DeleteContact(ctx, connect.NewRequest(&syncrv1.DeleteContactRequest{ContactId: c.GetId()}))
		wantCode(t, err, connect.CodeNotFound)
		_, err = client.SendTestNotification(ctx, connect.NewRequest(&syncrv1.SendTestNotificationRequest{ContactId: c.GetId()}))
		wantCode(t, err, connect.CodeNotFound)
		_, err = client.SendTestNotification(ctx, connect.NewRequest(&syncrv1.SendTestNotificationRequest{
			ContactId: c.GetId(), Input: &syncrv1.ContactInput{Options: map[string]string{"room": "x"}},
		}))
		wantCode(t, err, connect.CodeNotFound)
		_, err = client.ListDeliveries(ctx, connect.NewRequest(&syncrv1.ListDeliveriesRequest{ContactId: c.GetId()}))
		wantCode(t, err, connect.CodeNotFound)
		_, err = client.RetryDelivery(ctx, connect.NewRequest(&syncrv1.RetryDeliveryRequest{DeliveryId: dead}))
		wantCode(t, err, connect.CodeNotFound)
		if list, err := client.ListContacts(ctx, connect.NewRequest(&syncrv1.ListContactsRequest{})); err != nil || len(list.Msg.GetContacts()) != 0 {
			t.Errorf("ListContacts(%s) = (%v, %v)", a.Role, list, err)
		}
		if ds := e.listDeliveries(a, &syncrv1.ListDeliveriesRequest{}); len(ds.GetDeliveries()) != 0 {
			t.Errorf("ListDeliveries(%s) = %v", a.Role, ds)
		}
	}
	if len(e.channel.messages()) != 1 {
		t.Errorf("other users' calls sent notifications: %+v", e.channel.messages())
	}
	// The owner still has it all.
	if _, err := e.contactsFor(testUser).RetryDelivery(ctx, connect.NewRequest(&syncrv1.RetryDeliveryRequest{DeliveryId: dead})); err != nil {
		t.Errorf("RetryDelivery(owner): %v", err)
	}
}

func TestContactUsedBy(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t)
	ctx := t.Context()
	c := e.createContact(testUser, fakeContactInput("Team chat", "ops"))
	link := schedules.ContactLink{ContactID: e.contactID(c), Events: domain.EventSet{domain.EventFailed}}
	nightly := e.addSchedule(testUser, "nightly", link)
	archive := e.addSchedule(testUser, "Archive", link)
	got, err := e.contactsFor(testUser).GetContact(ctx, connect.NewRequest(&syncrv1.GetContactRequest{ContactId: c.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	refs := got.Msg.GetContact().GetUsedBy()
	if len(refs) != 2 || refs[0].GetId() != archive.ID.String() || refs[0].GetName() != "Archive" || refs[1].GetId() != nightly.ID.String() {
		t.Errorf("used_by = %v", refs)
	}
	list, _ := e.contactsFor(testUser).ListContacts(ctx, connect.NewRequest(&syncrv1.ListContactsRequest{}))
	if len(list.Msg.GetContacts()) != 1 || len(list.Msg.GetContacts()[0].GetUsedBy()) != 2 {
		t.Errorf("ListContacts used_by = %v", list.Msg)
	}
}

func TestSendTestNotification(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t)
	ctx := t.Context()
	client := e.contactsFor(testUser)
	c := e.createContact(testUser, fakeContactInput("Team chat", "ops"))

	resp, err := client.SendTestNotification(ctx, connect.NewRequest(&syncrv1.SendTestNotificationRequest{ContactId: c.GetId()}))
	if err != nil || !resp.Msg.GetOk() || resp.Msg.GetError() != "" || resp.Msg.GetLatency() == nil {
		t.Fatalf("SendTestNotification = (%v, %v)", resp, err)
	}
	// Unsaved edits of the stored contact, then unsaved values alone: a
	// failure is a result, not an error.
	resp, err = client.SendTestNotification(ctx, connect.NewRequest(&syncrv1.SendTestNotificationRequest{
		ContactId: c.GetId(), Input: &syncrv1.ContactInput{Options: map[string]string{"room": "nowhere"}},
	}))
	// Users who are not administrators are not told what the destination
	// answered, how, or how fast (they could probe the network with it).
	if err != nil || resp.Msg.GetOk() || resp.Msg.GetStatus() != 0 || resp.Msg.GetError() != notify.GenericReason || resp.Msg.GetLatency().AsDuration() != 0 {
		t.Errorf("SendTestNotification(failing edit) = (%v, %v)", resp, err)
	}
	noContactSecret(t, "SendTestNotification", resp.Msg)
	resp, err = client.SendTestNotification(ctx, connect.NewRequest(&syncrv1.SendTestNotificationRequest{Input: fakeContactInput("", "busy")}))
	if err != nil || resp.Msg.GetOk() || resp.Msg.GetStatus() != 0 || resp.Msg.GetError() != notify.GenericReason {
		t.Errorf("SendTestNotification(unsaved) = (%v, %v)", resp, err)
	}
	if sent := e.channel.messages(); len(sent) != 3 {
		t.Errorf("sends = %d", len(sent))
	}

	// The burst is spent: the next test is rate limited until the bucket
	// refills.
	_, err = client.SendTestNotification(ctx, connect.NewRequest(&syncrv1.SendTestNotificationRequest{ContactId: c.GetId()}))
	wantCode(t, err, connect.CodeResourceExhausted)
	e.clock.Advance(contacts.TestEvery)
	if _, err := client.SendTestNotification(ctx, connect.NewRequest(&syncrv1.SendTestNotificationRequest{ContactId: c.GetId()})); err != nil {
		t.Errorf("SendTestNotification after the refill: %v", err)
	}

	// Invalid requests.
	_, err = client.SendTestNotification(ctx, connect.NewRequest(&syncrv1.SendTestNotificationRequest{}))
	wantCode(t, err, connect.CodeInvalidArgument)
	invalidCfg := fakeContactInput("", "")
	_, err = client.SendTestNotification(ctx, connect.NewRequest(&syncrv1.SendTestNotificationRequest{Input: invalidCfg}))
	if _, ok := fieldViolations(t, err)["options.room"]; !ok {
		t.Errorf("SendTestNotification(invalid) = %v", err)
	}
	e.noSecretInLogs()
}

// Administrators control the network policy, so they see what the
// destination answered.
func TestSendTestNotificationAdminSeesDetails(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t)
	ctx := t.Context()
	client := e.contactsFor(testAdmin)
	c := e.createContact(testAdmin, fakeContactInput("Team chat", "ops"))
	resp, err := client.SendTestNotification(ctx, connect.NewRequest(&syncrv1.SendTestNotificationRequest{
		ContactId: c.GetId(), Input: &syncrv1.ContactInput{Options: map[string]string{"room": "nowhere"}},
	}))
	if err != nil || resp.Msg.GetOk() || resp.Msg.GetStatus() != 404 || resp.Msg.GetError() != "no such room" {
		t.Errorf("SendTestNotification(failing edit) = (%v, %v)", resp, err)
	}
	noContactSecret(t, "SendTestNotification", resp.Msg)
	resp, err = client.SendTestNotification(ctx, connect.NewRequest(&syncrv1.SendTestNotificationRequest{Input: fakeContactInput("", "busy")}))
	if err != nil || resp.Msg.GetOk() || resp.Msg.GetStatus() != 503 {
		t.Errorf("SendTestNotification(unsaved) = (%v, %v)", resp, err)
	}
}

// storeAdmins is the outbox's notifications.AdminChecker, reading roles from
// the store as the composition root does.
type storeAdmins struct{ store *memstore.Store }

func (a storeAdmins) IsAdmin(ctx context.Context, id domain.UserID) (bool, error) {
	u, err := a.store.GetUser(ctx, id)
	return u.Role == domain.RoleAdmin, err
}

// The delivery log tells administrators what the destination answered and
// everybody else only that it could not be delivered.
func TestDeliveryLogHidesNetworkDetailsFromUsers(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t)
	for _, tc := range []struct {
		actor domain.Actor
		want  string
	}{
		{testUser, notify.GenericReason},
		{testAdmin, "status 503: the room is busy"},
	} {
		in := fakeContactInput("Busy chat", "busy")
		in.JobEvents = []syncrv1.NotificationEvent{syncrv1.NotificationEvent_NOTIFICATION_EVENT_FAILED}
		busy := e.createContact(tc.actor, in)
		e.finishJob(tc.actor, domain.JobFailed, domain.ScheduleID{})
		e.planAndDeliver()
		ds := e.listDeliveries(tc.actor, &syncrv1.ListDeliveriesRequest{ContactId: busy.GetId()})
		if len(ds.GetDeliveries()) != 1 || ds.GetDeliveries()[0].GetLastError() != tc.want {
			t.Errorf("deliveries of %s = %v, want last_error %q", tc.actor.Role, ds, tc.want)
		}
	}
}

func TestDeliveryLog(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t)
	ctx := t.Context()
	client := e.contactsFor(testUser)
	in := fakeContactInput("Busy chat", "busy")
	in.JobEvents = []syncrv1.NotificationEvent{syncrv1.NotificationEvent_NOTIFICATION_EVENT_FAILED}
	busy := e.createContact(testUser, in)
	in = fakeContactInput("Ops chat", "ops")
	in.JobEvents = []syncrv1.NotificationEvent{syncrv1.NotificationEvent_NOTIFICATION_EVENT_FAILED}
	ops := e.createContact(testUser, in)
	var jobIDs []domain.JobID
	for range 3 {
		jobIDs = append(jobIDs, e.finishJob(testUser, domain.JobFailed, domain.ScheduleID{}).ID)
	}
	e.planAndDeliver()

	all := e.listDeliveries(testUser, &syncrv1.ListDeliveriesRequest{})
	if len(all.GetDeliveries()) != 6 || all.GetNextPageToken() != "" {
		t.Fatalf("ListDeliveries = %v", all)
	}
	retrying := e.listDeliveries(testUser, &syncrv1.ListDeliveriesRequest{
		ContactId: busy.GetId(), Statuses: []syncrv1.DeliveryStatus{syncrv1.DeliveryStatus_DELIVERY_STATUS_PENDING},
	})
	if len(retrying.GetDeliveries()) != 3 {
		t.Fatalf("pending deliveries of the busy contact = %v", retrying)
	}
	d := retrying.GetDeliveries()[0]
	if d.GetContactId() != busy.GetId() || d.GetEvent() != syncrv1.NotificationEvent_NOTIFICATION_EVENT_FAILED || d.GetAttempts() != 1 ||
		d.GetMaxAttempts() != 8 || d.GetNextAttemptAt() == nil || d.GetLastAttemptAt() == nil || d.GetDeliveredAt() != nil ||
		d.GetLastError() != notify.GenericReason || d.GetTitle() != "Job failed: rclone_transfer" ||
		d.GetJobId() != jobIDs[2].String() || d.GetScheduleId() != "" || d.GetCreatedAt() == nil {
		t.Errorf("newest pending delivery = %v", d)
	}
	delivered := e.listDeliveries(testUser, &syncrv1.ListDeliveriesRequest{
		ContactId: ops.GetId(), Statuses: []syncrv1.DeliveryStatus{syncrv1.DeliveryStatus_DELIVERY_STATUS_DELIVERED},
	})
	if len(delivered.GetDeliveries()) != 3 || delivered.GetDeliveries()[0].GetDeliveredAt() == nil || delivered.GetDeliveries()[0].GetNextAttemptAt() != nil {
		t.Errorf("delivered = %v", delivered)
	}

	// Paging.
	var seen []string
	req := &syncrv1.ListDeliveriesRequest{PageSize: 4}
	for {
		page := e.listDeliveries(testUser, req)
		for _, d := range page.GetDeliveries() {
			seen = append(seen, d.GetId())
		}
		if page.GetNextPageToken() == "" {
			break
		}
		req.PageToken = page.GetNextPageToken()
	}
	if len(seen) != 6 || !slices.IsSortedFunc(seen, func(a, b string) int { return strings.Compare(b, a) }) {
		t.Errorf("paged = %v, want 6 newest first", seen)
	}
	for _, bad := range []*syncrv1.ListDeliveriesRequest{
		{PageToken: "garbage"},
		{PageToken: encodePageToken(jobIDs[0])}, // a job list token
		{PageSize: -1},
		{Statuses: []syncrv1.DeliveryStatus{syncrv1.DeliveryStatus_DELIVERY_STATUS_UNSPECIFIED}},
		{ContactId: "nope"},
	} {
		_, err := client.ListDeliveries(ctx, connect.NewRequest(bad))
		wantCode(t, err, connect.CodeInvalidArgument)
	}

	// Retry: only dead deliveries.
	_, err := client.RetryDelivery(ctx, connect.NewRequest(&syncrv1.RetryDeliveryRequest{DeliveryId: d.GetId()}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	_, err = client.RetryDelivery(ctx, connect.NewRequest(&syncrv1.RetryDeliveryRequest{}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = client.RetryDelivery(ctx, connect.NewRequest(&syncrv1.RetryDeliveryRequest{DeliveryId: e.ids.NewDeliveryID().String()}))
	wantCode(t, err, connect.CodeNotFound)

	// Deleting a contact deletes its log.
	if _, err := client.DeleteContact(ctx, connect.NewRequest(&syncrv1.DeleteContactRequest{ContactId: busy.GetId()})); err != nil {
		t.Fatal(err)
	}
	if all := e.listDeliveries(testUser, &syncrv1.ListDeliveriesRequest{}); len(all.GetDeliveries()) != 3 {
		t.Errorf("deliveries after the delete = %d", len(all.GetDeliveries()))
	}
	e.noSecretInLogs()
}

func TestDeliveryLogNeedsTheOutbox(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t, func(d *Deps) { d.Deliveries = nil })
	ctx := t.Context()
	_, err := e.contactsFor(testUser).ListDeliveries(ctx, connect.NewRequest(&syncrv1.ListDeliveriesRequest{}))
	wantCode(t, err, connect.CodeUnimplemented)
	_, err = e.contactsFor(testUser).RetryDelivery(ctx, connect.NewRequest(&syncrv1.RetryDeliveryRequest{DeliveryId: e.ids.NewDeliveryID().String()}))
	wantCode(t, err, connect.CodeUnimplemented)
	// The contacts themselves work.
	e.createContact(testUser, fakeContactInput("Team chat", "ops"))
}

func TestContactServicesUnmounted(t *testing.T) {
	t.Parallel()
	e := newEnv(t) // no contact or channel services
	ctx := t.Context()
	header := actorHeader(testUser, "session")
	_, err := syncrv1connect.NewContactServiceClient(e.clientWith(testActorHeader, header), e.srv.URL).
		ListContacts(ctx, connect.NewRequest(&syncrv1.ListContactsRequest{}))
	wantCode(t, err, connect.CodeUnimplemented)
	_, err = syncrv1connect.NewChannelServiceClient(e.clientWith(testActorHeader, header), e.srv.URL).
		ListChannels(ctx, connect.NewRequest(&syncrv1.ListChannelsRequest{}))
	wantCode(t, err, connect.CodeUnimplemented)
}

func TestContactProceduresNeedAUser(t *testing.T) {
	t.Parallel()
	e := newNotifyEnv(t)
	ctx := t.Context()
	anon := syncrv1connect.NewContactServiceClient(e.clientWith("", ""), e.srv.URL)
	_, err := anon.ListContacts(ctx, connect.NewRequest(&syncrv1.ListContactsRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
	_, err = syncrv1connect.NewChannelServiceClient(e.clientWith("", ""), e.srv.URL).
		ListChannels(ctx, connect.NewRequest(&syncrv1.ListChannelsRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
	// API tokens may use them too (user access).
	tok := syncrv1connect.NewContactServiceClient(e.clientWith(testActorHeader, actorHeader(testUser, "token")), e.srv.URL)
	if _, err := tok.ListContacts(ctx, connect.NewRequest(&syncrv1.ListContactsRequest{})); err != nil {
		t.Errorf("ListContacts with a token: %v", err)
	}
}
