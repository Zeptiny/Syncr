package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/adapters/api"
)

// guardReason is what a send refused by the notification network policy
// reports.
const guardReason = "not allowed by the notification network policy"

// m4Client bundles the Milestone 4 API clients of one caller.
type m4Client struct {
	schedules syncrv1connect.ScheduleServiceClient
	contacts  syncrv1connect.ContactServiceClient
	remotes   syncrv1connect.RemoteServiceClient
	workers   syncrv1connect.WorkerAdminServiceClient
	channels  syncrv1connect.ChannelServiceClient
	dashboard syncrv1connect.DashboardServiceClient
}

func newM4Client(c *http.Client, base string) m4Client {
	return m4Client{
		schedules: syncrv1connect.NewScheduleServiceClient(c, base),
		contacts:  syncrv1connect.NewContactServiceClient(c, base),
		remotes:   syncrv1connect.NewRemoteServiceClient(c, base),
		workers:   syncrv1connect.NewWorkerAdminServiceClient(c, base),
		channels:  syncrv1connect.NewChannelServiceClient(c, base),
		dashboard: syncrv1connect.NewDashboardServiceClient(c, base),
	}
}

// labelSchedule is a schedule input that copies remote's data on a worker
// with label (no worker has it in these tests, so every run fails at once
// with no_worker_with_label), notifying contacts of failed runs.
func labelSchedule(name, remote, label string, contacts ...string) *syncrv1.ScheduleInput {
	in := &syncrv1.ScheduleInput{
		Name: name, Cron: "0 3 * * *", Timezone: "UTC", Enabled: true,
		Target:     &syncrv1.ScheduleTarget{Target: &syncrv1.ScheduleTarget_WorkerLabel{WorkerLabel: label}},
		Definition: copyDefinition(remote, "/srv/restore"),
	}
	for _, c := range contacts {
		in.Contacts = append(in.Contacts, &syncrv1.ScheduleContact{
			ContactId: c, Events: []syncrv1.NotificationEvent{syncrv1.NotificationEvent_NOTIFICATION_EVENT_FAILED},
		})
	}
	return in
}

