package api

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/app/contacts"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// scheduleEnv is the API test env with the real schedules service (over
// the env's store, remotes and workers) and fake contacts.
type scheduleEnv struct {
	*testEnv
	contacts *fakeScheduleContacts
	svc      *schedules.Service
}

// lateSchedules lets the schedules service be built after newEnv made the
// store it needs (the handler is built before).
type lateSchedules struct{ ScheduleService }

func newScheduleEnv(t *testing.T) *scheduleEnv {
	t.Helper()
	late := &lateSchedules{}
	fc := &fakeScheduleContacts{refs: map[domain.ContactID]contacts.Ref{}, owner: map[domain.ContactID]domain.UserID{}}
	e := newEnv(t, func(d *Deps) {
		d.Schedules = late
		d.Contacts = fc
	})
	svc, err := schedules.New(schedules.Deps{
		Repo: e.store, Remotes: e.remotes, Contacts: fc, Workers: apiWorkerChecker{e.wsvc},
		Waker: nopScheduleWaker{}, Clock: e.clock, IDs: &idgen.Sequential{},
	}, schedules.Config{})
	if err != nil {
		t.Fatal(err)
	}
	late.ScheduleService = svc
	return &scheduleEnv{testEnv: e, contacts: fc, svc: svc}
}

func (e *scheduleEnv) schedulesFor(a domain.Actor) syncrv1connect.ScheduleServiceClient {
	return syncrv1connect.NewScheduleServiceClient(e.clientWith(testActorHeader, actorHeader(a, "session")), e.srv.URL)
}

// addContact adds a contact of owner to the fake service and the store.
func (e *scheduleEnv) addContact(owner domain.Actor, name string) domain.ContactID {
	e.t.Helper()
	c := contacts.Contact{ID: domain.NewContactID(), OwnerID: owner.UserID, Name: name, Type: "webhook", Enabled: true, Version: 1}
	if err := e.store.CreateContact(context.Background(), c); err != nil {
		e.t.Fatal(err)
	}
	e.contacts.add(c, owner)
	return c.ID
}

// scheduleInput is a valid hourly schedule on the env's worker.
func (e *scheduleEnv) scheduleInput(name string) *syncrv1.ScheduleInput {
	return &syncrv1.ScheduleInput{
		Name: name, Cron: "0 * * * *", Timezone: "Europe/Berlin",
		Target:     &syncrv1.ScheduleTarget{Target: &syncrv1.ScheduleTarget_WorkerId{WorkerId: e.worker.String()}},
		MissedRun:  &syncrv1.MissedRunPolicy{Action: syncrv1.MissedRunAction_MISSED_RUN_ACTION_RUN_LATE, MaxDelay: durationpb.New(2 * time.Hour)},
		Overlap:    syncrv1.OverlapPolicy_OVERLAP_POLICY_QUEUE,
		Enabled:    true,
		Definition: testDefinition(),
	}
}

func (e *scheduleEnv) createSchedule(a domain.Actor, in *syncrv1.ScheduleInput) *syncrv1.Schedule {
	e.t.Helper()
	resp, err := e.schedulesFor(a).CreateSchedule(e.t.Context(), connect.NewRequest(&syncrv1.CreateScheduleRequest{Input: in}))
	if err != nil {
		e.t.Fatalf("CreateSchedule(%s): %v", in.GetName(), err)
	}
	return resp.Msg.GetSchedule()
}

