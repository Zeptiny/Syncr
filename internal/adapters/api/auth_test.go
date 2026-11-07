package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/redact"
)

// protectedCalls performs every RPC of the worker and job services with a
// valid request shape, returning the call's error. Streams report the error
// of their first Receive.
func protectedCalls(e *testEnv, authorization string) map[string]func(ctx context.Context) error {
	w, j := e.workersAs(authorization), e.jobsAs(authorization)
	id := e.worker.String()
	return map[string]func(ctx context.Context) error{
		"CreateEnrollmentToken": func(ctx context.Context) error {
			_, err := w.CreateEnrollmentToken(ctx, connect.NewRequest(&syncrv1.CreateEnrollmentTokenRequest{}))
			return err
		},
		"RegisterPassiveWorker": func(ctx context.Context) error {
			_, err := w.RegisterPassiveWorker(ctx, connect.NewRequest(&syncrv1.RegisterPassiveWorkerRequest{
				Name: "p", Url: "https://p.example:7443", CertFingerprint: testFP, PairingSecret: pairingSecret,
			}))
			return err
		},
		"ListWorkers": func(ctx context.Context) error {
			_, err := w.ListWorkers(ctx, connect.NewRequest(&syncrv1.ListWorkersRequest{}))
			return err
		},
		"GetWorker": func(ctx context.Context) error {
			_, err := w.GetWorker(ctx, connect.NewRequest(&syncrv1.GetWorkerRequest{WorkerId: id}))
			return err
		},
		"RenameWorker": func(ctx context.Context) error {
			_, err := w.RenameWorker(ctx, connect.NewRequest(&syncrv1.RenameWorkerRequest{WorkerId: id, Name: "renamed"}))
			return err
		},
		"RemoveWorker": func(ctx context.Context) error {
			_, err := w.RemoveWorker(ctx, connect.NewRequest(&syncrv1.RemoveWorkerRequest{WorkerId: domain.NewWorkerID().String()}))
			return err
		},
		"RunJob": func(ctx context.Context) error {
			_, err := j.RunJob(ctx, connect.NewRequest(&syncrv1.RunJobRequest{WorkerId: id, Definition: testDefinition()}))
			return err
		},
		"GetJob": func(ctx context.Context) error {
			_, err := j.GetJob(ctx, connect.NewRequest(&syncrv1.GetJobRequest{JobId: domain.NewJobID().String()}))
			return err
		},
		"ListJobs": func(ctx context.Context) error {
			_, err := j.ListJobs(ctx, connect.NewRequest(&syncrv1.ListJobsRequest{}))
			return err
		},
		"CancelJob": func(ctx context.Context) error {
			_, err := j.CancelJob(ctx, connect.NewRequest(&syncrv1.CancelJobRequest{JobId: domain.NewJobID().String()}))
			return err
		},
		"WatchJob": func(ctx context.Context) error {
			stream, err := j.WatchJob(ctx, connect.NewRequest(&syncrv1.WatchJobRequest{JobId: domain.NewJobID().String()}))
			if err != nil {
				return err
			}
			defer stream.Close()
			for stream.Receive() {
			}
			return stream.Err()
		},
	}
}

func TestAuthRejectsBadCredentials(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withAdminToken(t))
	for name, header := range map[string]string{
		"missing":        "",
		"wrong token":    "Bearer " + strings.Repeat("x", len(adminToken)),
		"prefix only":    "Bearer " + adminToken[:len(adminToken)-1],
		"token appended": "Bearer " + adminToken + "x",
		"wrong scheme":   "Basic " + adminToken,
		"no scheme":      adminToken,
		"empty bearer":   "Bearer ",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, call := range protectedCalls(e, header) {
				wantCode(t, call(t.Context()), connect.CodeUnauthenticated)
			}
		})
	}
}

