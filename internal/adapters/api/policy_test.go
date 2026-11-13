package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/notify"
	"github.com/Zeptiny/Syncr/internal/platform/logging"
)

// Stand-ins for the use cases that later tracks implement: enough to mount
// their services (the stub handlers never call them).
type (
	stubIdentity   struct{ IdentityService }
	stubUsers      struct{ UserAdminService }
	stubRemotes    struct{ RemoteService }
	stubSchedules  struct{ ScheduleService }
	stubRestic     struct{ ResticService }
	stubProbes     struct{ ProbeService }
	stubContacts   struct{ ContactService }
	stubDeliveries struct{ DeliveryService }
	// stubDashboardJobs adds the dashboard's job use cases to the real
	// JobService.
	stubDashboardJobs struct {
		JobService
		DashboardJobs
	}
)

// withEveryService wires a stand-in for every service, so NewHandler mounts
// them all.
func withEveryService(t *testing.T) func(*Deps) {
	t.Helper()
	reg, err := backend.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	channels, err := notify.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	return func(d *Deps) {
		d.Identity, d.Users, d.Remotes, d.Backends = stubIdentity{}, stubUsers{}, stubRemotes{}, reg
		d.Schedules, d.Contacts, d.Deliveries, d.Channels = stubSchedules{}, stubContacts{}, stubDeliveries{}, channels
		d.Restic, d.Probes = stubRestic{}, stubProbes{}
		if _, ok := d.Jobs.(DashboardJobs); !ok {
			d.Jobs = stubDashboardJobs{JobService: d.Jobs}
		}
	}
}

// linkServices are served by the worker link transports, not by NewHandler.
func linkServices() []string {
	return []string{syncrv1connect.ControllerLinkServiceName, syncrv1connect.WorkerLinkServiceName, syncrv1connect.EnrollmentServiceName}
}

// serviceDescriptor returns the descriptor of a service by full name.
func serviceDescriptor(t *testing.T, name string) protoreflect.ServiceDescriptor {
	t.Helper()
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(name))
	if err != nil {
		t.Fatalf("service %s: %v", name, err)
	}
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		t.Fatalf("%s is not a service", name)
	}
	return sd
}

func procedure(sd protoreflect.ServiceDescriptor, md protoreflect.MethodDescriptor) string {
	return "/" + string(sd.FullName()) + "/" + string(md.Name())
}

// TestPolicyCoversEveryProcedure walks every method of every API service
// and asserts it has an access level, and that the table names nothing
// else. A new service of the syncr.v1 package must be added to apiServices
// (or be a link service).
func TestPolicyCoversEveryProcedure(t *testing.T) {
	t.Parallel()
	policy := newPolicy()
	known := map[string]bool{}
	for _, name := range apiServices() {
		sd := serviceDescriptor(t, name)
		for i := range sd.Methods().Len() {
			p := procedure(sd, sd.Methods().Get(i))
			known[p] = true
			if _, ok := policy[p]; !ok {
				t.Errorf("procedure %s has no access policy", p)
			}
		}
	}
	for p := range policy {
		if !known[p] {
			t.Errorf("policy entry %s is not a procedure of an API service", p)
		}
	}

	// Every service of the package is either served by NewHandler or a link
	// service.
	protoregistry.GlobalFiles.RangeFilesByPackage("syncr.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			name := string(fd.Services().Get(i).FullName())
			if !slices.Contains(apiServices(), name) && !slices.Contains(linkServices(), name) {
				t.Errorf("service %s (%s) is neither an API service nor a link service", name, fd.Path())
			}
		}
		return true
	})
}