// TestSecurityNotifications checks the Milestone 4 access rules on the real
// wired controller with the default notification network policy
// (private): owner scoping of schedules, runs, contacts, deliveries and
// test sends; admin-only worker labels; contact secrets of every built-in
// channel never in responses, logs, delivery errors or test results; the
// guard refusing loopback and cloud metadata targets on test sends and on
// deliveries; and the test send rate limit.
func TestSecurityNotifications(t *testing.T) {
	t.Parallel()
	logs := &syncBuffer{}
	addr := startController(t, devEnv(map[string]string{
		"SYNCR_SETUP_CODE": testSetupCode, "SYNCR_LOG_LEVEL": "debug", "SYNCR_NOTIFY_SEND_TIMEOUT": "2s",
	}), logs)
	base := "http://" + addr.String()
	ctx := t.Context()
	adminHTTP := bearerClient(http.DefaultTransport, bootstrap(t, http.DefaultTransport, base, testSetupCode))
	admin := newM4Client(adminHTTP, base)

	// A second user, bob, with an API token of his own.
	users := syncrv1connect.NewUserServiceClient(adminHTTP, base)
	if _, err := users.CreateUser(ctx, connect.NewRequest(&syncrv1.CreateUserRequest{
		Username: "bob", Password: "a long password for bob", Role: syncrv1.UserRole_USER_ROLE_USER,
	})); err != nil {
		t.Fatal(err)
	}
	authc := syncrv1connect.NewAuthServiceClient(http.DefaultClient, base)
	login := connect.NewRequest(&syncrv1.LoginRequest{Username: "bob", Password: "a long password for bob"})
	login.Header().Set(api.CSRFHeader, "1")
	resp, err := authc.Login(ctx, login)
	if err != nil {
		t.Fatal(err)
	}
	bobHTTP := cookieClient(http.DefaultTransport, sessionCookie(t, resp.Header()), true)
	bob := newM4Client(bobHTTP, base)

	var responses []string
	record := func(m proto.Message) {
		b, err := protojson.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		responses = append(responses, string(b))
	}

	// The built-in channels are listed.
	chans, err := admin.channels.ListChannels(ctx, connect.NewRequest(&syncrv1.ListChannelsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range chans.Msg.GetChannels() {
		ids = append(ids, c.GetId())
	}
	if strings.Join(ids, ",") != "discord,email,webhook" {
		t.Errorf("channels = %v", ids)
	}

	// The admin's contacts of every built-in channel, with sentinel
	// secrets; the email and webhook ones point at loopback.
	sentinel := func(ch, field string, n int) string { return fmt.Sprintf("sentinel-%s-%s-%d-secret", ch, field, n) }
	inputs := map[string]*syncrv1.ContactInput{
		"discord": {Name: "Discord", ChannelType: "discord",
			Secrets: map[string]string{"webhook_url": "https://discord.com/api/webhooks/42/" + sentinel("discord", "webhook_url", 1)}},
		"email": {Name: "Mail", ChannelType: "email",
			Options: map[string]string{"host": "127.0.0.1", "port": "2525", "security": "starttls", "username": "syncr",
				"from": "syncr@example.net", "to": "admin@example.net"},
			Secrets: map[string]string{"password": sentinel("email", "password", 1)}},
		"webhook": {Name: "Hook", ChannelType: "webhook",
			Secrets: map[string]string{"url": "http://127.0.0.1:9/hook/" + sentinel("webhook", "url", 1),
				"signing_secret": sentinel("webhook", "signing_secret", 1), "authorization": "Bearer " + sentinel("webhook", "authorization", 1)}},
		"metadata": {Name: "Metadata", ChannelType: "webhook",
			Secrets: map[string]string{"url": "http://169.254.169.254/latest/" + sentinel("metadata", "url", 1)}},
	}
	var sentinels []string
	for _, in := range inputs {
		in.Enabled = true
		for _, v := range in.GetSecrets() {
			sentinels = append(sentinels, strings.TrimPrefix(v, "Bearer "))
		}
	}
	contacts := map[string]*syncrv1.Contact{}
	for name, in := range inputs {
		created, err := admin.contacts.CreateContact(ctx, connect.NewRequest(&syncrv1.CreateContactRequest{Input: in}))
		if err != nil {
			t.Fatalf("CreateContact(%s): %v", name, err)
		}
		record(created.Msg)
		c := created.Msg.GetContact()
		// Replace one secret; the others are kept.
		up, ok := proto.Clone(in).(*syncrv1.ContactInput)
		if !ok {
			t.Fatal("clone is not a ContactInput")
		}
		up.Secrets = map[string]string{}
		for k, v := range in.GetSecrets() {
			up.Secrets[k] = strings.Replace(v, "-1-secret", "-2-secret", 1)
			sentinels = append(sentinels, strings.TrimPrefix(up.Secrets[k], "Bearer "))
			break
		}
		updated, err := admin.contacts.UpdateContact(ctx, connect.NewRequest(&syncrv1.UpdateContactRequest{ContactId: c.GetId(), Version: c.GetVersion(), Input: up}))
		if err != nil {
			t.Fatalf("UpdateContact(%s): %v", name, err)
		}
		record(updated.Msg)
		got, err := admin.contacts.GetContact(ctx, connect.NewRequest(&syncrv1.GetContactRequest{ContactId: c.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		record(got.Msg)
		if want := len(in.GetSecrets()); len(got.Msg.GetContact().GetSecretsSet()) != want {
			t.Errorf("%s: secrets_set %v, want %d", name, got.Msg.GetContact().GetSecretsSet(), want)
		}
		contacts[name] = got.Msg.GetContact()
	}
	list, err := admin.contacts.ListContacts(ctx, connect.NewRequest(&syncrv1.ListContactsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	record(list.Msg)

	t.Run("test sends to loopback and metadata are refused", func(t *testing.T) {
		// Not parallel: the admin's three test sends are exactly the burst
		// of the rate limit.
		for _, name := range []string{"email", "webhook", "metadata"} {
			res, err := admin.contacts.SendTestNotification(ctx, connect.NewRequest(&syncrv1.SendTestNotificationRequest{ContactId: contacts[name].GetId()}))
			if err != nil {
				t.Fatalf("SendTest(%s): %v", name, err)
			}
			record(res.Msg)
			if res.Msg.GetOk() || !strings.Contains(res.Msg.GetError(), guardReason) {
				t.Errorf("SendTest(%s) = %v, want the network policy's refusal", name, res.Msg)
			}
		}
	})

	// A schedule whose runs fail at once notifies the loopback and the
	// metadata webhooks, whose deliveries die on the guard.
	remote, err := admin.remotes.CreateRemote(ctx, connect.NewRequest(&syncrv1.CreateRemoteRequest{Input: &syncrv1.RemoteInput{
		Name: "Data", BackendType: "local", Options: map[string]string{"path": "/srv/data"},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	remoteID := remote.Msg.GetRemote().GetId()
	sched, err := admin.schedules.CreateSchedule(ctx, connect.NewRequest(&syncrv1.CreateScheduleRequest{
		Input: labelSchedule("Nightly", remoteID, "nowhere", contacts["webhook"].GetId(), contacts["metadata"].GetId()),
	}))
	if err != nil {
		t.Fatal(err)
	}
	schedule := sched.Msg.GetSchedule()
	if _, err := admin.schedules.RunScheduleNow(ctx, connect.NewRequest(&syncrv1.RunScheduleNowRequest{ScheduleId: schedule.GetId()})); err != nil {
		t.Fatal(err)
	}
	var dead []*syncrv1.Delivery
	deadline := time.Now().Add(30 * time.Second)
	for len(dead) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("no dead deliveries after 30 s: %v", dead)
		}
		time.Sleep(100 * time.Millisecond)
		ds, err := admin.contacts.ListDeliveries(ctx, connect.NewRequest(&syncrv1.ListDeliveriesRequest{
			Statuses: []syncrv1.DeliveryStatus{syncrv1.DeliveryStatus_DELIVERY_STATUS_DEAD},
		}))
		if err != nil {
			t.Fatal(err)
		}
		dead = ds.Msg.GetDeliveries()
	}
	for _, d := range dead {
		record(d)
		if !strings.Contains(d.GetLastError(), guardReason) || d.GetAttempts() != 1 || d.GetScheduleId() != schedule.GetId() ||
			d.GetEvent() != syncrv1.NotificationEvent_NOTIFICATION_EVENT_FAILED {
			t.Errorf("delivery = %v, want dead on the network policy after one attempt", d)
		}
	}
	runs, err := admin.schedules.ListScheduleRuns(ctx, connect.NewRequest(&syncrv1.ListScheduleRunsRequest{ScheduleId: schedule.GetId()}))
	if err != nil || len(runs.Msg.GetRuns()) != 1 || runs.Msg.GetRuns()[0].GetReason() != "no_worker_with_label" {
		t.Fatalf("runs = %v (%v)", runs, err)
	}
	runID := runs.Msg.GetRuns()[0].GetId()

	t.Run("another user's schedules, runs, contacts and deliveries are not found", func(t *testing.T) {
		sid, cid := schedule.GetId(), contacts["webhook"].GetId()
		own, err := bob.remotes.CreateRemote(ctx, connect.NewRequest(&syncrv1.CreateRemoteRequest{Input: &syncrv1.RemoteInput{
			Name: "Bob's data", BackendType: "local", Options: map[string]string{"path": "/srv/bob"},
		}}))
		if err != nil {
			t.Fatal(err)
		}
		bobRemote := own.Msg.GetRemote().GetId()
		if _, err := bob.schedules.CreateSchedule(ctx, connect.NewRequest(&syncrv1.CreateScheduleRequest{Input: labelSchedule("Own", bobRemote, "nowhere")})); err != nil {
			t.Fatalf("bob CreateSchedule with his own remote: %v", err)
		}
		calls := map[string]func() error{
			"GetSchedule": func() error {
				_, err := bob.schedules.GetSchedule(ctx, connect.NewRequest(&syncrv1.GetScheduleRequest{ScheduleId: sid}))
				return err
			},
			"UpdateSchedule": func() error {
				_, err := bob.schedules.UpdateSchedule(ctx, connect.NewRequest(&syncrv1.UpdateScheduleRequest{ScheduleId: sid, Version: schedule.GetVersion(),
					Input: labelSchedule("Mine now", remoteID, "nowhere")}))
				return err
			},
			"SetScheduleEnabled": func() error {
				_, err := bob.schedules.SetScheduleEnabled(ctx, connect.NewRequest(&syncrv1.SetScheduleEnabledRequest{ScheduleId: sid}))
				return err
			},
			"DeleteSchedule": func() error {
				_, err := bob.schedules.DeleteSchedule(ctx, connect.NewRequest(&syncrv1.DeleteScheduleRequest{ScheduleId: sid}))
				return err
			},
			"RunScheduleNow": func() error {
				_, err := bob.schedules.RunScheduleNow(ctx, connect.NewRequest(&syncrv1.RunScheduleNowRequest{ScheduleId: sid}))
				return err
			},
			"ListScheduleRuns": func() error {
				_, err := bob.schedules.ListScheduleRuns(ctx, connect.NewRequest(&syncrv1.ListScheduleRunsRequest{ScheduleId: sid}))
				return err
			},
			"GetContact": func() error {
				_, err := bob.contacts.GetContact(ctx, connect.NewRequest(&syncrv1.GetContactRequest{ContactId: cid}))
				return err
			},
			"UpdateContact": func() error {
				_, err := bob.contacts.UpdateContact(ctx, connect.NewRequest(&syncrv1.UpdateContactRequest{ContactId: cid, Version: contacts["webhook"].GetVersion(),
					Input: &syncrv1.ContactInput{Name: "Mine now", ChannelType: "webhook"}}))
				return err
			},
			"DeleteContact": func() error {
				_, err := bob.contacts.DeleteContact(ctx, connect.NewRequest(&syncrv1.DeleteContactRequest{ContactId: cid}))
				return err
			},
			"SendTestNotification": func() error {
				_, err := bob.contacts.SendTestNotification(ctx, connect.NewRequest(&syncrv1.SendTestNotificationRequest{ContactId: cid}))
				return err
			},
			"SendTestNotification with input": func() error {
				_, err := bob.contacts.SendTestNotification(ctx, connect.NewRequest(&syncrv1.SendTestNotificationRequest{ContactId: cid,
					Input: &syncrv1.ContactInput{Name: "x", ChannelType: "webhook", Secrets: map[string]string{"url": "http://192.0.2.1/x"}}}))
				return err
			},
			"ListDeliveries": func() error {
				_, err := bob.contacts.ListDeliveries(ctx, connect.NewRequest(&syncrv1.ListDeliveriesRequest{ContactId: cid}))
				return err
			},
			"RetryDelivery": func() error {
				_, err := bob.contacts.RetryDelivery(ctx, connect.NewRequest(&syncrv1.RetryDeliveryRequest{DeliveryId: dead[0].GetId()}))
				return err
			},
			"CreateSchedule with the admin's contact": func() error {
				_, err := bob.schedules.CreateSchedule(ctx, connect.NewRequest(&syncrv1.CreateScheduleRequest{Input: labelSchedule("Steal", bobRemote, "nowhere", cid)}))
				return err
			},
			"CreateSchedule with the admin's remote": func() error {
				_, err := bob.schedules.CreateSchedule(ctx, connect.NewRequest(&syncrv1.CreateScheduleRequest{Input: labelSchedule("Steal", remoteID, "nowhere")}))
				return err
			},
		}
		for name, call := range calls {
			if err := call(); connect.CodeOf(err) != connect.CodeNotFound {
				t.Errorf("bob %s on the admin's object: %s, want not_found", name, codeOrOK(err))
			}
		}
		// His lists hold none of the admin's objects.
		ss, err := bob.schedules.ListSchedules(ctx, connect.NewRequest(&syncrv1.ListSchedulesRequest{}))
		if err != nil || len(ss.Msg.GetSchedules()) != 1 || ss.Msg.GetSchedules()[0].GetName() != "Own" {
			t.Errorf("bob's ListSchedules = %v (%v)", ss, err)
		}
		cs, err := bob.contacts.ListContacts(ctx, connect.NewRequest(&syncrv1.ListContactsRequest{}))
		if err != nil || len(cs.Msg.GetContacts()) != 0 {
			t.Errorf("bob's ListContacts = %v (%v)", cs, err)
		}
		ds, err := bob.contacts.ListDeliveries(ctx, connect.NewRequest(&syncrv1.ListDeliveriesRequest{}))
		if err != nil || len(ds.Msg.GetDeliveries()) != 0 {
			t.Errorf("bob's ListDeliveries = %v (%v)", ds, err)
		}
		// The admin's dashboard is the admin's.
		dash, err := bob.dashboard.GetDashboard(ctx, connect.NewRequest(&syncrv1.GetDashboardRequest{AllUsers: true}))
		if err != nil {
			t.Fatal(err)
		}
		if len(dash.Msg.GetRecentMissed()) != 0 || len(dash.Msg.GetUpcoming()) != 1 || dash.Msg.GetUpcoming()[0].GetScheduleId() == sid {
			t.Errorf("bob's dashboard shows the admin's schedules: %v", dash.Msg)
		}
		if mine, err := admin.dashboard.GetDashboard(ctx, connect.NewRequest(&syncrv1.GetDashboardRequest{})); err != nil ||
			len(mine.Msg.GetRecentMissed()) != 1 || mine.Msg.GetRecentMissed()[0].GetId() != runID {
			t.Errorf("admin's dashboard = %v (%v)", mine, err)
		}
		// The admin may retry its own dead delivery.
		if _, err := admin.contacts.RetryDelivery(ctx, connect.NewRequest(&syncrv1.RetryDeliveryRequest{DeliveryId: dead[0].GetId()})); err != nil {
			t.Errorf("admin RetryDelivery = %v", err)
		}
	})

	t.Run("worker labels need an admin", func(t *testing.T) {
		tok, err := admin.workers.CreateEnrollmentToken(ctx, connect.NewRequest(&syncrv1.CreateEnrollmentTokenRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		enrolled, err := syncrv1connect.NewEnrollmentServiceClient(http.DefaultClient, base).Enroll(ctx, connect.NewRequest(&syncrv1.EnrollRequest{
			EnrollmentToken: tok.Msg.GetToken(), Name: "tagged", Os: "linux", Arch: "amd64",
		}))
		if err != nil {
			t.Fatal(err)
		}
		req := &syncrv1.SetWorkerLabelsRequest{WorkerId: enrolled.Msg.GetWorkerId(), Labels: []string{"nas"}}
		if _, err := bob.workers.SetWorkerLabels(ctx, connect.NewRequest(req)); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("bob SetWorkerLabels = %s, want permission_denied", codeOrOK(err))
		}
		got, err := admin.workers.SetWorkerLabels(ctx, connect.NewRequest(req))
		if err != nil || strings.Join(got.Msg.GetWorker().GetLabels(), ",") != "nas" {
			t.Errorf("admin SetWorkerLabels = %v (%v)", got, err)
		}
	})

	t.Run("test sends are rate limited per user", func(t *testing.T) {
		in := &syncrv1.ContactInput{Name: "Probe", ChannelType: "webhook", Secrets: map[string]string{"url": "http://127.0.0.1:9/probe"}}
		for i := range 3 {
			res, err := bob.contacts.SendTestNotification(ctx, connect.NewRequest(&syncrv1.SendTestNotificationRequest{Input: in}))
			if err != nil || res.Msg.GetOk() || !strings.Contains(res.Msg.GetError(), guardReason) {
				t.Fatalf("bob's test send %d = %v (%v)", i, res, err)
			}
		}
		_, err := bob.contacts.SendTestNotification(ctx, connect.NewRequest(&syncrv1.SendTestNotificationRequest{Input: in}))
		if connect.CodeOf(err) != connect.CodeResourceExhausted {
			t.Errorf("bob's fourth test send = %s, want resource_exhausted", codeOrOK(err))
		}
		// The admin's limit is the admin's: used up by the three sends above.
		_, err = admin.contacts.SendTestNotification(ctx, connect.NewRequest(&syncrv1.SendTestNotificationRequest{ContactId: contacts["webhook"].GetId()}))
		if connect.CodeOf(err) != connect.CodeResourceExhausted {
			t.Errorf("admin's fourth test send = %s, want resource_exhausted", codeOrOK(err))
		}
	})

	t.Run("contact secrets never come back", func(t *testing.T) {
		for _, c := range contacts {
			if strings.Contains(c.GetSummary(), "/") || strings.Contains(c.GetSummary(), "sentinel") {
				t.Errorf("summary of %s = %q shows its URL", c.GetName(), c.GetSummary())
			}
		}
		all := strings.Join(responses, "\n")
		out := logs.String()
		for _, v := range sentinels {
			if strings.Contains(all, v) {
				t.Errorf("an API response contains secret %q", v)
			}
			if strings.Contains(out, v) {
				t.Errorf("the log contains secret %q", v)
			}
		}
		if !strings.Contains(out, `"msg":"contact created"`) {
			t.Errorf("the log is not the controller's debug log:\n%s", out)
		}
	})
}
