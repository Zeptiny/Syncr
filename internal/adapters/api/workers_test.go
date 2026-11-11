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
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/platform/version"
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
	req.Name = "passive-2" // names are unique among live workers
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

func TestCreateEnrollmentTokenInstallCommands(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	create := func(e *testEnv, controllerURL string) (*syncrv1.CreateEnrollmentTokenResponse, error) {
		t.Helper()
		resp, err := e.workersClient().CreateEnrollmentToken(ctx, connect.NewRequest(&syncrv1.CreateEnrollmentTokenRequest{ControllerUrl: controllerURL}))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}

	// Without a public URL the browser's origin is used.
	e := newEnv(t)
	msg, err := create(e, "https://syncr.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(msg.GetInstallCommands()); n != 4 {
		t.Fatalf("%d install commands, want 4", n)
	}
	for _, c := range msg.GetInstallCommands() {
		if !strings.Contains(c.GetCommand(), "--controller 'https://syncr.example.com' --token '"+msg.GetToken()+"'") {
			t.Errorf("%s: %s", c.GetTitle(), c.GetCommand())
		}
	}
	if msg, err := create(e, ""); err != nil || len(msg.GetInstallCommands()) != 0 || msg.GetToken() == "" {
		t.Errorf("without a URL: %v (%v), want a token and no commands", msg, err)
	}
	// An unusable origin is refused before a token is made.
	for _, bad := range []string{"http://syncr.lan:8080", "https://x.example.com/'; rm -rf ~", "not a url"} {
		_, err := create(e, bad)
		wantCode(t, err, connect.CodeInvalidArgument)
		if v := violations(t, err); len(v) != 1 || v[0].GetField() != "controller_url" {
			t.Errorf("%q: violations %v", bad, v)
		}
	}

	// The public URL wins over the request's.
	pub := newEnv(t, func(d *Deps) {
		d.Install = InstallConfig{PublicURL: "https://public.example.com", DownloadBaseURL: "https://mirror.example.com/r"}
	})
	msg, err = create(pub, "http://localhost:9999")
	if err != nil || len(msg.GetInstallCommands()) != 4 {
		t.Fatalf("public URL: %v (%v)", msg, err)
	}
	if c := msg.GetInstallCommands()[0].GetCommand(); !strings.Contains(c, "'https://public.example.com'") || !strings.Contains(c, "https://mirror.example.com/r/syncr-worker_linux_") || strings.Contains(c, "localhost") {
		t.Errorf("public URL command = %s", c)
	}
	if msg, err := create(pub, "garbage"); err != nil || len(msg.GetInstallCommands()) != 4 {
		t.Errorf("the request's URL is ignored with a public URL: %v (%v)", msg, err)
	}

	// A misconfigured public URL still yields a token, without commands.
	broken := newEnv(t, func(d *Deps) { d.Install = InstallConfig{PublicURL: "http://syncr.lan"} })
	msg, err = create(broken, "https://syncr.example.com")
	if err != nil || msg.GetToken() == "" || len(msg.GetInstallCommands()) != 0 {
		t.Errorf("broken public URL: %v (%v)", msg, err)
	}
	if !strings.Contains(broken.logs.String(), "public_url") {
		t.Errorf("no warning logged: %s", broken.logs.String())
	}
	if strings.Contains(broken.logs.String(), msg.GetToken()) {
		t.Error("the token is in the logs")
	}
}