// TestPolicyLevels pins the access table of the design (docs: M3 design
// section 1, "Authorization").
func TestPolicyLevels(t *testing.T) {
	t.Parallel()
	policy := newPolicy()
	want := map[access][]string{
		public: {
			syncrv1connect.SystemServiceGetInfoProcedure,
			syncrv1connect.SetupServiceGetSetupStatusProcedure, syncrv1connect.SetupServiceCompleteSetupProcedure,
			syncrv1connect.AuthServiceLoginProcedure,
		},
		userSession: {
			syncrv1connect.AuthServiceChangePasswordProcedure, syncrv1connect.AuthServiceCreateAPITokenProcedure,
			syncrv1connect.AuthServiceDeleteAPITokenProcedure,
		},
		admin: {
			syncrv1connect.WorkerAdminServiceCreateEnrollmentTokenProcedure, syncrv1connect.WorkerAdminServiceGetEnrollmentTokenProcedure,
			syncrv1connect.WorkerAdminServiceGetPassiveInstallCommandsProcedure,
			syncrv1connect.WorkerAdminServiceRegisterPassiveWorkerProcedure, syncrv1connect.WorkerAdminServiceRenameWorkerProcedure,
			syncrv1connect.WorkerAdminServiceRemoveWorkerProcedure, syncrv1connect.WorkerAdminServiceSetWorkerLabelsProcedure,
			syncrv1connect.WorkerAdminServiceGetWorkerUpgradeCommandsProcedure,
		},
		adminSession: {
			syncrv1connect.UserServiceCreateUserProcedure, syncrv1connect.UserServiceResetPasswordProcedure,
		},
	}
	for level, procs := range want {
		for _, p := range procs {
			if policy[p] != level {
				t.Errorf("%s is %v, want %v", p, policy[p], level)
			}
		}
	}
	for p, level := range policy {
		svc := strings.Split(p, "/")[1]
		switch {
		case svc == syncrv1connect.UserServiceName && level != admin && level != adminSession:
			t.Errorf("%s is %v; UserService is admin only", p, level)
		case slices.Contains([]string{
			syncrv1connect.JobServiceName, syncrv1connect.RemoteServiceName, syncrv1connect.BackendServiceName,
			syncrv1connect.ScheduleServiceName, syncrv1connect.ChannelServiceName, syncrv1connect.ContactServiceName,
			syncrv1connect.DashboardServiceName, syncrv1connect.ResticServiceName, syncrv1connect.ProbeServiceName,
		}, svc) && level != user:
			t.Errorf("%s is %v, want user", p, level)
		}
	}
	for _, p := range []string{syncrv1connect.WorkerAdminServiceListWorkersProcedure, syncrv1connect.WorkerAdminServiceGetWorkerProcedure,
		syncrv1connect.AuthServiceLogoutProcedure, syncrv1connect.AuthServiceGetSessionProcedure, syncrv1connect.AuthServiceListAPITokensProcedure} {
		if policy[p] != user {
			t.Errorf("%s is %v, want user", p, policy[p])
		}
	}
}