func TestAuthAcceptsAdminToken(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withAdminToken(t))
	for _, header := range []string{"Bearer " + adminToken, "bearer " + adminToken} {
		for rpc, call := range protectedCalls(e, header) {
			// Valid credentials get past authentication and the policy (the
			// token acts as an admin): the call either works or fails for
			// its own reasons (unknown IDs here; RunJob because the token
			// is no user that could own a job).
			if code := codeOf(t, call(t.Context())); code == connect.CodeUnauthenticated || code == connect.CodePermissionDenied {
				t.Errorf("%s with %q: %v", rpc, header[:6], code)
			}
		}
	}
}

func TestAuthFailureDetails(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withAdminToken(t))
	const wrong = "wrong-token-that-must-not-be-logged-0000"
	_, err := e.workersAs("Bearer "+wrong).ListWorkers(t.Context(), connect.NewRequest(&syncrv1.ListWorkersRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)

	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatal("not a connect error")
	}
	if got := cerr.Meta().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q", got)
	}
	if strings.Contains(err.Error(), "wrong-token") || strings.Contains(err.Error(), "wrong bearer") {
		t.Errorf("the error explains too much: %v", err)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "api authentication failed") {
		t.Errorf("failure not logged: %q", logs)
	}
	if strings.Contains(logs, wrong) || strings.Contains(logs, adminToken) {
		t.Errorf("logs contain a token: %q", logs)
	}
}

func TestPublicEndpointsNeedNoToken(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string][]func(*Deps){
		"protected services mounted": nil,
		"no token configured":        {func(d *Deps) { d.Auth = nil }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, mutate...)
			client := syncrv1connect.NewSystemServiceClient(e.srv.Client(), e.srv.URL)
			if _, err := client.GetInfo(t.Context(), connect.NewRequest(&syncrv1.GetInfoRequest{})); err != nil {
				t.Errorf("GetInfo: %v", err)
			}
			resp, err := e.srv.Client().Get(e.srv.URL + "/healthz")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("/healthz status = %d", resp.StatusCode)
			}
		})
	}
}

func TestProtectedServicesNotMountedWithoutAuth(t *testing.T) {
	t.Parallel()
	workerRPCs := []string{"CreateEnrollmentToken", "RegisterPassiveWorker", "ListWorkers", "GetWorker", "RenameWorker", "RemoveWorker"}
	jobRPCs := []string{"RunJob", "GetJob", "ListJobs", "CancelJob", "WatchJob"}
	tests := map[string]struct {
		mutate    func(*Deps)
		unmounted []string
	}{
		"no authenticator": {func(d *Deps) { d.Auth = nil }, append(slices.Clone(workerRPCs), jobRPCs...)},
		"no workers":       {func(d *Deps) { d.Workers = nil }, workerRPCs},
		"no jobs":          {func(d *Deps) { d.Jobs = nil }, jobRPCs},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, withAdminToken(t), tt.mutate)
			// Even the right token gets nothing: the services do not exist.
			for rpc, call := range protectedCalls(e, "Bearer "+adminToken) {
				err := call(t.Context())
				if slices.Contains(tt.unmounted, rpc) {
					wantCode(t, err, connect.CodeUnimplemented)
				} else if codeOf(t, err) == connect.CodeUnimplemented && rpc != "RunJob" {
					t.Errorf("%s is not served", rpc)
				}
			}
			if !strings.Contains(e.logs.String(), "disabled") {
				t.Errorf("no warning logged: %q", e.logs.String())
			}
		})
	}
}

func TestNoAuthWarnsAboutAdminToken(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(d *Deps) { d.Auth = nil })
	if logs := e.logs.String(); !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "SYNCR_ADMIN_TOKEN") {
		t.Errorf("logs = %q", logs)
	}
}

func TestNewAdminTokenAuth(t *testing.T) {
	t.Parallel()
	const short = "short-secret-token"
	for _, tok := range []redact.Secret{"", short} {
		_, err := NewAdminTokenAuth(tok)
		if err == nil {
			t.Fatalf("NewAdminTokenAuth(%q) succeeded", tok.Reveal())
		}
		if short != "" && strings.Contains(err.Error(), short) {
			t.Errorf("error leaks the token: %v", err)
		}
	}
	if _, err := NewAdminTokenAuth(redact.Secret(strings.Repeat("a", MinAdminTokenLen))); err != nil {
		t.Errorf("minimum length token: %v", err)
	}
}