func TestGetPassiveInstallCommands(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	resp, err := e.workersClient().GetPassiveInstallCommands(t.Context(), connect.NewRequest(&syncrv1.GetPassiveInstallCommandsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(resp.Msg.GetCommands()); n != 4 {
		t.Fatalf("%d commands, want 4", n)
	}
	for _, c := range resp.Msg.GetCommands() {
		if !strings.Contains(c.GetCommand(), "setup ") || !strings.Contains(c.GetCommand(), "--passive") {
			t.Errorf("%s: %s", c.GetTitle(), c.GetCommand())
		}
	}
	bad := newEnv(t, func(d *Deps) { d.Install = InstallConfig{DownloadBaseURL: "ftp://mirror"} })
	_, err = bad.workersClient().GetPassiveInstallCommands(t.Context(), connect.NewRequest(&syncrv1.GetPassiveInstallCommandsRequest{}))
	wantCode(t, err, connect.CodeInternal)
}

// TestWorkersPageAccess: every user may list and inspect workers (the
// workers page); enrolling, registering and managing them is for admins.
func TestWorkersPageAccess(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	u := e.workersFor(testUser)
	list, err := u.ListWorkers(ctx, connect.NewRequest(&syncrv1.ListWorkersRequest{}))
	if err != nil || len(list.Msg.GetWorkers()) != 1 {
		t.Errorf("user ListWorkers = %v (%v)", list, err)
	}
	if _, err := u.GetWorker(ctx, connect.NewRequest(&syncrv1.GetWorkerRequest{WorkerId: e.worker.String()})); err != nil {
		t.Errorf("user GetWorker: %v", err)
	}
	_, err = u.CreateEnrollmentToken(ctx, connect.NewRequest(&syncrv1.CreateEnrollmentTokenRequest{}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = u.GetPassiveInstallCommands(ctx, connect.NewRequest(&syncrv1.GetPassiveInstallCommandsRequest{}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = u.RemoveWorker(ctx, connect.NewRequest(&syncrv1.RemoveWorkerRequest{WorkerId: e.worker.String()}))
	wantCode(t, err, connect.CodePermissionDenied)

	// A worker's job history is ListJobs{worker_id}: a user sees their own
	// jobs on it, an admin everyone's with all_users.
	mine, err := e.runAs(testUser, testDefinition())
	if err != nil {
		t.Fatal(err)
	}
	e.runJob() // the admin's
	for _, tt := range []struct {
		actor domain.Actor
		all   bool
		want  int
	}{{testUser, false, 1}, {testOther, false, 0}, {testAdmin, false, 1}, {testAdmin, true, 2}} {
		resp, err := e.jobsFor(tt.actor).ListJobs(ctx, connect.NewRequest(&syncrv1.ListJobsRequest{WorkerId: e.worker.String(), AllUsers: tt.all}))
		if err != nil || len(resp.Msg.GetJobs()) != tt.want {
			t.Errorf("%s (all %v): %d jobs (%v), want %d", tt.actor.Role, tt.all, len(resp.Msg.GetJobs()), err, tt.want)
		}
		if tt.actor == testUser && resp.Msg.GetJobs()[0].GetId() != mine.GetId() {
			t.Errorf("user history = %v", resp.Msg.GetJobs())
		}
	}
}

func TestSetWorkerLabels(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	admin := e.workersClient()
	id := e.worker.String()

	resp, err := admin.SetWorkerLabels(t.Context(), connect.NewRequest(&syncrv1.SetWorkerLabelsRequest{WorkerId: id, Labels: []string{"NAS", "linux", "nas"}}))
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Msg.GetWorker().GetLabels(); strings.Join(got, ",") != "linux,nas" {
		t.Errorf("labels = %q", got)
	}
	got, err := admin.GetWorker(t.Context(), connect.NewRequest(&syncrv1.GetWorkerRequest{WorkerId: id}))
	if err != nil || strings.Join(got.Msg.GetWorker().GetLabels(), ",") != "linux,nas" {
		t.Errorf("GetWorker labels = %v, %v", got, err)
	}

	// Users may list workers by label but not set labels.
	user := e.workersFor(testUser)
	_, err = user.SetWorkerLabels(t.Context(), connect.NewRequest(&syncrv1.SetWorkerLabelsRequest{WorkerId: id, Labels: []string{"mine"}}))
	wantCode(t, err, connect.CodePermissionDenied)
	other := e.addWorker()
	for label, want := range map[string]int{"nas": 1, "NAS": 1, "windows": 0, "": 2} {
		list, err := user.ListWorkers(t.Context(), connect.NewRequest(&syncrv1.ListWorkersRequest{Label: label}))
		if err != nil || len(list.Msg.GetWorkers()) != want {
			t.Errorf("ListWorkers(label %q) = %v, %v; want %d workers", label, list, err, want)
		}
	}
	_, err = user.ListWorkers(t.Context(), connect.NewRequest(&syncrv1.ListWorkersRequest{Label: "not a label"}))
	wantCode(t, err, connect.CodeInvalidArgument)

	// Invalid labels name the field; unknown and removed workers are not
	// found.
	_, err = admin.SetWorkerLabels(t.Context(), connect.NewRequest(&syncrv1.SetWorkerLabelsRequest{WorkerId: id, Labels: []string{"ok", "-bad"}}))
	wantCode(t, err, connect.CodeInvalidArgument)
	if err == nil || !strings.Contains(err.Error(), "labels[1]") {
		t.Errorf("message does not name the field: %v", err)
	}
	_, err = admin.SetWorkerLabels(t.Context(), connect.NewRequest(&syncrv1.SetWorkerLabelsRequest{WorkerId: "nope"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = admin.SetWorkerLabels(t.Context(), connect.NewRequest(&syncrv1.SetWorkerLabelsRequest{WorkerId: domain.NewWorkerID().String()}))
	wantCode(t, err, connect.CodeNotFound)
	if _, err := admin.RemoveWorker(t.Context(), connect.NewRequest(&syncrv1.RemoveWorkerRequest{WorkerId: other.String()})); err != nil {
		t.Fatal(err)
	}
	_, err = admin.SetWorkerLabels(t.Context(), connect.NewRequest(&syncrv1.SetWorkerLabelsRequest{WorkerId: other.String(), Labels: []string{"x"}}))
	wantCode(t, err, connect.CodeNotFound)

	// No labels clears them.
	resp, err = admin.SetWorkerLabels(t.Context(), connect.NewRequest(&syncrv1.SetWorkerLabelsRequest{WorkerId: id}))
	if err != nil || len(resp.Msg.GetWorker().GetLabels()) != 0 {
		t.Errorf("clear = %v, %v", resp, err)
	}
}

func TestWorkerNameTaken(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.workersClient()
	second := e.addWorker()
	_, err := c.RenameWorker(t.Context(), connect.NewRequest(&syncrv1.RenameWorkerRequest{WorkerId: second.String(), Name: "WORKER"}))
	wantCode(t, err, connect.CodeAlreadyExists)
	if err == nil || !strings.Contains(err.Error(), "name") {
		t.Errorf("message does not explain: %v", err)
	}
	_, err = c.RegisterPassiveWorker(t.Context(), connect.NewRequest(&syncrv1.RegisterPassiveWorkerRequest{
		Name: "worker", Url: "https://worker.example:7443", CertFingerprint: testFP, PairingSecret: pairingSecret,
	}))
	wantCode(t, err, connect.CodeAlreadyExists)
}

// TestGetWorkerUpgradeCommands: the commands follow the worker's reported
// operating system and the controller's version (BuildInfo when the
// install configuration names none); an unknown OS has none, and an
// unknown or removed worker is NotFound.
func TestGetWorkerUpgradeCommands(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(d *Deps) {
		d.BuildInfo = version.Info{Version: "v2.0.0"}
		d.Install = InstallConfig{DownloadBaseURL: "https://mirror.example.com/syncr"}
	})
	ctx := t.Context()
	c := e.workersClient()
	get := func(id domain.WorkerID) ([]*syncrv1.InstallCommand, error) {
		resp, err := c.GetWorkerUpgradeCommands(ctx, connect.NewRequest(&syncrv1.GetWorkerUpgradeCommandsRequest{WorkerId: id.String()}))
		if err != nil {
			return nil, err
		}
		return resp.Msg.GetCommands(), nil
	}

	// Never connected: the OS is unknown.
	if cmds, err := get(e.worker); err != nil || len(cmds) != 0 {
		t.Errorf("unknown OS: %v, %v; want none", cmds, err)
	}
	for _, tt := range []struct {
		os, arch string
		platform syncrv1.InstallPlatform
		want     int
	}{
		{"linux", "arm64", syncrv1.InstallPlatform_INSTALL_PLATFORM_LINUX, 2},
		{"windows", "amd64", syncrv1.InstallPlatform_INSTALL_PLATFORM_WINDOWS, 2},
		{"darwin", "arm64", 0, 0},
	} {
		if err := e.store.SetWorkerInfo(ctx, e.worker, "", workers.Info{Version: "v1.9.0", OS: tt.os, Arch: tt.arch, ReportedAt: e.clock.Now()}); err != nil {
			t.Fatal(err)
		}
		cmds, err := get(e.worker)
		if err != nil || len(cmds) != tt.want {
			t.Fatalf("%s: %v, %v; want %d commands", tt.os, cmds, err, tt.want)
		}
		for _, cmd := range cmds {
			if cmd.GetPlatform() != tt.platform || !strings.Contains(cmd.GetCommand(), " upgrade") ||
				!strings.Contains(cmd.GetCommand(), " --version 'v2.0.0' --from 'https://mirror.example.com/syncr'") {
				t.Errorf("%s: %v", tt.os, cmd)
			}
		}
	}

	_, err := get(domain.NewWorkerID())
	wantCode(t, err, connect.CodeNotFound)
	_, err = c.GetWorkerUpgradeCommands(ctx, connect.NewRequest(&syncrv1.GetWorkerUpgradeCommandsRequest{WorkerId: "nope"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	if _, err := c.RemoveWorker(ctx, connect.NewRequest(&syncrv1.RemoveWorkerRequest{WorkerId: e.worker.String()})); err != nil {
		t.Fatal(err)
	}
	_, err = get(e.worker)
	wantCode(t, err, connect.CodeNotFound)
	_, err = e.workersFor(testUser).GetWorkerUpgradeCommands(ctx, connect.NewRequest(&syncrv1.GetWorkerUpgradeCommandsRequest{WorkerId: e.worker.String()}))
	wantCode(t, err, connect.CodePermissionDenied)

	bad := newEnv(t, func(d *Deps) { d.Install = InstallConfig{DownloadBaseURL: "ftp://mirror"} })
	if err := bad.store.SetWorkerInfo(ctx, bad.worker, "", workers.Info{Version: "v2", OS: "linux", ReportedAt: bad.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	_, err = bad.workersClient().GetWorkerUpgradeCommands(ctx, connect.NewRequest(&syncrv1.GetWorkerUpgradeCommandsRequest{WorkerId: bad.worker.String()}))
	wantCode(t, err, connect.CodeInternal)
}

// TestWorkerOutdatedThroughTheAPI: GetWorker and ListWorkers report
// Worker.outdated against the controller's version.
func TestWorkerOutdatedThroughTheAPI(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(d *Deps) { d.Install.Version = "v2.1.0" })
	ctx := t.Context()
	get := func() *syncrv1.Worker {
		resp, err := e.workersClient().GetWorker(ctx, connect.NewRequest(&syncrv1.GetWorkerRequest{WorkerId: e.worker.String()}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetWorker()
	}
	if get().GetOutdated() {
		t.Error("a worker that never connected is outdated")
	}
	for v, want := range map[string]bool{"v2.0.0": true, "v2.1.0": false} {
		if err := e.store.SetWorkerInfo(ctx, e.worker, "", workers.Info{Version: v, OS: "linux", Features: link.Features(), ReportedAt: e.clock.Now()}); err != nil {
			t.Fatal(err)
		}
		if got := get().GetOutdated(); got != want {
			t.Errorf("worker %s: outdated = %v, want %v", v, got, want)
		}
		list, err := e.workersClient().ListWorkers(ctx, connect.NewRequest(&syncrv1.ListWorkersRequest{}))
		if err != nil || list.Msg.GetWorkers()[0].GetOutdated() != want {
			t.Errorf("worker %s: listed %v (%v)", v, list, err)
		}
	}
}

func TestDashboardWorkersOutdated(t *testing.T) {
	t.Parallel()
	d := newDashEnv(t)
	d.h.version = "v2.1.0"
	if err := d.store.SetWorkerInfo(t.Context(), d.worker, "", workers.Info{Version: "v2.0.0", OS: "linux", Features: link.Features(), ReportedAt: d.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	ws := d.get(t, testUser, &syncrv1.GetDashboardRequest{}).GetWorkers().GetWorkers()
	if len(ws) != 1 || !ws[0].GetOutdated() {
		t.Errorf("dashboard workers = %v, want the worker outdated", ws)
	}
}
