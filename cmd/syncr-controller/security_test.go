package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/adapters/api"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/builtin"
	controllerconfig "github.com/Zeptiny/Syncr/internal/controller/config"
	"github.com/Zeptiny/Syncr/internal/devseed"
)

// TestDownloadBaseURLDefaultsAgree: the configuration's default download
// base URL is the API's, and the configuration leaves it unset.
func TestDownloadBaseURLDefaultsAgree(t *testing.T) {
	t.Parallel()
	if controllerconfig.DefaultDownloadBaseURL != api.DefaultDownloadBaseURL {
		t.Errorf("config default %q, api default %q", controllerconfig.DefaultDownloadBaseURL, api.DefaultDownloadBaseURL)
	}
	// Unset by default, so a release build's commands download its own
	// release (api.InstallConfig treats any value as a mirror).
	ac, err := newAPIConfig(controllerconfig.Default())
	if err != nil || ac.install.DownloadBaseURL != "" {
		t.Errorf("default install config = %+v (%v), want no download base URL", ac.install, err)
	}
}

// logLine is the part of a JSON log line the tests read.
type logLine struct {
	Level     string `json:"level"`
	Msg       string `json:"msg"`
	SetupCode string `json:"setup_code"`
}

// logLines parses the JSON log.
func logLines(t *testing.T, logs string) []logLine {
	t.Helper()
	var out []logLine
	sc := bufio.NewScanner(strings.NewReader(logs))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var l logLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("log line %q: %v", sc.Text(), err)
		}
		out = append(out, l)
	}
	return out
}

// TestRunFirstRunSetupCode: a new controller logs a random setup code at
// WARN, which completes the setup (once); the cookie warning comes with
// plain HTTP and no public URL; a restart with a user logs no code.
func TestRunFirstRunSetupCode(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	addr := startController(t, devEnv(nil), &logs)
	base := "http://" + addr.String()
	var code string
	for _, l := range logLines(t, logs.String()) {
		if l.Msg == setupCodeMessage {
			if l.Level != "WARN" {
				t.Errorf("setup code logged at %s", l.Level)
			}
			code = l.SetupCode
		}
	}
	if len(code) != 23 || strings.Count(code, "-") != 3 {
		t.Fatalf("setup code %q not logged as XXXXX-XXXXX-XXXXX-XXXXX:\n%s", code, logs.String())
	}
	if !strings.Contains(logs.String(), "session cookies are not marked Secure") {
		t.Errorf("no cookie warning:\n%s", logs.String())
	}

	// A wrong code is refused (without logging it); the logged one works,
	// once.
	setup := syncrv1connect.NewSetupServiceClient(http.DefaultClient, base)
	req := connect.NewRequest(&syncrv1.CompleteSetupRequest{SetupCode: "WRONG-WRONG-WRONG-WRONG", Username: "admin", Password: "a long admin password"})
	req.Header().Set(api.CSRFHeader, "1")
	if _, err := setup.CompleteSetup(t.Context(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("CompleteSetup(wrong code) = %v", err)
	}
	token := bootstrap(t, http.DefaultTransport, base, code)
	if token == "" {
		t.Fatal("no token")
	}
	again := connect.NewRequest(&syncrv1.CompleteSetupRequest{SetupCode: code, Username: "other", Password: "a long admin password"})
	again.Header().Set(api.CSRFHeader, "1")
	if _, err := setup.CompleteSetup(t.Context(), again); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("second CompleteSetup = %v, want failed precondition", err)
	}
}