func TestAdminTokenAuthenticate(t *testing.T) {
	t.Parallel()
	auth, err := NewAdminTokenAuth(adminToken)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		header string
		ok     bool
	}{
		{"Bearer " + adminToken, true},
		{"bearer " + adminToken, true},
		{"BEARER " + adminToken, true},
		{"  Bearer " + adminToken + "  ", true},
		{"Bearer  " + adminToken, true},
		{"", false},
		{"Bearer", false},
		{"Bearer ", false},
		{"Basic " + adminToken, false},
		{adminToken, false},
		{"Bearer " + strings.ToUpper(adminToken), false},
		{"Bearer " + adminToken + " extra", false},
	}
	for _, tt := range tests {
		h := http.Header{}
		if tt.header != "" {
			h.Set("Authorization", tt.header)
		}
		_, err := auth.Authenticate(context.Background(), h, connect.Spec{})
		if tt.ok != (err == nil) {
			t.Errorf("Authenticate(%q) = %v, want ok=%v", tt.header, err, tt.ok)
		}
		if err != nil && !errors.Is(err, domain.ErrUnauthenticated) {
			t.Errorf("Authenticate(%q) = %v, want ErrUnauthenticated", tt.header, err)
		}
	}
}

// fakeAuth is an authenticator whose decision is fn's: nil accepts the
// call as the test admin.
type fakeAuth struct{ fn func(h http.Header) error }

func (f fakeAuth) Authenticate(ctx context.Context, h http.Header, _ connect.Spec) (context.Context, error) {
	if err := f.fn(h); err != nil {
		return ctx, err
	}
	return WithCaller(ctx, Caller{Actor: testAdmin, Method: AuthSession}), nil
}

func TestAuthenticatorSeam(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(d *Deps) {
		d.Auth = fakeAuth{fn: func(h http.Header) error {
			switch h.Get("X-Test") {
			case "ok":
				return nil
			case "broken":
				return errors.New("session store exploded: dsn=secret")
			default:
				return fmt.Errorf("no session: %w", domain.ErrUnauthenticated)
			}
		}}
	})
	call := func(value string) error {
		client := syncrv1connect.NewWorkerAdminServiceClient(e.srv.Client(), e.srv.URL, connect.WithInterceptors(headerInterceptor("X-Test", value)))
		_, err := client.ListWorkers(t.Context(), connect.NewRequest(&syncrv1.ListWorkersRequest{}))
		return err
	}
	if err := call("ok"); err != nil {
		t.Errorf("accepted call: %v", err)
	}
	wantCode(t, call("none"), connect.CodeUnauthenticated)
	err := call("broken")
	wantCode(t, err, connect.CodeInternal)
	if strings.Contains(err.Error(), "exploded") || strings.Contains(err.Error(), "dsn") {
		t.Errorf("internal authentication error leaks: %v", err)
	}
}

// headerInterceptor sets a request header on unary client calls.
func headerInterceptor(key, value string) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set(key, value)
			return next(ctx, req)
		}
	})
}

func TestAdminTokenActsAsAdminWithoutAccount(t *testing.T) {
	t.Parallel()
	auth, err := NewAdminTokenAuth(adminToken)
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+adminToken)
	ctx, err := auth.Authenticate(context.Background(), h, connect.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	c, ok := CallerFrom(ctx)
	if !ok || !c.Actor.IsAdmin() || !c.Actor.UserID.IsZero() || c.Method != AuthToken {
		t.Errorf("caller = %+v (%v), want an admin without a user ID, by token", c, ok)
	}
	if a, ok := ActorFrom(context.Background()); ok || a != (domain.Actor{}) {
		t.Errorf("ActorFrom(empty context) = (%+v, %v)", a, ok)
	}
}