// rawCall posts an empty request to procedure without credentials, as a
// unary or server-streaming Connect call, and returns the HTTP status and
// body.
func rawCall(t *testing.T, e *testEnv, procedure string, streaming bool, header http.Header) (int, string) {
	t.Helper()
	body, contentType := []byte("{}"), "application/json"
	if streaming {
		var env bytes.Buffer
		env.WriteByte(0)
		_ = binary.Write(&env, binary.BigEndian, uint32(2))
		env.WriteString("{}")
		body, contentType = env.Bytes(), "application/connect+json"
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.srv.URL+procedure, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// TestEveryServiceIsMountedBehindTheGuard calls every procedure of every API
// service without credentials on a fully wired handler: each one exists (no
// 404 from the mux), and each non-public one is refused as Unauthenticated
// before any handler code runs.
func TestEveryServiceIsMountedBehindTheGuard(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withEveryService(t))
	policy := newPolicy()
	for _, name := range apiServices() {
		sd := serviceDescriptor(t, name)
		for i := range sd.Methods().Len() {
			md := sd.Methods().Get(i)
			p := procedure(sd, md)
			status, body := rawCall(t, e, p, md.IsStreamingServer(), nil)
			if status == http.StatusNotFound {
				t.Errorf("%s is not mounted", p)
				continue
			}
			if policy[p] != public && !strings.Contains(body, `"unauthenticated"`) {
				t.Errorf("%s without credentials: %d %s, want unauthenticated", p, status, body)
			}
			if policy[p] == public && strings.Contains(body, `"unauthenticated"`) {
				t.Errorf("public %s asks for credentials: %s", p, body)
			}
		}
	}
}

func TestGuardChecksRoleAndMethod(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withEveryService(t))
	as := func(a domain.Actor, method string) *http.Client {
		return e.clientWith(testActorHeader, actorHeader(a, method))
	}

	// Users may list workers but not manage them; admins may.
	userWorkers := syncrv1connect.NewWorkerAdminServiceClient(as(testUser, "session"), e.srv.URL)
	if _, err := userWorkers.ListWorkers(t.Context(), connect.NewRequest(&syncrv1.ListWorkersRequest{})); err != nil {
		t.Errorf("user ListWorkers: %v", err)
	}
	_, err := userWorkers.RenameWorker(t.Context(), connect.NewRequest(&syncrv1.RenameWorkerRequest{WorkerId: e.worker.String(), Name: "x"}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = userWorkers.CreateEnrollmentToken(t.Context(), connect.NewRequest(&syncrv1.CreateEnrollmentTokenRequest{}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = userWorkers.GetEnrollmentToken(t.Context(), connect.NewRequest(&syncrv1.GetEnrollmentTokenRequest{TokenId: domain.NewEnrollmentTokenID().String()}))
	wantCode(t, err, connect.CodePermissionDenied)
	adminWorkers := syncrv1connect.NewWorkerAdminServiceClient(as(testAdmin, "token"), e.srv.URL)
	if _, err := adminWorkers.RenameWorker(t.Context(), connect.NewRequest(&syncrv1.RenameWorkerRequest{WorkerId: e.worker.String(), Name: "x"})); err != nil {
		t.Errorf("admin RenameWorker: %v", err)
	}

	// Upgrade commands are for admins; any user may fetch host keys.
	_, err = userWorkers.GetWorkerUpgradeCommands(t.Context(), connect.NewRequest(&syncrv1.GetWorkerUpgradeCommandsRequest{WorkerId: e.worker.String()}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = adminWorkers.GetWorkerUpgradeCommands(t.Context(), connect.NewRequest(&syncrv1.GetWorkerUpgradeCommandsRequest{WorkerId: e.worker.String()}))
	wantPastGuard(t, err)
	for _, a := range []domain.Actor{testUser, testAdmin} {
		_, err = syncrv1connect.NewProbeServiceClient(as(a, "token"), e.srv.URL).GetSftpHostKeys(t.Context(),
			connect.NewRequest(&syncrv1.GetSftpHostKeysRequest{Host: "sftp.example.com"}))
		wantPastGuard(t, err)
	}

	// UserService is admin only.
	_, err = syncrv1connect.NewUserServiceClient(as(testUser, "session"), e.srv.URL).ListUsers(t.Context(), connect.NewRequest(&syncrv1.ListUsersRequest{}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = syncrv1connect.NewUserServiceClient(as(testAdmin, "session"), e.srv.URL).ListUsers(t.Context(), connect.NewRequest(&syncrv1.ListUsersRequest{}))
	wantPastGuard(t, err)

	// An admin's API token cannot create accounts or set passwords: either
	// would let a leaked token sign in. A session can; a user cannot either
	// way.
	resetPassword := func(c *http.Client) error {
		_, err := syncrv1connect.NewUserServiceClient(c, e.srv.URL).ResetPassword(t.Context(),
			connect.NewRequest(&syncrv1.ResetPasswordRequest{UserId: testAdmin.UserID.String(), NewPassword: "a long enough password"}))
		return err
	}
	createUser := func(c *http.Client) error {
		_, err := syncrv1connect.NewUserServiceClient(c, e.srv.URL).CreateUser(t.Context(),
			connect.NewRequest(&syncrv1.CreateUserRequest{Username: "mallory", Password: "a long enough password", Role: syncrv1.UserRole_USER_ROLE_ADMIN}))
		return err
	}
	for _, call := range []func(*http.Client) error{resetPassword, createUser} {
		wantCode(t, call(as(testAdmin, "token")), connect.CodePermissionDenied)
		wantCode(t, call(as(testUser, "session")), connect.CodePermissionDenied)
		wantPastGuard(t, call(as(testAdmin, "session")))
	}
	_, err = syncrv1connect.NewUserServiceClient(as(testAdmin, "token"), e.srv.URL).ListUsers(t.Context(), connect.NewRequest(&syncrv1.ListUsersRequest{}))
	wantPastGuard(t, err) // other admin procedures accept tokens

	// API tokens cannot mint tokens, whatever the role; sessions can.
	for _, a := range []domain.Actor{testUser, testAdmin} {
		_, err = syncrv1connect.NewAuthServiceClient(as(a, "token"), e.srv.URL).CreateAPIToken(t.Context(), connect.NewRequest(&syncrv1.CreateAPITokenRequest{Name: "x"}))
		wantCode(t, err, connect.CodePermissionDenied)
	}
	_, err = syncrv1connect.NewAuthServiceClient(as(testUser, "session"), e.srv.URL).CreateAPIToken(t.Context(), connect.NewRequest(&syncrv1.CreateAPITokenRequest{Name: "x"}))
	wantPastGuard(t, err)
	_, err = syncrv1connect.NewAuthServiceClient(as(testUser, "token"), e.srv.URL).GetSession(t.Context(), connect.NewRequest(&syncrv1.GetSessionRequest{}))
	wantPastGuard(t, err) // tokens may call user procedures

	// Denials are logged without credentials.
	if logs := e.logs.String(); !strings.Contains(logs, "api call denied") || strings.Contains(logs, testActorHeader) {
		t.Errorf("logs = %q", logs)
	}
}

// wantPastGuard asserts that a call got past the guard: the stand-in use
// cases of withEveryService have no behavior, so the handler fails, but not
// with the guard's codes.
func wantPastGuard(t *testing.T, err error) {
	t.Helper()
	if code := connect.CodeOf(err); code == connect.CodeUnauthenticated || code == connect.CodePermissionDenied {
		t.Errorf("call refused by the guard: %v", err)
	}
}

func TestGuardFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := newGuard(testAuth{}, logging.Discard())
	h := http.Header{}
	h.Set(testActorHeader, actorHeader(testAdmin, "session"))

	// A procedure without a policy entry is denied, even for an admin.
	_, err := g.check(ctx, h, connect.Spec{Procedure: "/syncr.v1.NewService/Anything"}, "peer")
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("unknown procedure: %v", err)
	}
	// Without an authenticator only public procedures work.
	open := newGuard(nil, logging.Discard())
	if _, err := open.check(ctx, h, connect.Spec{Procedure: syncrv1connect.SystemServiceGetInfoProcedure}, "peer"); err != nil {
		t.Errorf("public procedure without authenticator: %v", err)
	}
	_, err = open.check(ctx, h, connect.Spec{Procedure: syncrv1connect.JobServiceListJobsProcedure}, "peer")
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("protected procedure without authenticator: %v", err)
	}
	// An authenticator that accepts without naming the caller is a bug:
	// Internal, not access.
	lax := newGuard(fakeAuthNoCaller{}, logging.Discard())
	_, err = lax.check(ctx, h, connect.Spec{Procedure: syncrv1connect.JobServiceListJobsProcedure}, "peer")
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("authenticator without caller: %v", err)
	}
	// Public procedures never reach the authenticator.
	boom := newGuard(fakeAuth{fn: func(http.Header) error { return errors.New("must not be called") }}, logging.Discard())
	if _, err := boom.check(ctx, h, connect.Spec{Procedure: syncrv1connect.AuthServiceLoginProcedure}, "peer"); err != nil {
		t.Errorf("Login consulted the authenticator: %v", err)
	}
}

type fakeAuthNoCaller struct{}

func (fakeAuthNoCaller) Authenticate(ctx context.Context, _ http.Header, _ connect.Spec) (context.Context, error) {
	return ctx, nil
}

// TestCrossOriginRequestsAreRejected: browsers announce cross-origin
// requests (Sec-Fetch-Site, Origin); http.CrossOriginProtection refuses
// them before any service runs. Clients without these headers (workers,
// curl) are unaffected.
func TestCrossOriginRequestsAreRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withEveryService(t))
	p := syncrv1connect.AuthServiceLoginProcedure
	for name, h := range map[string]http.Header{
		"cross-site fetch": {"Sec-Fetch-Site": {"cross-site"}},
		"same-site fetch":  {"Sec-Fetch-Site": {"same-site"}},
		"foreign origin":   {"Origin": {"https://attacker.example"}},
	} {
		if status, body := rawCall(t, e, p, false, h); status != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403", name, status, body)
		}
	}
	for name, h := range map[string]http.Header{
		"no browser headers": nil,
		"same-origin fetch":  {"Sec-Fetch-Site": {"same-origin"}},
		"same origin":        {"Origin": {e.srv.URL}},
	} {
		if status, body := rawCall(t, e, p, false, h); status == http.StatusForbidden {
			t.Errorf("%s: %d %s, want it through", name, status, body)
		}
	}
	// Safe methods pass (Connect GET calls of NO_SIDE_EFFECTS procedures).
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.srv.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("cross-site GET /healthz: %d", resp.StatusCode)
	}
}

func TestCallerContext(t *testing.T) {
	t.Parallel()
	if _, ok := CallerFrom(context.Background()); ok {
		t.Error("empty context has a caller")
	}
	c := Caller{Actor: testUser, Method: AuthSession}
	got, ok := CallerFrom(WithCaller(context.Background(), c))
	if !ok || got.Actor != testUser || got.Method != AuthSession {
		t.Errorf("CallerFrom = (%+v, %v)", got, ok)
	}
	if a, ok := ActorFrom(WithCaller(context.Background(), c)); !ok || a != testUser {
		t.Errorf("ActorFrom = (%+v, %v)", a, ok)
	}
	for m, want := range map[AuthMethod]string{AuthNone: "none", AuthSession: "session", AuthToken: "token"} {
		if m.String() != want {
			t.Errorf("%d.String() = %q", m, m.String())
		}
	}
	for a, want := range map[access]string{public: "public", user: "user", userSession: "user (session)", admin: "admin", adminSession: "admin (session)", 99: "unknown"} {
		if a.String() != want {
			t.Errorf("access %d = %q", a, a.String())
		}
	}
}
