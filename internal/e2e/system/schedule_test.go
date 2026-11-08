//go:build integration

package system

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
)

// hookSecret is the webhook contact's signing secret (Standard Webhooks:
// whsec_ and the base64 key; a test value).
const hookSecret = "whsec_c3luY3ItZTJlLXdlYmhvb2stc2lnbmluZy1rZXk="

// hookCall is one request the webhook receiver got.
type hookCall struct {
	id, timestamp, signature string
	body                     []byte
}

// verify checks the Standard Webhooks signature of the call.
func (h hookCall) verify(t *testing.T) {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(hookSecret, "whsec_"))
	if err != nil {
		t.Fatal(err)
	}
	ts, err := strconv.ParseInt(h.timestamp, 10, 64)
	if err != nil || time.Since(time.Unix(ts, 0)).Abs() > 5*time.Minute {
		t.Errorf("webhook-timestamp %q is not current", h.timestamp)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(h.id + "." + h.timestamp + "."))
	mac.Write(h.body)
	want := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	ok := false
	for _, sig := range strings.Fields(h.signature) {
		ok = ok || hmac.Equal([]byte(sig), []byte(want))
	}
	if !ok {
		t.Errorf("webhook-signature %q does not verify (want %q)", h.signature, want)
	}
}

// TestSystemScheduleFires: a schedule running every minute on an active
// worker copies into MinIO; its job runs on the real worker and the signed
// "succeeded" webhook arrives. With the worker stopped (and offline) the
// next occurrence stays pending; once the worker is back, the run launches
// within its deadline.
func TestSystemScheduleFires(t *testing.T) {
	t.Parallel()
	rclone, restic := requirements(t)
	cbin, wbin := binaries(t)
	endpoint := startMinIO(t)

	calls := make(chan hookCall, 64)
	hook := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls <- hookCall{id: r.Header.Get("Webhook-Id"), timestamp: r.Header.Get("Webhook-Timestamp"), signature: r.Header.Get("Webhook-Signature"), body: b}
	}))
	t.Cleanup(hook.Close)

	// The receiver is on loopback, which only the "any" policy allows.
	c := newController(t, cbin, "SYNCR_SCHEDULES_TICK=1s", "SYNCR_NOTIFY_NETWORK_POLICY=any")
	w := setupWorker(t, c, wbin, rclone, restic, false)
	t.Cleanup(func() { killMatching(t, w.dir) })
	w.start()
	c.waitOnline(w.id)

	src := filepath.Join(w.root, "src")
	writeRandom(t, filepath.Join(src, "a.bin"), 64<<10)
	minio := c.createRemote(&syncrv1.RemoteInput{
		Name: "MinIO", BackendType: "s3",
		Options: map[string]string{
			"provider": "Minio", "endpoint": endpoint, "region": "us-east-1",
			"access_key_id": minioUser, "bucket": "syncr-sched", "force_path_style": "true",
		},
		Secrets: map[string]string{"secret_access_key": minioPassword},
	})
	client := c.client()
	contacts := syncrv1connect.NewContactServiceClient(client, c.base)
	contact, err := contacts.CreateContact(t.Context(), connect.NewRequest(&syncrv1.CreateContactRequest{Input: &syncrv1.ContactInput{
		Name: "Receiver", ChannelType: "webhook", Enabled: true,
		Secrets: map[string]string{"url": hook.URL + "/syncr", "signing_secret": hookSecret},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	schedules := syncrv1connect.NewScheduleServiceClient(client, c.base)
	created, err := schedules.CreateSchedule(t.Context(), connect.NewRequest(&syncrv1.CreateScheduleRequest{Input: &syncrv1.ScheduleInput{
		Name: "Every minute", Cron: "* * * * *", Timezone: "UTC", Enabled: true,
		Target: &syncrv1.ScheduleTarget{Target: &syncrv1.ScheduleTarget_WorkerId{WorkerId: w.id}},
		MissedRun: &syncrv1.MissedRunPolicy{
			Action: syncrv1.MissedRunAction_MISSED_RUN_ACTION_RUN_LATE, MaxDelay: durationpb.New(10 * time.Minute),
		},
		Overlap:    syncrv1.OverlapPolicy_OVERLAP_POLICY_SKIP,
		Definition: copyDef(local(src), onRemote(minio, "scheduled"), 0),
		Contacts: []*syncrv1.ScheduleContact{{
			ContactId: contact.Msg.GetContact().GetId(), Events: []syncrv1.NotificationEvent{syncrv1.NotificationEvent_NOTIFICATION_EVENT_SUCCEEDED},
		}},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	scheduleID := created.Msg.GetSchedule().GetId()

	// Within 75 s the first occurrence's job ran on the worker.
	first := waitScheduledJob(t, c, scheduleID, 75*time.Second)
	j, _ := c.watch(first.GetId())
	wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
	if j.GetSchedule().GetName() != "Every minute" || j.GetWorkerId() != w.id || j.GetStats().GetFiles() != 1 {
		t.Errorf("scheduled job = %v", j)
	}

	// The signed notification of its success arrives.
	deadline := time.After(waitLong)
	for found := false; !found; {
		select {
		case call := <-calls:
			var msg struct {
				ID       string                      `json:"id"`
				Event    string                      `json:"event"`
				Job      struct{ ID, Status string } `json:"job"`
				Schedule struct{ ID string }         `json:"schedule"`
			}
			if err := json.Unmarshal(call.body, &msg); err != nil {
				t.Fatalf("webhook body %q: %v", call.body, err)
			}
			if msg.Job.ID != first.GetId() {
				continue
			}
			found = true
			call.verify(t)
			if msg.Event != "succeeded" || msg.Job.Status != "succeeded" || msg.Schedule.ID != scheduleID || msg.ID == "" || msg.ID != call.id {
				t.Errorf("notification = %s (webhook-id %q)", call.body, call.id)
			}
		case <-deadline:
			t.Fatalf("no notification of job %s within %s", first.GetId(), waitLong)
		}
	}

	// Stop the worker; once it is offline, the next occurrence waits.
	if err := w.proc.stop(30 * time.Second); err != nil {
		t.Fatalf("worker stop: %v", err)
	}
	waitWorkerStatus(t, c, w.id, syncrv1.WorkerStatus_WORKER_STATUS_OFFLINE, waitLong)
	offline := time.Now()
	pending := waitRun(t, schedules, scheduleID, 75*time.Second, func(r *syncrv1.ScheduleRun) bool {
		return r.GetStatus() == syncrv1.ScheduleRunStatus_SCHEDULE_RUN_STATUS_PENDING && r.GetScheduledFor().AsTime().After(offline.Add(-time.Second))
	})
	before := scheduledJobs(t, c, scheduleID)
	time.Sleep(3 * time.Second) // three scheduler ticks
	if got := waitRun(t, schedules, scheduleID, time.Second, func(r *syncrv1.ScheduleRun) bool { return r.GetId() == pending.GetId() }); got.GetStatus() !=
		syncrv1.ScheduleRunStatus_SCHEDULE_RUN_STATUS_PENDING || len(scheduledJobs(t, c, scheduleID)) != len(before) {
		t.Fatalf("with the worker offline the run is %v", got)
	}

	// The worker comes back: the run launches within its deadline.
	w.start()
	c.waitOnline(w.id)
	launched := waitRun(t, schedules, scheduleID, waitLong, func(r *syncrv1.ScheduleRun) bool {
		return r.GetId() == pending.GetId() && r.GetStatus() != syncrv1.ScheduleRunStatus_SCHEDULE_RUN_STATUS_PENDING
	})
	if launched.GetStatus() != syncrv1.ScheduleRunStatus_SCHEDULE_RUN_STATUS_LAUNCHED || launched.GetJobId() == "" ||
		!time.Now().Before(launched.GetDeadline().AsTime()) {
		t.Fatalf("the waiting run = %v", launched)
	}
	j, _ = c.watch(launched.GetJobId())
	wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
	if strings.Contains(c.proc.out.String(), minioPassword) || strings.Contains(c.proc.out.String(), hookSecret) {
		t.Error("a secret appears in the controller's log")
	}
}

// scheduledJobs lists the jobs of a schedule.
func scheduledJobs(t *testing.T, c *controller, scheduleID string) []*syncrv1.Job {
	t.Helper()
	resp, err := c.jobs.ListJobs(t.Context(), connect.NewRequest(&syncrv1.ListJobsRequest{ScheduleId: scheduleID}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetJobs()
}

// waitScheduledJob waits up to d for the schedule's first job.
func waitScheduledJob(t *testing.T, c *controller, scheduleID string, d time.Duration) *syncrv1.Job {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if js := scheduledJobs(t, c, scheduleID); len(js) > 0 {
			return js[len(js)-1]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no job of schedule %s within %s", scheduleID, d)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitRun waits up to d for a run of the schedule satisfying ok.
func waitRun(t *testing.T, sc syncrv1connect.ScheduleServiceClient, scheduleID string, d time.Duration, ok func(*syncrv1.ScheduleRun) bool) *syncrv1.ScheduleRun {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		resp, err := sc.ListScheduleRuns(t.Context(), connect.NewRequest(&syncrv1.ListScheduleRunsRequest{ScheduleId: scheduleID}))
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range resp.Msg.GetRuns() {
			if ok(r) {
				return r
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no matching run of schedule %s within %s: %v", scheduleID, d, resp.Msg.GetRuns())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitWorkerStatus waits up to d for the worker's connection status.
func waitWorkerStatus(t *testing.T, c *controller, id string, want syncrv1.WorkerStatus, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		resp, err := c.admin.GetWorker(t.Context(), connect.NewRequest(&syncrv1.GetWorkerRequest{WorkerId: id}))
		if err == nil && resp.Msg.GetWorker().GetStatus() == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker %s is not %s within %s (%v, %v)", id, want, d, resp, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
