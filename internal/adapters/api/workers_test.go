package api

import (
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

func TestCreateEnrollmentToken(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.workersClient()

	resp, err := c.CreateEnrollmentToken(t.Context(), connect.NewRequest(&syncrv1.CreateEnrollmentTokenRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.Msg.GetToken(), "syncr_et_") {
		t.Errorf("token = %q", resp.Msg.GetToken())
	}
	if want := e.clock.Now().Add(time.Hour); !resp.Msg.GetExpiresAt().AsTime().Equal(want) {
		t.Errorf("default expiry = %v, want %v", resp.Msg.GetExpiresAt().AsTime(), want)
	}
	if got := resp.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}

	resp2, err := c.CreateEnrollmentToken(t.Context(), connect.NewRequest(&syncrv1.CreateEnrollmentTokenRequest{Ttl: durationpb.New(10 * time.Minute)}))
	if err != nil {
		t.Fatal(err)
	}
	if want := e.clock.Now().Add(10 * time.Minute); !resp2.Msg.GetExpiresAt().AsTime().Equal(want) {
		t.Errorf("custom expiry = %v, want %v", resp2.Msg.GetExpiresAt().AsTime(), want)
	}
	if resp2.Msg.GetToken() == resp.Msg.GetToken() {
		t.Error("two tokens are equal")
	}

	// The token is redeemable (so it is a real one) and appears nowhere else:
	// the listing of workers does not carry it.
	enr, err := e.wsvc.Enroll(t.Context(), workers.EnrollParams{Token: resp.Msg.GetToken(), Name: "enrolled"})
	if err != nil {
		t.Fatal(err)
	}
	list, err := c.ListWorkers(t.Context(), connect.NewRequest(&syncrv1.ListWorkersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list.Msg.String(), "enrolled") || strings.Contains(list.Msg.String(), resp.Msg.GetToken()) || strings.Contains(list.Msg.String(), enr.Secret) {
		t.Errorf("ListWorkers = %v", list.Msg)
	}
}

func TestCreateEnrollmentTokenInvalid(t *testing.T) {
	t.Parallel()
	c := newEnv(t).workersClient()
	for name, ttl := range map[string]*durationpb.Duration{
		"too short": durationpb.New(30 * time.Second),
		"too long":  durationpb.New(30 * 24 * time.Hour),
		"negative":  durationpb.New(-time.Hour),
		"invalid":   {Seconds: 1 << 40, Nanos: -5},
	} {
		_, err := c.CreateEnrollmentToken(t.Context(), connect.NewRequest(&syncrv1.CreateEnrollmentTokenRequest{Ttl: ttl}))
		if codeOf(t, err) != connect.CodeInvalidArgument {
			t.Errorf("%s: err = %v, want InvalidArgument", name, err)
		}
	}
	_, err := c.CreateEnrollmentToken(t.Context(), connect.NewRequest(&syncrv1.CreateEnrollmentTokenRequest{Ttl: durationpb.New(time.Second)}))
	if err == nil || !strings.Contains(err.Error(), "ttl") {
		t.Errorf("message does not explain: %v", err)
	}
}

func TestRegisterPassiveWorker(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.workersClient()
	valid := func() *syncrv1.RegisterPassiveWorkerRequest {
		return &syncrv1.RegisterPassiveWorkerRequest{
			Name: " passive-1 ", Url: "https://worker.example:7443/", CertFingerprint: testFP, PairingSecret: pairingSecret,
		}
	}

	resp, err := c.RegisterPassiveWorker(t.Context(), connect.NewRequest(valid()))
	if err != nil {
		t.Fatal(err)
	}
	w := resp.Msg.GetWorker()
	if w.GetName() != "passive-1" || w.GetMode() != syncrv1.WorkerMode_WORKER_MODE_PASSIVE ||
		w.GetUrl() != "https://worker.example:7443" || w.GetCertFingerprint() != testFP ||
		w.GetStatus() != syncrv1.WorkerStatus_WORKER_STATUS_PENDING || w.GetLastSeen() != nil ||
		!w.GetCreatedAt().AsTime().Equal(e.clock.Now()) || w.GetInfo() != nil {
		t.Errorf("worker = %v", w)
	}
	if strings.Contains(resp.Msg.String(), pairingSecret) {
		t.Error("the pairing secret is echoed")
	}
	got, err := c.GetWorker(t.Context(), connect.NewRequest(&syncrv1.GetWorkerRequest{WorkerId: w.GetId()}))
	if err != nil || strings.Contains(got.Msg.String(), pairingSecret) {
		t.Errorf("GetWorker = %v, %v", got, err)
	}

	// Colons and upper case in the fingerprint are normalized (see the
	// field's documentation).
	req := valid()
	req.CertFingerprint = strings.ToUpper(strings.Repeat("ab:", 31) + "ab")
	resp, err = c.RegisterPassiveWorker(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Repeat("ab", 32); resp.Msg.GetWorker().GetCertFingerprint() != want {
		t.Errorf("fingerprint = %q, want %q", resp.Msg.GetWorker().GetCertFingerprint(), want)
	}

	tests := []struct {
		name   string
		mutate func(*syncrv1.RegisterPassiveWorkerRequest)
		want   string
	}{
		{"http url", func(r *syncrv1.RegisterPassiveWorkerRequest) { r.Url = "http://worker.example:7443" }, "https"},
		{"no host", func(r *syncrv1.RegisterPassiveWorkerRequest) { r.Url = "https://" }, "host"},
		{"url with credentials", func(r *syncrv1.RegisterPassiveWorkerRequest) { r.Url = "https://u:p@worker.example" }, "credentials"},
		{"url with query", func(r *syncrv1.RegisterPassiveWorkerRequest) { r.Url = "https://worker.example/?a=b" }, "query"},
		{"empty url", func(r *syncrv1.RegisterPassiveWorkerRequest) { r.Url = "" }, "https"},
		{"short fingerprint", func(r *syncrv1.RegisterPassiveWorkerRequest) { r.CertFingerprint = testFP[:63] }, "fingerprint"},
		{"non-hex fingerprint", func(r *syncrv1.RegisterPassiveWorkerRequest) { r.CertFingerprint = strings.Repeat("z", 64) }, "fingerprint"},
		{"empty fingerprint", func(r *syncrv1.RegisterPassiveWorkerRequest) { r.CertFingerprint = "" }, "fingerprint"},
		{"short secret", func(r *syncrv1.RegisterPassiveWorkerRequest) { r.PairingSecret = "too-short" }, "pairing secret"},
		{"long secret", func(r *syncrv1.RegisterPassiveWorkerRequest) { r.PairingSecret = strings.Repeat("a", 513) }, "pairing secret"},
		{"secret with space", func(r *syncrv1.RegisterPassiveWorkerRequest) { r.PairingSecret = pairingSecret + " x" }, "printable"},
		{"secret with newline", func(r *syncrv1.RegisterPassiveWorkerRequest) { r.PairingSecret = pairingSecret + "\n" }, "printable"},
		{"empty name", func(r *syncrv1.RegisterPassiveWorkerRequest) { r.Name = "  " }, "name"},
		{"long name", func(r *syncrv1.RegisterPassiveWorkerRequest) { r.Name = strings.Repeat("n", 65) }, "name"},
		{"control name", func(r *syncrv1.RegisterPassiveWorkerRequest) { r.Name = "a\x00b" }, "name"},
	}
	for _, tt := range tests {
		req := valid()
		tt.mutate(req)
		_, err := c.RegisterPassiveWorker(t.Context(), connect.NewRequest(req))
		if codeOf(t, err) != connect.CodeInvalidArgument {
			t.Errorf("%s: err = %v, want InvalidArgument", tt.name, err)
			continue
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: message %q does not mention %q", tt.name, err, tt.want)
		}
		if strings.Contains(err.Error(), pairingSecret) {
			t.Errorf("%s: the error echoes the pairing secret", tt.name)
		}
	}
}

func TestListGetRenameRemoveWorkers(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.workersClient()
	ctx := t.Context()

	reg := func(name string) string {
		t.Helper()
		resp, err := c.RegisterPassiveWorker(ctx, connect.NewRequest(&syncrv1.RegisterPassiveWorkerRequest{
			Name: name, Url: "https://" + strings.ToLower(name) + ".example", CertFingerprint: testFP, PairingSecret: pairingSecret,
		}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetWorker().GetId()
	}
	reg("bravo")
	a := reg("Alpha")

	list, err := c.ListWorkers(ctx, connect.NewRequest(&syncrv1.ListWorkersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, w := range list.Msg.GetWorkers() {
		names = append(names, w.GetName())
	}
	// Sorted by name regardless of case; "worker" is newEnv's active worker.
	if got, want := strings.Join(names, ","), "Alpha,bravo,worker"; got != want {
		t.Errorf("names = %s, want %s", got, want)
	}

	got, err := c.GetWorker(ctx, connect.NewRequest(&syncrv1.GetWorkerRequest{WorkerId: a}))
	if err != nil || got.Msg.GetWorker().GetName() != "Alpha" {
		t.Fatalf("GetWorker = %v, %v", got, err)
	}

	ren, err := c.RenameWorker(ctx, connect.NewRequest(&syncrv1.RenameWorkerRequest{WorkerId: a, Name: "  Zulu "}))
	if err != nil || ren.Msg.GetWorker().GetName() != "Zulu" || ren.Msg.GetWorker().GetId() != a {
		t.Fatalf("RenameWorker = %v, %v", ren, err)
	}
	_, err = c.RenameWorker(ctx, connect.NewRequest(&syncrv1.RenameWorkerRequest{WorkerId: a, Name: ""}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = c.RenameWorker(ctx, connect.NewRequest(&syncrv1.RenameWorkerRequest{WorkerId: domain.NewWorkerID().String(), Name: "x"}))
	wantCode(t, err, connect.CodeNotFound)

	// Removing ends the worker's unfinished jobs and hides it.
	j := e.runJob()
	if _, err := c.RemoveWorker(ctx, connect.NewRequest(&syncrv1.RemoveWorkerRequest{WorkerId: e.worker.String()})); err != nil {
		t.Fatal(err)
	}
	job, err := e.jobsClient().GetJob(ctx, connect.NewRequest(&syncrv1.GetJobRequest{JobId: j.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	if job.Msg.GetJob().GetStatus() != syncrv1.JobStatus_JOB_STATUS_FAILED || job.Msg.GetJob().GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_WORKER_REMOVED {
		t.Errorf("job of the removed worker = %v", job.Msg.GetJob())
	}
	_, err = c.GetWorker(ctx, connect.NewRequest(&syncrv1.GetWorkerRequest{WorkerId: e.worker.String()}))
	wantCode(t, err, connect.CodeNotFound)
	if _, err := c.RemoveWorker(ctx, connect.NewRequest(&syncrv1.RemoveWorkerRequest{WorkerId: e.worker.String()})); err != nil {
		t.Errorf("removing twice: %v", err)
	}
	_, err = c.RemoveWorker(ctx, connect.NewRequest(&syncrv1.RemoveWorkerRequest{WorkerId: domain.NewWorkerID().String()}))
	wantCode(t, err, connect.CodeNotFound)
	list, err = c.ListWorkers(ctx, connect.NewRequest(&syncrv1.ListWorkersRequest{}))
	if err != nil || len(list.Msg.GetWorkers()) != 2 {
		t.Errorf("ListWorkers after remove = %v, %v", list, err)
	}
	_, err = e.jobsClient().RunJob(ctx, connect.NewRequest(&syncrv1.RunJobRequest{WorkerId: e.worker.String(), Definition: testDefinition()}))
	wantCode(t, err, connect.CodeNotFound)
}

func TestWorkerIDValidation(t *testing.T) {
	t.Parallel()
	c := newEnv(t).workersClient()
	for _, id := range []string{"", "not-a-uuid", "00000000-0000-0000-0000-000000000000", strings.Repeat("a", 1000)} {
		_, err := c.GetWorker(t.Context(), connect.NewRequest(&syncrv1.GetWorkerRequest{WorkerId: id}))
		wantCode(t, err, connect.CodeInvalidArgument)
		_, err = c.RenameWorker(t.Context(), connect.NewRequest(&syncrv1.RenameWorkerRequest{WorkerId: id, Name: "x"}))
		wantCode(t, err, connect.CodeInvalidArgument)
		_, err = c.RemoveWorker(t.Context(), connect.NewRequest(&syncrv1.RemoveWorkerRequest{WorkerId: id}))
		wantCode(t, err, connect.CodeInvalidArgument)
	}
}

func TestWorkerStatusFollowsTheClock(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.workersClient()
	status := func() syncrv1.WorkerStatus {
		t.Helper()
		resp, err := c.GetWorker(t.Context(), connect.NewRequest(&syncrv1.GetWorkerRequest{WorkerId: e.worker.String()}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetWorker().GetStatus()
	}
	if got := status(); got != syncrv1.WorkerStatus_WORKER_STATUS_PENDING {
		t.Errorf("never seen: %v", got)
	}
	if err := e.store.TouchWorker(t.Context(), e.worker, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if got := status(); got != syncrv1.WorkerStatus_WORKER_STATUS_ONLINE {
		t.Errorf("just seen: %v", got)
	}
	e.clock.Advance(time.Minute)
	if got := status(); got != syncrv1.WorkerStatus_WORKER_STATUS_OFFLINE {
		t.Errorf("a minute later: %v", got)
	}
	resp, err := c.GetWorker(t.Context(), connect.NewRequest(&syncrv1.GetWorkerRequest{WorkerId: e.worker.String()}))
	if err != nil || resp.Msg.GetWorker().GetLastSeen() == nil {
		t.Errorf("last_seen unset: %v, %v", resp, err)
	}
}