// TestRunRemovedAdminToken: a configuration that still sets the admin
// token of Milestone 2 starts with a warning; the token is no credential.
func TestRunRemovedAdminToken(t *testing.T) {
	t.Parallel()
	const old = "old-admin-token-0123456789-abcdefghij"
	var logs syncBuffer
	addr := startController(t, devEnv(map[string]string{"SYNCR_ADMIN_TOKEN": old, "SYNCR_SETUP_CODE": testSetupCode}), &logs)
	found := false
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `"level":"WARN"`) && strings.Contains(line, `"env":"SYNCR_ADMIN_TOKEN"`) && strings.Contains(line, "API token") {
			found = true
		}
	}
	if !found {
		t.Errorf("no removal warning:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), old) || strings.Contains(logs.String(), testSetupCode) {
		t.Errorf("the log contains the old token or the preset setup code:\n%s", logs.String())
	}
	admin := syncrv1connect.NewWorkerAdminServiceClient(bearerClient(http.DefaultTransport, old), "http://"+addr.String())
	if _, err := admin.ListWorkers(t.Context(), connect.NewRequest(&syncrv1.ListWorkersRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("ListWorkers with the old admin token = %v", err)
	}
}

// TestRunRefusesUnusableSettings: settings the API cannot use stop the
// start with an error naming them.
func TestRunRefusesUnusableSettings(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		env  map[string]string
		want string
	}{
		"plain http public url": {map[string]string{"SYNCR_PUBLIC_URL": "http://nas.lan:8080"}, "http.public_url"},
		"relative public url":   {map[string]string{"SYNCR_PUBLIC_URL": "nas.lan"}, "http.public_url"},
		"short setup code":      {map[string]string{"SYNCR_SETUP_CODE": "too-short"}, "auth.setup_code"},
		"seed with postgres": {map[string]string{
			"SYNCR_DEV_SEED": "true", "SYNCR_DEV_MEMORY_STORE": "false", "SYNCR_DATABASE_URL": "postgres://db/syncr",
		}, "dev.seed needs dev.memory_store"},
		"bad cookie mode": {map[string]string{"SYNCR_AUTH_COOKIE_SECURE": "sometimes"}, "auth.cookie_secure"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := run(t.Context(), nil, io.Discard, io.Discard, envOf(devEnv(tt.env)), nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("run() = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

// seeded is a client of a controller started with dev.seed.
type seeded struct {
	t    *testing.T
	base string
	logs *syncBuffer
}

func startSeeded(t *testing.T) *seeded {
	t.Helper()
	return startSeededWith(t, nil)
}

// startSeededWith starts a seeded controller with further settings.
func startSeededWith(t *testing.T, extra map[string]string) *seeded {
	t.Helper()
	logs := &syncBuffer{}
	env := devEnv(map[string]string{"SYNCR_DEV_SEED": "true", "SYNCR_LOG_LEVEL": "debug"})
	maps.Copy(env, extra)
	addr := startController(t, env, logs)
	return &seeded{t: t, base: "http://" + addr.String(), logs: logs}
}

// login logs in as the web UI does and returns the session cookie.
func (s *seeded) login(username, password string) *http.Cookie {
	s.t.Helper()
	auth := syncrv1connect.NewAuthServiceClient(http.DefaultClient, s.base)
	req := connect.NewRequest(&syncrv1.LoginRequest{Username: username, Password: password})
	req.Header().Set(api.CSRFHeader, "1")
	resp, err := auth.Login(s.t.Context(), req)
	if err != nil {
		s.t.Fatalf("Login(%s): %v", username, err)
	}
	return sessionCookie(s.t, resp.Header())
}

// TestRunSeeded: dev.seed fills the store (no setup), its demo logins work,
// and the running job moves (WatchJob streams its progress).
func TestRunSeeded(t *testing.T) {
	t.Parallel()
	s := startSeeded(t)
	ctx := t.Context()
	if strings.Contains(s.logs.String(), setupCodeMessage) {
		t.Errorf("a seeded controller logs a setup code:\n%s", s.logs.String())
	}
	status, err := syncrv1connect.NewSetupServiceClient(http.DefaultClient, s.base).GetSetupStatus(ctx, connect.NewRequest(&syncrv1.GetSetupStatusRequest{}))
	if err != nil || status.Msg.GetSetupRequired() {
		t.Fatalf("GetSetupStatus = (%v, %v)", status, err)
	}
	admin := cookieClient(http.DefaultTransport, s.login(devseed.AdminUsername, devseed.AdminPassword), true)
	jobsClient := syncrv1connect.NewJobServiceClient(admin, s.base)
	list, err := jobsClient.ListJobs(ctx, connect.NewRequest(&syncrv1.ListJobsRequest{AllUsers: true, PageSize: 100}))
	if err != nil {
		t.Fatal(err)
	}
	var running *syncrv1.Job
	// The transfer is the job the seed keeps moving (a backup and a check
	// are shown running too, without progress).
	for _, j := range list.Msg.GetJobs() {
		if j.GetStatus() == syncrv1.JobStatus_JOB_STATUS_RUNNING && j.GetKind() == "rclone_transfer" {
			running = j
		}
	}
	if n := len(list.Msg.GetJobs()); n < 20 || running == nil {
		t.Fatalf("%d jobs, running %v", n, running)
	}
	if refs := running.GetRemotes(); len(refs) != 2 {
		t.Errorf("running job's remotes = %v", refs)
	}
	samples, err := jobsClient.ListJobSamples(ctx, connect.NewRequest(&syncrv1.ListJobSamplesRequest{JobId: running.GetId()}))
	if err != nil || len(samples.Msg.GetSamples()) < 100 {
		t.Fatalf("ListJobSamples = %d samples (%v)", len(samples.Msg.GetSamples()), err)
	}
	workers, err := syncrv1connect.NewWorkerAdminServiceClient(admin, s.base).ListWorkers(ctx, connect.NewRequest(&syncrv1.ListWorkersRequest{}))
	if err != nil || len(workers.Msg.GetWorkers()) != 5 {
		t.Fatalf("ListWorkers = %v (%v)", workers, err)
	}
	checkSeededMilestone4(t, s, admin)

	// Live progress arrives about every second.
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stream, err := jobsClient.WatchJob(wctx, connect.NewRequest(&syncrv1.WatchJobRequest{JobId: running.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	var progress []int64
	for len(progress) < 2 && stream.Receive() {
		if p := stream.Msg().GetProgress(); p != nil {
			progress = append(progress, p.GetStats().GetBytes())
		}
	}
	if len(progress) < 2 || progress[1] <= progress[0] {
		t.Fatalf("progress %v (stream error %v)", progress, stream.Err())
	}
}

// codeOrOK returns the call's Connect code, or "ok".
func codeOrOK(err error) string {
	if err == nil {
		return "ok"
	}
	return connect.CodeOf(err).String()
}

// TestSecurityEndToEnd checks the access rules on the real wired
// controller (seeded): owner scoping, admin-only procedures, the CSRF
// defenses and that secrets never come back or reach the log, also when the
// dispatcher resolves them into a worker's spec.
func TestSecurityEndToEnd(t *testing.T) {
	t.Parallel()
	s := startSeeded(t)
	ctx := t.Context()
	adminCookie := s.login(devseed.AdminUsername, devseed.AdminPassword)
	aliceCookie := s.login(devseed.AliceUsername, devseed.AlicePassword)
	admin := cookieClient(http.DefaultTransport, adminCookie, true)
	alice := cookieClient(http.DefaultTransport, aliceCookie, true)

	// The admin's objects, found as the admin.
	adminRemotes, err := syncrv1connect.NewRemoteServiceClient(admin, s.base).ListRemotes(ctx, connect.NewRequest(&syncrv1.ListRemotesRequest{}))
	if err != nil || len(adminRemotes.Msg.GetRemotes()) < 6 {
		t.Fatalf("admin's ListRemotes = %v (%v)", adminRemotes, err)
	}
	adminRemote := adminRemotes.Msg.GetRemotes()[0]
	adminJobs, err := syncrv1connect.NewJobServiceClient(admin, s.base).ListJobs(ctx, connect.NewRequest(&syncrv1.ListJobsRequest{}))
	if err != nil || len(adminJobs.Msg.GetJobs()) == 0 {
		t.Fatalf("admin's ListJobs = %v (%v)", adminJobs, err)
	}
	var adminJob, adminRunning *syncrv1.Job
	for _, j := range adminJobs.Msg.GetJobs() {
		if j.GetOwnerId() == "" {
			t.Fatalf("job without owner: %v", j)
		}
		if j.GetStatus() == syncrv1.JobStatus_JOB_STATUS_RUNNING {
			adminRunning = j
		} else if adminJob == nil {
			adminJob = j
		}
	}
	if adminRunning == nil || adminJob == nil {
		t.Fatal("the seed has no running and finished admin jobs")
	}

	t.Run("another user's objects are not found", func(t *testing.T) {
		t.Parallel()
		rc := syncrv1connect.NewRemoteServiceClient(alice, s.base)
		jc := syncrv1connect.NewJobServiceClient(alice, s.base)
		calls := map[string]func() error{
			"GetRemote": func() error {
				_, err := rc.GetRemote(ctx, connect.NewRequest(&syncrv1.GetRemoteRequest{RemoteId: adminRemote.GetId()}))
				return err
			},
			"UpdateRemote": func() error {
				_, err := rc.UpdateRemote(ctx, connect.NewRequest(&syncrv1.UpdateRemoteRequest{RemoteId: adminRemote.GetId(), Version: adminRemote.GetVersion(),
					Input: &syncrv1.RemoteInput{Name: "mine now", Options: adminRemote.GetOptions()}}))
				return err
			},
			"DeleteRemote": func() error {
				_, err := rc.DeleteRemote(ctx, connect.NewRequest(&syncrv1.DeleteRemoteRequest{RemoteId: adminRemote.GetId()}))
				return err
			},
			"GetJob": func() error {
				_, err := jc.GetJob(ctx, connect.NewRequest(&syncrv1.GetJobRequest{JobId: adminJob.GetId()}))
				return err
			},
			"CancelJob": func() error {
				_, err := jc.CancelJob(ctx, connect.NewRequest(&syncrv1.CancelJobRequest{JobId: adminRunning.GetId()}))
				return err
			},
			"ListJobSamples": func() error {
				_, err := jc.ListJobSamples(ctx, connect.NewRequest(&syncrv1.ListJobSamplesRequest{JobId: adminRunning.GetId()}))
				return err
			},
			"WatchJob": func() error {
				stream, err := jc.WatchJob(ctx, connect.NewRequest(&syncrv1.WatchJobRequest{JobId: adminRunning.GetId()}))
				if err != nil {
					return err
				}
				defer func() { _ = stream.Close() }()
				for stream.Receive() {
					return errors.New("received a message of another user's job")
				}
				return stream.Err()
			},
		}
		for name, call := range calls {
			if err := call(); connect.CodeOf(err) != connect.CodeNotFound {
				t.Errorf("alice %s on the admin's object: %s, want not_found", name, codeOrOK(err))
			}
		}
		// Running a job on the admin's remote is refused like a missing one.
		run, err := jc.RunJob(ctx, connect.NewRequest(&syncrv1.RunJobRequest{WorkerId: adminJob.GetWorkerId(), Definition: copyDefinition(adminRemote.GetId(), "/srv/x")}))
		if err == nil {
			t.Errorf("alice ran a job on the admin's remote: %v", run.Msg.GetJob())
		}
		// Lists contain only her own, even when she asks for all users.
		remotes, err := rc.ListRemotes(ctx, connect.NewRequest(&syncrv1.ListRemotesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range remotes.Msg.GetRemotes() {
			if r.GetId() == adminRemote.GetId() || !strings.HasPrefix(r.GetName(), "Alice") {
				t.Errorf("alice lists remote %q", r.GetName())
			}
		}
		jobs, err := jc.ListJobs(ctx, connect.NewRequest(&syncrv1.ListJobsRequest{AllUsers: true, PageSize: 100}))
		if err != nil || len(jobs.Msg.GetJobs()) == 0 {
			t.Fatalf("alice's ListJobs = %v (%v)", jobs, err)
		}
		for _, j := range jobs.Msg.GetJobs() {
			if j.GetOwnerId() == adminJob.GetOwnerId() {
				t.Errorf("alice lists the admin's job %s", j.GetId())
			}
		}
		// The admin sees and may cancel any job, but never alice's remotes.
		aliceJob := jobs.Msg.GetJobs()[0]
		if _, err := syncrv1connect.NewJobServiceClient(admin, s.base).GetJob(ctx, connect.NewRequest(&syncrv1.GetJobRequest{JobId: aliceJob.GetId()})); err != nil {
			t.Errorf("admin GetJob(alice's) = %v", err)
		}
		if len(aliceJob.GetRemotes()) == 0 {
			t.Fatalf("alice's job has no remotes: %v", aliceJob)
		}
		_, err = syncrv1connect.NewRemoteServiceClient(admin, s.base).GetRemote(ctx, connect.NewRequest(&syncrv1.GetRemoteRequest{RemoteId: aliceJob.GetRemotes()[0].GetId()}))
		if connect.CodeOf(err) != connect.CodeNotFound {
			t.Errorf("admin GetRemote(alice's) = %s, want not_found", codeOrOK(err))
		}
	})

	t.Run("admin procedures need an admin", func(t *testing.T) {
		t.Parallel()
		uc := syncrv1connect.NewUserServiceClient(alice, s.base)
		wc := syncrv1connect.NewWorkerAdminServiceClient(alice, s.base)
		id := adminJob.GetWorkerId()
		calls := map[string]func() error{
			"ListUsers": func() error { _, err := uc.ListUsers(ctx, connect.NewRequest(&syncrv1.ListUsersRequest{})); return err },
			"CreateUser": func() error {
				_, err := uc.CreateUser(ctx, connect.NewRequest(&syncrv1.CreateUserRequest{Username: "mallory", Password: "a long enough password", Role: syncrv1.UserRole_USER_ROLE_ADMIN}))
				return err
			},
			"UpdateUser": func() error {
				_, err := uc.UpdateUser(ctx, connect.NewRequest(&syncrv1.UpdateUserRequest{UserId: adminJob.GetOwnerId(), Version: 1, Role: syncrv1.UserRole_USER_ROLE_USER}))
				return err
			},
			"ResetPassword": func() error {
				_, err := uc.ResetPassword(ctx, connect.NewRequest(&syncrv1.ResetPasswordRequest{UserId: adminJob.GetOwnerId(), NewPassword: "a long enough password"}))
				return err
			},
			"DeleteUser": func() error {
				_, err := uc.DeleteUser(ctx, connect.NewRequest(&syncrv1.DeleteUserRequest{UserId: adminJob.GetOwnerId()}))
				return err
			},
			"CreateEnrollmentToken": func() error {
				_, err := wc.CreateEnrollmentToken(ctx, connect.NewRequest(&syncrv1.CreateEnrollmentTokenRequest{}))
				return err
			},
			"GetPassiveInstallCommands": func() error {
				_, err := wc.GetPassiveInstallCommands(ctx, connect.NewRequest(&syncrv1.GetPassiveInstallCommandsRequest{}))
				return err
			},
			"RegisterPassiveWorker": func() error {
				_, err := wc.RegisterPassiveWorker(ctx, connect.NewRequest(&syncrv1.RegisterPassiveWorkerRequest{
					Name: "p", Url: "https://p.example:7443", CertFingerprint: strings.Repeat("ab", 32), PairingSecret: "pairing-secret-0123456789-abcdefghij",
				}))
				return err
			},
			"RenameWorker": func() error {
				_, err := wc.RenameWorker(ctx, connect.NewRequest(&syncrv1.RenameWorkerRequest{WorkerId: id, Name: "mine"}))
				return err
			},
			"RemoveWorker": func() error {
				_, err := wc.RemoveWorker(ctx, connect.NewRequest(&syncrv1.RemoveWorkerRequest{WorkerId: id}))
				return err
			},
		}
		for name, call := range calls {
			if err := call(); connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Errorf("alice %s: %s, want permission_denied", name, codeOrOK(err))
			}
		}
		// Users may list and inspect the shared workers.
		if _, err := wc.GetWorker(ctx, connect.NewRequest(&syncrv1.GetWorkerRequest{WorkerId: id})); err != nil {
			t.Errorf("alice GetWorker = %v", err)
		}
	})

	t.Run("an API token cannot mint tokens", func(t *testing.T) {
		t.Parallel()
		tok, err := syncrv1connect.NewAuthServiceClient(alice, s.base).CreateAPIToken(ctx, connect.NewRequest(&syncrv1.CreateAPITokenRequest{Name: "script"}))
		if err != nil {
			t.Fatal(err)
		}
		byToken := bearerClient(http.DefaultTransport, tok.Msg.GetSecret())
		if _, err := syncrv1connect.NewRemoteServiceClient(byToken, s.base).ListRemotes(ctx, connect.NewRequest(&syncrv1.ListRemotesRequest{})); err != nil {
			t.Errorf("ListRemotes by token = %v", err)
		}
		_, err = syncrv1connect.NewAuthServiceClient(byToken, s.base).CreateAPIToken(ctx, connect.NewRequest(&syncrv1.CreateAPITokenRequest{Name: "more"}))
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("CreateAPIToken by token = %s, want permission_denied", codeOrOK(err))
		}
	})

	t.Run("cookie calls need the CSRF header", func(t *testing.T) {
		t.Parallel()
		noCSRF := cookieClient(http.DefaultTransport, aliceCookie, false)
		_, err := syncrv1connect.NewRemoteServiceClient(noCSRF, s.base).ListRemotes(ctx, connect.NewRequest(&syncrv1.ListRemotesRequest{}))
		if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("cookie without %s = %s, want unauthenticated", api.CSRFHeader, codeOrOK(err))
		}
		// Login without it is refused too.
		req := connect.NewRequest(&syncrv1.LoginRequest{Username: devseed.AliceUsername, Password: devseed.AlicePassword})
		if _, err := syncrv1connect.NewAuthServiceClient(http.DefaultClient, s.base).Login(ctx, req); err == nil {
			t.Error("Login without the CSRF header succeeded")
		}
	})

	t.Run("cross-origin requests are rejected", func(t *testing.T) {
		t.Parallel()
		for name, set := range map[string]func(http.Header){
			"Sec-Fetch-Site":   func(h http.Header) { h.Set("Sec-Fetch-Site", "cross-site") },
			"Origin":           func(h http.Header) { h.Set("Origin", "https://evil.example") },
			"same-site origin": func(h http.Header) { h.Set("Sec-Fetch-Site", "same-site") },
		} {
			for _, path := range []string{"/syncr.v1.RemoteService/DeleteRemote", "/syncr.v1.AuthService/Login"} {
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+path, strings.NewReader(`{"remoteId":"`+adminRemote.GetId()+`"}`))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set(api.CSRFHeader, "1")
				req.AddCookie(adminCookie)
				set(req.Header)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusForbidden {
					t.Errorf("%s POST %s: status %d, want 403", name, path, resp.StatusCode)
				}
			}
		}
		if _, err := syncrv1connect.NewRemoteServiceClient(admin, s.base).GetRemote(ctx, connect.NewRequest(&syncrv1.GetRemoteRequest{RemoteId: adminRemote.GetId()})); err != nil {
			t.Errorf("the admin's remote is gone after the cross-origin attempts: %v", err)
		}
	})

	t.Run("secrets never come back", func(t *testing.T) {
		t.Parallel()
		checkSecretsStayHidden(t, s, admin, alice)
	})
}

// copyDefinition copies from a path of remoteID to a worker path.
func copyDefinition(remoteID, dst string) *syncrv1.JobDefinition {
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransferDefinition{
		Mode:        syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY,
		Source:      &syncrv1.JobLocation{RemoteId: remoteID, Path: "data"},
		Destination: &syncrv1.JobLocation{Path: dst},
	}}}
}

// checkSecretsStayHidden creates a remote of every built-in type with
// sentinel secrets as alice, reads, lists and updates them, and runs a job
// with each on a worker that polls for it over the real link: the worker's
// spec carries the secrets, and no API response nor log line does.
func checkSecretsStayHidden(t *testing.T, s *seeded, admin, alice *http.Client) {
	ctx := t.Context()
	rc := syncrv1connect.NewRemoteServiceClient(alice, s.base)
	var responses []string
	record := func(m proto.Message) {
		b, err := protojson.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		responses = append(responses, string(b))
	}
	sentinel := func(typ, field string, n int) string { return fmt.Sprintf("sentinel-%s-%s-%d-secret", typ, field, n) }
	var sentinels []string

	reg, err := backend.NewRegistry(builtin.Types()...)
	if err != nil {
		t.Fatal(err)
	}
	var members []string
	for i := range 2 {
		m, err := rc.CreateRemote(ctx, connect.NewRequest(&syncrv1.CreateRemoteRequest{Input: &syncrv1.RemoteInput{
			Name: fmt.Sprintf("Alice member %d", i), BackendType: "local", Options: map[string]string{"path": fmt.Sprintf("/srv/m%d", i)},
		}}))
		if err != nil {
			t.Fatal(err)
		}
		members = append(members, m.Msg.GetRemote().GetId())
	}
	created := map[string]*syncrv1.Remote{}
	for _, typ := range reg.Types() {
		ex, ok := typ.(backend.Exampler)
		if !ok || len(ex.Examples()) == 0 {
			t.Fatalf("type %s has no example configuration", typ.ID())
		}
		cfg := ex.Examples()[0]
		in := &syncrv1.RemoteInput{Name: "Alice " + typ.ID(), BackendType: typ.ID(), Options: maps.Clone(cfg.Options), Secrets: map[string]string{}}
		for _, f := range typ.Fields() {
			if f.Secret {
				in.Secrets[f.Name] = sentinel(typ.ID(), f.Name, 1)
				sentinels = append(sentinels, in.Secrets[f.Name])
			}
		}
		if typ.Capabilities().Union {
			in.Members = []*syncrv1.UnionMember{{RemoteId: members[0]}, {RemoteId: members[1]}}
		}
		resp, err := rc.CreateRemote(ctx, connect.NewRequest(&syncrv1.CreateRemoteRequest{Input: in}))
		if err != nil {
			t.Fatalf("CreateRemote(%s): %v", typ.ID(), err)
		}
		record(resp.Msg)
		r := resp.Msg.GetRemote()
		if want := slices.Sorted(maps.Keys(in.GetSecrets())); !slices.Equal(r.GetSecretsSet(), want) {
			t.Errorf("%s: secrets_set %v, want %v", typ.ID(), r.GetSecretsSet(), want)
		}
		// Replace one secret, keep the others.
		up, ok := proto.Clone(in).(*syncrv1.RemoteInput)
		if !ok {
			t.Fatal("clone is not a RemoteInput")
		}
		up.Secrets = map[string]string{}
		for k := range in.GetSecrets() {
			up.Secrets[k] = sentinel(typ.ID(), k, 2)
			sentinels = append(sentinels, up.Secrets[k])
			break
		}
		upd, err := rc.UpdateRemote(ctx, connect.NewRequest(&syncrv1.UpdateRemoteRequest{RemoteId: r.GetId(), Version: r.GetVersion(), Input: up}))
		if err != nil {
			t.Fatalf("UpdateRemote(%s): %v", typ.ID(), err)
		}
		record(upd.Msg)
		got, err := rc.GetRemote(ctx, connect.NewRequest(&syncrv1.GetRemoteRequest{RemoteId: r.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		record(got.Msg)
		created[typ.ID()] = got.Msg.GetRemote()
	}
	list, err := rc.ListRemotes(ctx, connect.NewRequest(&syncrv1.ListRemotesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	record(list.Msg)

	// A worker of its own, enrolled over the real link.
	tok, err := syncrv1connect.NewWorkerAdminServiceClient(admin, s.base).CreateEnrollmentToken(ctx, connect.NewRequest(&syncrv1.CreateEnrollmentTokenRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := syncrv1connect.NewEnrollmentServiceClient(http.DefaultClient, s.base).Enroll(ctx, connect.NewRequest(&syncrv1.EnrollRequest{
		EnrollmentToken: tok.Msg.GetToken(), Name: "secrets-test", Os: "linux", Arch: "amd64",
	}))
	if err != nil {
		t.Fatal(err)
	}
	workerID := enrolled.Msg.GetWorkerId()
	jc := syncrv1connect.NewJobServiceClient(alice, s.base)
	var jobIDs []string
	for _, id := range slices.Sorted(maps.Keys(created)) {
		r := created[id]
		if id == "local" {
			continue // no secrets, and its root need not exist on the worker
		}
		run, err := jc.RunJob(ctx, connect.NewRequest(&syncrv1.RunJobRequest{WorkerId: workerID, Definition: copyDefinition(r.GetId(), "/srv/restore/"+id)}))
		if err != nil {
			t.Fatalf("RunJob(%s): %v", id, err)
		}
		record(run.Msg)
		jobIDs = append(jobIDs, run.Msg.GetJob().GetId())
	}

	link := syncrv1connect.NewControllerLinkServiceClient(&http.Client{Transport: headers{next: http.DefaultTransport, set: func(h http.Header) {
		h.Set("Authorization", "Bearer "+workerID+"."+enrolled.Msg.GetWorkerSecret())
	}}}, s.base)
	// The worker's first Report carries its Hello; no StartJob comes before it.
	hello := &syncrv1.Event{Seq: 1, Payload: &syncrv1.Event_Hello{Hello: &syncrv1.Hello{Os: "linux", Arch: "amd64", JobKinds: []string{"rclone_transfer"}}}}
	if _, err := link.Report(ctx, connect.NewRequest(&syncrv1.ReportRequest{Epoch: "secrets-test-epoch", Events: []*syncrv1.Event{hello}})); err != nil {
		t.Fatal(err)
	}
	poll, err := link.Poll(ctx, connect.NewRequest(&syncrv1.PollRequest{Epoch: "secrets-test-epoch", Wait: durationpb.New(0)}))
	if err != nil {
		t.Fatal(err)
	}
	specs, err := protojson.Marshal(poll.Msg)
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	for _, c := range poll.Msg.GetCommands() {
		if c.GetStartJob() != nil {
			starts++
		}
	}
	if starts != len(jobIDs) {
		t.Fatalf("the worker got %d StartJob commands, want %d:\n%s", starts, len(jobIDs), specs)
	}
	// The worker, and only the worker, gets the current secrets.
	for _, typ := range []string{"s3", "sftp", "b2", "webdav"} {
		if !strings.Contains(string(specs), "sentinel-"+typ+"-") {
			t.Errorf("the %s secrets are not in the worker's spec", typ)
		}
	}
	for _, j := range jobIDs {
		got, err := jc.GetJob(ctx, connect.NewRequest(&syncrv1.GetJobRequest{JobId: j}))
		if err != nil {
			t.Fatal(err)
		}
		record(got.Msg)
	}

	all := strings.Join(responses, "\n")
	logs := s.logs.String()
	for _, v := range sentinels {
		if strings.Contains(all, v) {
			t.Errorf("an API response contains secret %q", v)
		}
		if strings.Contains(logs, v) {
			t.Errorf("the log contains secret %q", v)
		}
	}
	if !strings.Contains(logs, `"msg":"remote created"`) {
		t.Errorf("the log is not the controller's debug log:\n%s", logs)
	}
}

// checkSeededMilestone4 checks the seeded schedules, contacts, deliveries,
// worker labels and dashboard through the API, as the web UI's screenshot
// script expects them (by name), and that a test send under dev.seed
// succeeds without sending (notify.DryRun).
func checkSeededMilestone4(t *testing.T, s *seeded, admin *http.Client) {
	t.Helper()
	ctx := t.Context()
	withLabel, err := syncrv1connect.NewWorkerAdminServiceClient(admin, s.base).ListWorkers(ctx, connect.NewRequest(&syncrv1.ListWorkersRequest{Label: "nas"}))
	if err != nil || len(withLabel.Msg.GetWorkers()) != 1 || withLabel.Msg.GetWorkers()[0].GetName() != "nas-01" {
		t.Errorf("workers withLabel nas = %v (%v)", withLabel, err)
	}
	sc := syncrv1connect.NewScheduleServiceClient(admin, s.base)
	list, err := sc.ListSchedules(ctx, connect.NewRequest(&syncrv1.ListSchedulesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*syncrv1.Schedule{}
	for _, x := range list.Msg.GetSchedules() {
		byName[x.GetName()] = x
	}
	for _, n := range []string{"Nightly photos → B2", "Office docs hourly", "Weekly archive", "Offsite mirror", "Old NAS sync"} {
		if byName[n] == nil {
			t.Errorf("no seeded schedule %q in %v", n, slices.Collect(maps.Keys(byName)))
		}
	}
	if p := byName["Old NAS sync"]; p == nil || p.GetEnabled() || p.GetPausedReason() != "worker_removed" {
		t.Errorf("Old NAS sync = %v", p)
	}
	if n := byName["Nightly photos → B2"]; n == nil || n.GetTarget().GetWorkerLabel() != "nas" || n.GetTimezone() != "Europe/Berlin" {
		t.Errorf("Nightly photos = %v", n)
	}
	if o := byName["Offsite mirror"]; o != nil {
		jobs, err := syncrv1connect.NewJobServiceClient(admin, s.base).ListJobs(ctx, connect.NewRequest(&syncrv1.ListJobsRequest{ScheduleId: o.GetId()}))
		if err != nil || len(jobs.Msg.GetJobs()) == 0 || jobs.Msg.GetJobs()[0].GetSchedule().GetName() != "Offsite mirror" {
			t.Errorf("jobs of Offsite mirror = %d (%v)", len(jobs.Msg.GetJobs()), err)
		}
	}
	if o := byName["Office docs hourly"]; o != nil {
		runs, err := sc.ListScheduleRuns(ctx, connect.NewRequest(&syncrv1.ListScheduleRunsRequest{ScheduleId: o.GetId()}))
		if err != nil || len(runs.Msg.GetRuns()) == 0 || runs.Msg.GetRuns()[0].GetStatus() != syncrv1.ScheduleRunStatus_SCHEDULE_RUN_STATUS_PENDING ||
			runs.Msg.GetRuns()[0].GetCoalesced() == 0 {
			t.Errorf("Office docs hourly runs = %v (%v)", runs.Msg.GetRuns()[:min(1, len(runs.Msg.GetRuns()))], err)
		}
	}

	cc := syncrv1connect.NewContactServiceClient(admin, s.base)
	contacts, err := cc.ListContacts(ctx, connect.NewRequest(&syncrv1.ListContactsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]string{}
	var mail string
	for _, c := range contacts.Msg.GetContacts() {
		types[c.GetName()] = c.GetChannelType()
		if c.GetName() == "Admin mail" {
			mail = c.GetId()
		}
		if strings.Contains(c.GetSummary(), "syncr-demo") {
			t.Errorf("summary of %s = %q", c.GetName(), c.GetSummary())
		}
	}
	if !maps.Equal(types, map[string]string{"Ops Discord": "discord", "Admin mail": "email", "Home Assistant": "webhook"}) {
		t.Errorf("contacts = %v", types)
	}
	ds, err := cc.ListDeliveries(ctx, connect.NewRequest(&syncrv1.ListDeliveriesRequest{PageSize: 500}))
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[syncrv1.DeliveryStatus]int{}
	retrying, manual := 0, 0
	for _, d := range ds.Msg.GetDeliveries() {
		statuses[d.GetStatus()]++
		// Jobs without a schedule are titled by their description, never
		// by their kind.
		if strings.Contains(d.GetTitle(), "rclone_") || strings.Contains(d.GetTitle(), "restic_") {
			t.Errorf("delivery title %q names the job kind", d.GetTitle())
		}
		if d.GetJobId() != "" && d.GetScheduleId() == "" {
			manual++
			// A transfer names both ends; a restic job its repository.
			if !strings.Contains(d.GetTitle(), " → ") && !strings.Contains(d.GetTitle(), "Photos vault") {
				t.Errorf("manual job delivery title %q has no description", d.GetTitle())
			}
			// The title starts with the kind's label, as people read it.
			if strings.HasPrefix(d.GetTitle(), "Job ") {
				t.Errorf("manual job delivery title %q lacks the kind's label", d.GetTitle())
			}
		}
		if d.GetStatus() == syncrv1.DeliveryStatus_DELIVERY_STATUS_PENDING && d.GetAttempts() > 0 && d.GetLastError() != "" {
			retrying++
		}
	}
	if len(statuses) != 4 || retrying == 0 || manual == 0 {
		t.Errorf("delivery statuses %v, %d retrying, %d of manual jobs", statuses, retrying, manual)
	}
	res, err := cc.SendTestNotification(ctx, connect.NewRequest(&syncrv1.SendTestNotificationRequest{ContactId: mail}))
	if err != nil || !res.Msg.GetOk() {
		t.Errorf("SendTest(Admin mail) under dev.seed = %v (%v)", res, err)
	}

	dash, err := syncrv1connect.NewDashboardServiceClient(admin, s.base).GetDashboard(ctx, connect.NewRequest(&syncrv1.GetDashboardRequest{
		Range: syncrv1.DashboardRange_DASHBOARD_RANGE_WEEK,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if tot := dash.Msg.GetTotals(); tot.GetSucceeded() < 30 || tot.GetFailed() == 0 || len(dash.Msg.GetUpcoming()) == 0 ||
		len(dash.Msg.GetRecentFailures()) == 0 || len(dash.Msg.GetRecentMissed()) == 0 {
		t.Errorf("7-day dashboard: totals %v, %d upcoming, %d failures, %d missed", tot, len(dash.Msg.GetUpcoming()),
			len(dash.Msg.GetRecentFailures()), len(dash.Msg.GetRecentMissed()))
	}
}