func TestScheduleCRUD(t *testing.T) {
	t.Parallel()
	e := newScheduleEnv(t)
	ctx := t.Context()
	c := e.schedulesFor(testUser)
	remote := e.addRemote(testUser, "Photos", "s3")
	contact := e.addContact(testUser, "Ops")
	in := e.scheduleInput("Nightly")
	in.Definition.GetRcloneTransfer().Source = &syncrv1.JobLocation{RemoteId: remote.String(), Path: "pics"}
	in.Contacts = []*syncrv1.ScheduleContact{{ContactId: contact.String(), Events: []syncrv1.NotificationEvent{
		syncrv1.NotificationEvent_NOTIFICATION_EVENT_MISSED, syncrv1.NotificationEvent_NOTIFICATION_EVENT_FAILED,
	}}}
	sc := e.createSchedule(testUser, in)
	if sc.GetName() != "Nightly" || sc.GetOwnerId() != testUser.UserID.String() || sc.GetVersion() != 1 || !sc.GetEnabled() ||
		sc.GetTarget().GetWorkerId() != e.worker.String() || sc.GetOverlap() != syncrv1.OverlapPolicy_OVERLAP_POLICY_QUEUE ||
		sc.GetMissedRun().GetAction() != syncrv1.MissedRunAction_MISSED_RUN_ACTION_RUN_LATE ||
		sc.GetMissedRun().GetMaxDelay().AsDuration() != 2*time.Hour || sc.GetLastRun() != nil || sc.GetLastFiredAt() != nil {
		t.Errorf("CreateSchedule = %v", sc)
	}
	// 12:00 UTC is 13:00 in Berlin: the next hour is 14:00 CET.
	if got := sc.GetNextRunAt().AsTime(); !got.Equal(time.Date(2026, 11, 4, 13, 0, 0, 0, time.UTC)) {
		t.Errorf("next_run_at = %v", got)
	}
	if !proto.Equal(sc.GetDefinition(), in.GetDefinition()) {
		t.Errorf("definition = %v", sc.GetDefinition())
	}
	if len(sc.GetRemotes()) != 1 || sc.GetRemotes()[0].GetName() != "Photos" || sc.GetRemotes()[0].GetBackendType() != "s3" {
		t.Errorf("remotes = %v", sc.GetRemotes())
	}
	if l := sc.GetContacts(); len(l) != 1 || l[0].GetContactName() != "Ops" || l[0].GetChannelType() != "webhook" ||
		!slices.Equal(l[0].GetEvents(), []syncrv1.NotificationEvent{syncrv1.NotificationEvent_NOTIFICATION_EVENT_FAILED, syncrv1.NotificationEvent_NOTIFICATION_EVENT_MISSED}) {
		t.Errorf("contacts = %v", l)
	}

	// A label target, skip policy and default overlap.
	lab := e.scheduleInput("By label")
	lab.Target = &syncrv1.ScheduleTarget{Target: &syncrv1.ScheduleTarget_WorkerLabel{WorkerLabel: "NAS"}}
	lab.MissedRun = &syncrv1.MissedRunPolicy{Action: syncrv1.MissedRunAction_MISSED_RUN_ACTION_SKIP}
	lab.Overlap = syncrv1.OverlapPolicy_OVERLAP_POLICY_UNSPECIFIED
	lab.Enabled = false
	byLabel := e.createSchedule(testUser, lab)
	if byLabel.GetTarget().GetWorkerLabel() != "nas" || byLabel.GetOverlap() != syncrv1.OverlapPolicy_OVERLAP_POLICY_SKIP ||
		byLabel.GetMissedRun().GetMaxDelay() != nil || byLabel.GetNextRunAt() != nil {
		t.Errorf("CreateSchedule(label) = %v", byLabel)
	}

	got, err := c.GetSchedule(ctx, connect.NewRequest(&syncrv1.GetScheduleRequest{ScheduleId: sc.GetId()}))
	if err != nil || !proto.Equal(got.Msg.GetSchedule(), sc) {
		t.Errorf("GetSchedule = (%v, %v)", got, err)
	}
	list, err := c.ListSchedules(ctx, connect.NewRequest(&syncrv1.ListSchedulesRequest{}))
	if err != nil || len(list.Msg.GetSchedules()) != 2 || list.Msg.GetSchedules()[0].GetName() != "By label" {
		t.Errorf("ListSchedules = (%v, %v)", list, err)
	}
	byRemote, err := c.ListSchedules(ctx, connect.NewRequest(&syncrv1.ListSchedulesRequest{RemoteId: remote.String()}))
	if err != nil || len(byRemote.Msg.GetSchedules()) != 1 || byRemote.Msg.GetSchedules()[0].GetId() != sc.GetId() {
		t.Errorf("ListSchedules(remote) = (%v, %v)", byRemote, err)
	}
	byContact, err := c.ListSchedules(ctx, connect.NewRequest(&syncrv1.ListSchedulesRequest{ContactId: contact.String()}))
	if err != nil || len(byContact.Msg.GetSchedules()) != 1 {
		t.Errorf("ListSchedules(contact) = (%v, %v)", byContact, err)
	}
	_, err = c.ListSchedules(ctx, connect.NewRequest(&syncrv1.ListSchedulesRequest{RemoteId: "nope"}))
	wantCode(t, err, connect.CodeInvalidArgument)

	// Update: version-checked; a name taken by another schedule.
	upd := e.scheduleInput("Renamed")
	upd.Cron = "30 2 * * *"
	updated, err := c.UpdateSchedule(ctx, connect.NewRequest(&syncrv1.UpdateScheduleRequest{ScheduleId: sc.GetId(), Version: 1, Input: upd}))
	if err != nil || updated.Msg.GetSchedule().GetVersion() != 2 || updated.Msg.GetSchedule().GetCron() != "30 2 * * *" ||
		!updated.Msg.GetSchedule().GetNextRunAt().AsTime().Equal(time.Date(2026, 11, 5, 1, 30, 0, 0, time.UTC)) {
		t.Fatalf("UpdateSchedule = (%v, %v)", updated, err)
	}
	_, err = c.UpdateSchedule(ctx, connect.NewRequest(&syncrv1.UpdateScheduleRequest{ScheduleId: sc.GetId(), Version: 1, Input: upd}))
	wantCode(t, err, connect.CodeAborted)
	taken := e.scheduleInput("by LABEL")
	_, err = c.UpdateSchedule(ctx, connect.NewRequest(&syncrv1.UpdateScheduleRequest{ScheduleId: sc.GetId(), Version: 2, Input: taken}))
	wantCode(t, err, connect.CodeAlreadyExists)
	_, err = c.CreateSchedule(ctx, connect.NewRequest(&syncrv1.CreateScheduleRequest{Input: e.scheduleInput("renamed")}))
	wantCode(t, err, connect.CodeAlreadyExists)
	_, err = c.UpdateSchedule(ctx, connect.NewRequest(&syncrv1.UpdateScheduleRequest{ScheduleId: sc.GetId(), Input: upd}))
	wantCode(t, err, connect.CodeInvalidArgument)

	// Pause, resume.
	off, err := c.SetScheduleEnabled(ctx, connect.NewRequest(&syncrv1.SetScheduleEnabledRequest{ScheduleId: sc.GetId(), Enabled: false}))
	if err != nil || off.Msg.GetSchedule().GetEnabled() || off.Msg.GetSchedule().GetNextRunAt() != nil {
		t.Errorf("SetScheduleEnabled(false) = (%v, %v)", off, err)
	}
	on, err := c.SetScheduleEnabled(ctx, connect.NewRequest(&syncrv1.SetScheduleEnabledRequest{ScheduleId: sc.GetId(), Enabled: true}))
	if err != nil || !on.Msg.GetSchedule().GetEnabled() || on.Msg.GetSchedule().GetNextRunAt() == nil {
		t.Errorf("SetScheduleEnabled(true) = (%v, %v)", on, err)
	}

	// Run now; the run shows as the schedule's last run.
	run, err := c.RunScheduleNow(ctx, connect.NewRequest(&syncrv1.RunScheduleNowRequest{ScheduleId: sc.GetId()}))
	r := run.Msg.GetRun()
	if err != nil || r.GetTrigger() != syncrv1.ScheduleRunTrigger_SCHEDULE_RUN_TRIGGER_MANUAL ||
		r.GetStatus() != syncrv1.ScheduleRunStatus_SCHEDULE_RUN_STATUS_PENDING || r.GetScheduleId() != sc.GetId() ||
		!r.GetDeadline().AsTime().Equal(e.clock.Now().Add(2*time.Hour)) || r.GetJobId() != "" || r.GetWorkerId() != "" {
		t.Fatalf("RunScheduleNow = (%v, %v)", r, err)
	}
	got, _ = c.GetSchedule(ctx, connect.NewRequest(&syncrv1.GetScheduleRequest{ScheduleId: sc.GetId()}))
	if got.Msg.GetSchedule().GetLastRun().GetId() != r.GetId() {
		t.Errorf("last_run = %v", got.Msg.GetSchedule().GetLastRun())
	}

	if _, err := c.DeleteSchedule(ctx, connect.NewRequest(&syncrv1.DeleteScheduleRequest{ScheduleId: sc.GetId()})); err != nil {
		t.Fatal(err)
	}
	_, err = c.GetSchedule(ctx, connect.NewRequest(&syncrv1.GetScheduleRequest{ScheduleId: sc.GetId()}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = c.DeleteSchedule(ctx, connect.NewRequest(&syncrv1.DeleteScheduleRequest{ScheduleId: sc.GetId()}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = c.GetSchedule(ctx, connect.NewRequest(&syncrv1.GetScheduleRequest{}))
	wantCode(t, err, connect.CodeInvalidArgument)
}

func TestScheduleValidationFields(t *testing.T) {
	t.Parallel()
	e := newScheduleEnv(t)
	c := e.schedulesFor(testUser)
	tests := []struct {
		name   string
		mutate func(*syncrv1.ScheduleInput)
		want   []string
	}{
		{"no name", func(in *syncrv1.ScheduleInput) { in.Name = "" }, []string{"name"}},
		{"cron", func(in *syncrv1.ScheduleInput) { in.Cron = "@every 1h" }, []string{"cron"}},
		{"timezone", func(in *syncrv1.ScheduleInput) { in.Timezone = "Local" }, []string{"timezone"}},
		{"no target", func(in *syncrv1.ScheduleInput) { in.Target = nil }, []string{"target"}},
		{"bad worker id", func(in *syncrv1.ScheduleInput) {
			in.Target = &syncrv1.ScheduleTarget{Target: &syncrv1.ScheduleTarget_WorkerId{WorkerId: "nope"}}
		}, []string{"target.worker_id"}},
		{"unknown worker", func(in *syncrv1.ScheduleInput) {
			in.Target = &syncrv1.ScheduleTarget{Target: &syncrv1.ScheduleTarget_WorkerId{WorkerId: domain.NewWorkerID().String()}}
		}, []string{"target.worker_id"}},
		{"bad label", func(in *syncrv1.ScheduleInput) {
			in.Target = &syncrv1.ScheduleTarget{Target: &syncrv1.ScheduleTarget_WorkerLabel{WorkerLabel: "no spaces"}}
		}, []string{"target.worker_label"}},
		{"long delay", func(in *syncrv1.ScheduleInput) { in.MissedRun.MaxDelay = durationpb.New(8 * 24 * time.Hour) }, []string{"missed_run.max_delay"}},
		{"no definition", func(in *syncrv1.ScheduleInput) { in.Definition = nil }, []string{"definition"}},
		{"bad definition", func(in *syncrv1.ScheduleInput) {
			in.Definition.GetRcloneTransfer().Source = &syncrv1.JobLocation{Path: "relative"}
		}, []string{"definition.rclone_transfer.source.path"}},
		{"bad contact id", func(in *syncrv1.ScheduleInput) {
			in.Contacts = []*syncrv1.ScheduleContact{{ContactId: "x", Events: []syncrv1.NotificationEvent{syncrv1.NotificationEvent_NOTIFICATION_EVENT_FAILED}}}
		}, []string{"contacts[0].contact_id"}},
		{"no events", func(in *syncrv1.ScheduleInput) {
			in.Contacts = []*syncrv1.ScheduleContact{{ContactId: domain.NewContactID().String()}}
		}, []string{"contacts[0].events"}},
		{"test event", func(in *syncrv1.ScheduleInput) {
			in.Contacts = []*syncrv1.ScheduleContact{{ContactId: domain.NewContactID().String(), Events: []syncrv1.NotificationEvent{syncrv1.NotificationEvent_NOTIFICATION_EVENT_TEST}}}
		}, []string{"contacts[0].events"}},
	}
	for _, tt := range tests {
		in := e.scheduleInput("checked")
		tt.mutate(in)
		_, err := c.CreateSchedule(t.Context(), connect.NewRequest(&syncrv1.CreateScheduleRequest{Input: in}))
		if codeOf(t, err) != connect.CodeInvalidArgument {
			t.Errorf("%s: code %v (%v)", tt.name, codeOf(t, err), err)
			continue
		}
		var got []string
		for _, v := range violations(t, err) {
			got = append(got, v.GetField())
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("%s: fields = %v, want %v", tt.name, got, tt.want)
		}
	}
	_, err := c.CreateSchedule(t.Context(), connect.NewRequest(&syncrv1.CreateScheduleRequest{}))
	wantCode(t, err, connect.CodeInvalidArgument)
}

// TestScheduleOwnerScoping: another user, admins included, gets NotFound
// for a user's schedules and runs; linking another user's remote or contact
// is NotFound.
func TestScheduleOwnerScoping(t *testing.T) {
	t.Parallel()
	e := newScheduleEnv(t)
	ctx := t.Context()
	sc := e.createSchedule(testUser, e.scheduleInput("Mine"))
	if _, err := e.schedulesFor(testUser).RunScheduleNow(ctx, connect.NewRequest(&syncrv1.RunScheduleNowRequest{ScheduleId: sc.GetId()})); err != nil {
		t.Fatal(err)
	}
	id := sc.GetId()
	for _, other := range []domain.Actor{testOther, testAdmin} {
		c := e.schedulesFor(other)
		_, err := c.GetSchedule(ctx, connect.NewRequest(&syncrv1.GetScheduleRequest{ScheduleId: id}))
		wantCode(t, err, connect.CodeNotFound)
		_, err = c.UpdateSchedule(ctx, connect.NewRequest(&syncrv1.UpdateScheduleRequest{ScheduleId: id, Version: 1, Input: e.scheduleInput("x")}))
		wantCode(t, err, connect.CodeNotFound)
		_, err = c.SetScheduleEnabled(ctx, connect.NewRequest(&syncrv1.SetScheduleEnabledRequest{ScheduleId: id}))
		wantCode(t, err, connect.CodeNotFound)
		_, err = c.DeleteSchedule(ctx, connect.NewRequest(&syncrv1.DeleteScheduleRequest{ScheduleId: id}))
		wantCode(t, err, connect.CodeNotFound)
		_, err = c.RunScheduleNow(ctx, connect.NewRequest(&syncrv1.RunScheduleNowRequest{ScheduleId: id}))
		wantCode(t, err, connect.CodeNotFound)
		_, err = c.ListScheduleRuns(ctx, connect.NewRequest(&syncrv1.ListScheduleRunsRequest{ScheduleId: id}))
		wantCode(t, err, connect.CodeNotFound)
		if list, err := c.ListSchedules(ctx, connect.NewRequest(&syncrv1.ListSchedulesRequest{})); err != nil || len(list.Msg.GetSchedules()) != 0 {
			t.Errorf("ListSchedules(%s) = (%v, %v)", other.Role, list, err)
		}
	}
	if _, err := e.schedulesFor(testUser).GetSchedule(ctx, connect.NewRequest(&syncrv1.GetScheduleRequest{ScheduleId: id})); err != nil {
		t.Errorf("the owner's GetSchedule: %v", err)
	}

	theirRemote := e.addRemote(testOther, "Theirs", "s3")
	in := e.scheduleInput("Borrowed remote")
	in.Definition.GetRcloneTransfer().Source = &syncrv1.JobLocation{RemoteId: theirRemote.String(), Path: "x"}
	_, err := e.schedulesFor(testUser).CreateSchedule(ctx, connect.NewRequest(&syncrv1.CreateScheduleRequest{Input: in}))
	wantCode(t, err, connect.CodeNotFound)
	theirContact := e.addContact(testOther, "Theirs")
	in = e.scheduleInput("Borrowed contact")
	in.Contacts = []*syncrv1.ScheduleContact{{ContactId: theirContact.String(), Events: []syncrv1.NotificationEvent{syncrv1.NotificationEvent_NOTIFICATION_EVENT_FAILED}}}
	_, err = e.schedulesFor(testUser).CreateSchedule(ctx, connect.NewRequest(&syncrv1.CreateScheduleRequest{Input: in}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = e.schedulesFor(testUser).UpdateSchedule(ctx, connect.NewRequest(&syncrv1.UpdateScheduleRequest{ScheduleId: id, Version: 1, Input: in}))
	wantCode(t, err, connect.CodeNotFound)
}

func TestRunScheduleNowSkipWhileActive(t *testing.T) {
	t.Parallel()
	e := newScheduleEnv(t)
	ctx := t.Context()
	in := e.scheduleInput("Skip")
	in.Overlap = syncrv1.OverlapPolicy_OVERLAP_POLICY_SKIP
	sc := e.createSchedule(testUser, in)
	id, _ := domain.ParseScheduleID(sc.GetId())
	e.scheduledJob(testUser, id)
	_, err := e.schedulesFor(testUser).RunScheduleNow(ctx, connect.NewRequest(&syncrv1.RunScheduleNowRequest{ScheduleId: sc.GetId()}))
	wantCode(t, err, connect.CodeFailedPrecondition)
}

// scheduledJob queues a job of owner launched by schedule id.
func (e *scheduleEnv) scheduledJob(owner domain.Actor, id domain.ScheduleID) jobs.Job {
	e.t.Helper()
	j, err := e.jsvc.Create(e.t.Context(), owner, jobs.CreateParams{
		ScheduleID: id, WorkerID: e.worker, Kind: "rclone_transfer", Definition: []byte(`{"rclone_transfer":{"mode":"RCLONE_TRANSFER_MODE_COPY"}}`),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func TestListScheduleRunsPages(t *testing.T) {
	t.Parallel()
	e := newScheduleEnv(t)
	ctx := t.Context()
	in := e.scheduleInput("Pages")
	in.Overlap = syncrv1.OverlapPolicy_OVERLAP_POLICY_ALLOW
	sc := e.createSchedule(testUser, in)
	c := e.schedulesFor(testUser)
	var ids []string
	for range 3 {
		resp, err := c.RunScheduleNow(ctx, connect.NewRequest(&syncrv1.RunScheduleNowRequest{ScheduleId: sc.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		r := resp.Msg.GetRun()
		runID, _ := domain.ParseScheduleRunID(r.GetId())
		if _, err := e.svc.EndRun(ctx, schedules.Run{ID: runID, Status: schedules.RunPending}, schedules.RunMissed, schedules.ReasonWorkerOffline, "offline", false); err != nil {
			t.Fatal(err)
		}
		ids = append([]string{r.GetId()}, ids...)
	}
	p1, err := c.ListScheduleRuns(ctx, connect.NewRequest(&syncrv1.ListScheduleRunsRequest{ScheduleId: sc.GetId(), PageSize: 2}))
	if err != nil || len(p1.Msg.GetRuns()) != 2 || p1.Msg.GetRuns()[0].GetId() != ids[0] || p1.Msg.GetNextPageToken() == "" ||
		p1.Msg.GetRuns()[0].GetStatus() != syncrv1.ScheduleRunStatus_SCHEDULE_RUN_STATUS_MISSED || p1.Msg.GetRuns()[0].GetReason() != "worker_offline" {
		t.Fatalf("page 1 = (%v, %v)", p1, err)
	}
	p2, err := c.ListScheduleRuns(ctx, connect.NewRequest(&syncrv1.ListScheduleRunsRequest{ScheduleId: sc.GetId(), PageSize: 2, PageToken: p1.Msg.GetNextPageToken()}))
	if err != nil || len(p2.Msg.GetRuns()) != 1 || p2.Msg.GetRuns()[0].GetId() != ids[2] || p2.Msg.GetNextPageToken() != "" {
		t.Errorf("page 2 = (%v, %v)", p2, err)
	}
	for _, token := range []string{"garbage", encodePageToken(domain.NewJobID())} {
		_, err = c.ListScheduleRuns(ctx, connect.NewRequest(&syncrv1.ListScheduleRunsRequest{ScheduleId: sc.GetId(), PageToken: token}))
		wantCode(t, err, connect.CodeInvalidArgument)
	}
}

func TestPreviewSchedule(t *testing.T) {
	t.Parallel()
	e := newScheduleEnv(t)
	c := e.schedulesFor(testUser)
	resp, err := c.PreviewSchedule(t.Context(), connect.NewRequest(&syncrv1.PreviewScheduleRequest{
		Cron: "30  2 * * *", Timezone: "Europe/Berlin", Count: 2,
		After: timestamppb.New(time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC)),
	}))
	if err != nil || resp.Msg.GetNormalized() != "30 2 * * *" || len(resp.Msg.GetNextRuns()) != 2 ||
		!resp.Msg.GetNextRuns()[0].AsTime().Equal(time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC)) {
		t.Errorf("PreviewSchedule = (%v, %v)", resp, err)
	}
	if resp, err := c.PreviewSchedule(t.Context(), connect.NewRequest(&syncrv1.PreviewScheduleRequest{Cron: "@hourly", Timezone: "UTC"})); err != nil ||
		len(resp.Msg.GetNextRuns()) != schedules.DefaultPreview || !resp.Msg.GetNextRuns()[0].AsTime().Equal(e.clock.Now().Add(time.Hour)) {
		t.Errorf("PreviewSchedule(defaults) = (%v, %v)", resp, err)
	}
	_, err = c.PreviewSchedule(t.Context(), connect.NewRequest(&syncrv1.PreviewScheduleRequest{Cron: "0 2 * * *", Timezone: "Mars/Base"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	if v := violations(t, err); len(v) != 1 || v[0].GetField() != "timezone" {
		t.Errorf("violations = %v", v)
	}
}

// TestJobsOfSchedules: jobs show their schedule and are filtered by it.
func TestJobsOfSchedules(t *testing.T) {
	t.Parallel()
	e := newScheduleEnv(t)
	ctx := t.Context()
	sc := e.createSchedule(testUser, e.scheduleInput("Nightly"))
	other := e.createSchedule(testUser, e.scheduleInput("Other"))
	id, _ := domain.ParseScheduleID(sc.GetId())
	otherID, _ := domain.ParseScheduleID(other.GetId())
	j := e.scheduledJob(testUser, id)
	e.scheduledJob(testUser, otherID)
	manual, err := e.jobsFor(testUser).RunJob(ctx, connect.NewRequest(&syncrv1.RunJobRequest{WorkerId: e.worker.String(), Definition: testDefinition()}))
	if err != nil {
		t.Fatal(err)
	}
	list, err := e.jobsFor(testUser).ListJobs(ctx, connect.NewRequest(&syncrv1.ListJobsRequest{ScheduleId: sc.GetId()}))
	if err != nil || len(list.Msg.GetJobs()) != 1 || list.Msg.GetJobs()[0].GetId() != j.ID.String() {
		t.Fatalf("ListJobs(schedule) = (%v, %v)", list, err)
	}
	got := list.Msg.GetJobs()[0]
	if got.GetScheduleId() != sc.GetId() || got.GetSchedule().GetId() != sc.GetId() || got.GetSchedule().GetName() != "Nightly" {
		t.Errorf("job schedule = %q %v", got.GetScheduleId(), got.GetSchedule())
	}
	if m := manual.Msg.GetJob(); m.GetSchedule() != nil || m.GetScheduleId() != "" {
		t.Errorf("manual job schedule = %v", m.GetSchedule())
	}
	// An admin listing every user's jobs sees the schedule's name too.
	all, err := e.jobsFor(testAdmin).ListJobs(ctx, connect.NewRequest(&syncrv1.ListJobsRequest{AllUsers: true, ScheduleId: other.GetId()}))
	if err != nil || len(all.Msg.GetJobs()) != 1 || all.Msg.GetJobs()[0].GetSchedule().GetName() != "Other" {
		t.Errorf("ListJobs(admin, all users) = (%v, %v)", all, err)
	}
	// A deleted schedule's jobs keep existing without it.
	if _, err := e.schedulesFor(testUser).DeleteSchedule(ctx, connect.NewRequest(&syncrv1.DeleteScheduleRequest{ScheduleId: sc.GetId()})); err != nil {
		t.Fatal(err)
	}
	if g, err := e.jobsFor(testUser).GetJob(ctx, connect.NewRequest(&syncrv1.GetJobRequest{JobId: j.ID.String()})); err != nil ||
		g.Msg.GetJob().GetScheduleId() != "" || g.Msg.GetJob().GetSchedule() != nil {
		t.Errorf("job of a deleted schedule = (%v, %v)", g, err)
	}
	_, err = e.jobsFor(testUser).ListJobs(ctx, connect.NewRequest(&syncrv1.ListJobsRequest{ScheduleId: "nope"}))
	wantCode(t, err, connect.CodeInvalidArgument)
}

func TestScheduleServiceUnmounted(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := syncrv1connect.NewScheduleServiceClient(e.clientWith(testActorHeader, actorHeader(testUser, "session")), e.srv.URL)
	_, err := c.ListSchedules(t.Context(), connect.NewRequest(&syncrv1.ListSchedulesRequest{}))
	wantCode(t, err, connect.CodeUnimplemented)
}

// fakeScheduleContacts stands in for the contacts service: the schedules
// service's ContactChecker and the Names of Deps.Contacts. Other
// ContactService methods are not served.
type fakeScheduleContacts struct {
	ContactService

	mu    sync.Mutex
	refs  map[domain.ContactID]contacts.Ref
	owner map[domain.ContactID]domain.UserID
}

func (f *fakeScheduleContacts) add(c contacts.Contact, owner domain.Actor) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs[c.ID] = contacts.Ref{ID: c.ID, Name: c.Name, Type: c.Type}
	f.owner[c.ID] = owner.UserID
}

func (f *fakeScheduleContacts) Usable(_ context.Context, owner domain.UserID, ids []domain.ContactID) (map[domain.ContactID]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[domain.ContactID]string{}
	for _, id := range ids {
		if f.owner[id] != owner || owner.IsZero() {
			return nil, fmt.Errorf("contact %s: %w", id, domain.ErrNotFound)
		}
		out[id] = f.refs[id].Name
	}
	return out, nil
}

func (f *fakeScheduleContacts) Names(_ context.Context, ids []domain.ContactID) (map[domain.ContactID]contacts.Ref, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[domain.ContactID]contacts.Ref{}
	for _, id := range ids {
		if r, ok := f.refs[id]; ok {
			out[id] = r
		}
	}
	return out, nil
}

// apiWorkerChecker adapts the workers service, as the composition root does.
type apiWorkerChecker struct{ w *workers.Service }

func (c apiWorkerChecker) CheckWorker(ctx context.Context, id domain.WorkerID) error {
	_, err := c.w.Get(ctx, id)
	return err
}

type nopScheduleWaker struct{}

func (nopScheduleWaker) Wake() {}
