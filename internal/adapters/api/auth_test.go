package api

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/adapters/memstore"
	"github.com/Zeptiny/Syncr/internal/adapters/password"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/clock"
	"github.com/Zeptiny/Syncr/internal/platform/ratelimit"
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

// withSessionAuth makes the server authenticate with the real SessionAuth
// instead of testAuth, over a users service of its own (memstore, a fast
// hasher) that knows testAdmin. It returns the mutation and a personal API
// token of testAdmin.
func withSessionAuth(t *testing.T) (mutate func(*Deps), token string) {
	t.Helper()
	mutate, token, _ = withSessionAuthService(t)
	return mutate, token
}

// withSessionAuthService is withSessionAuth that also returns the users
// service, to revoke the token.
func withSessionAuthService(t *testing.T) (mutate func(*Deps), token string, svc *users.Service) {
	t.Helper()
	ctx := context.Background()
	st := memstore.New()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	admin := users.User{ID: testAdmin.UserID, Username: "admin", Role: domain.RoleAdmin, PasswordHash: "x",
		CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now, Version: 1}
	if err := st.CreateUser(ctx, admin); err != nil {
		t.Fatal(err)
	}
	hasher, err := password.New(password.Config{Params: password.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}})
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(now)
	limiter := func() *ratelimit.Limiter {
		l, err := ratelimit.New(ratelimit.Config{Burst: 100, Every: time.Minute, MaxKeys: 100}, clk)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	svc, err = users.New(users.Deps{
		Users: st, Sessions: st, Tokens: st, Hasher: hasher,
		Limiters: users.Limiters{ClientIP: limiter(), Username: limiter(), Setup: limiter()},
		Clock:    clk, IDs: &idgen.Sequential{}, Random: crand.Reader,
	}, users.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, token, err = svc.CreateAPIToken(ctx, testAdmin, "tests", 0)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := NewSessionAuth(svc, CookieConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return func(d *Deps) { d.Auth = auth }, token, svc
}

func TestAuthRejectsBadCredentials(t *testing.T) {
	t.Parallel()
	mutate, token := withSessionAuth(t)
	e := newEnv(t, mutate)
	for name, header := range map[string]string{
		"missing":          "",
		"unknown token":    "Bearer " + users.APITokenPrefix + strings.Repeat("x", len(token)-len(users.APITokenPrefix)),
		"truncated token":  "Bearer " + token[:len(token)-1],
		"token appended":   "Bearer " + token + "x",
		"wrong scheme":     "Basic " + token,
		"no scheme":        token,
		"empty bearer":     "Bearer ",
		"not a pat":        "Bearer test-admin-token-0123456789-abcdefghij",
		"test actor value": actorHeader(testAdmin, "token"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for rpc, call := range protectedCalls(e, header) {
				if code := codeOf(t, call(t.Context())); code != connect.CodeUnauthenticated {
					t.Errorf("%s: %v, want unauthenticated", rpc, code)
				}
			}
		})
	}
}

func TestAuthAcceptsAPIToken(t *testing.T) {
	t.Parallel()
	mutate, token := withSessionAuth(t)
	e := newEnv(t, mutate)
	for _, header := range []string{"Bearer " + token, "bearer " + token} {
		for rpc, call := range protectedCalls(e, header) {
			// Valid credentials get past authentication and the policy (the
			// token's owner is an admin): the call either works or fails
			// for its own reasons (unknown IDs here).
			if code := codeOf(t, call(t.Context())); code == connect.CodeUnauthenticated || code == connect.CodePermissionDenied {
				t.Errorf("%s with %q: %v", rpc, header[:6], code)
			}
		}
	}
}

func TestAuthFailureDetails(t *testing.T) {
	t.Parallel()
	mutate, token := withSessionAuth(t)
	e := newEnv(t, mutate)
	wrong := users.APITokenPrefix + "wrong-token-that-must-not-be-logged-0000"
	_, err := e.workersAs("Bearer "+wrong).ListWorkers(t.Context(), connect.NewRequest(&syncrv1.ListWorkersRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)

	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatal("not a connect error")
	}
	if got := cerr.Meta().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q", got)
	}
	if strings.Contains(err.Error(), "wrong-token") || strings.Contains(err.Error(), "unknown") {
		t.Errorf("the error explains too much: %v", err)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "api authentication failed") {
		t.Errorf("failure not logged: %q", logs)
	}
	if strings.Contains(logs, wrong) || strings.Contains(logs, token) {
		t.Errorf("logs contain a token: %q", logs)
	}
}

func TestPublicEndpointsNeedNoToken(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string][]func(*Deps){
		"protected services mounted": nil,
		"no authenticator":           {func(d *Deps) { d.Auth = nil }},
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
			auth, token := withSessionAuth(t)
			e := newEnv(t, auth, tt.mutate)
			// Even a valid token gets nothing: the services do not exist.
			for rpc, call := range protectedCalls(e, "Bearer "+token) {
				err := call(t.Context())
				if slices.Contains(tt.unmounted, rpc) {
					wantCode(t, err, connect.CodeUnimplemented)
				} else if codeOf(t, err) == connect.CodeUnimplemented {
					t.Errorf("%s is not served", rpc)
				}
			}
			if !strings.Contains(e.logs.String(), "disabled") {
				t.Errorf("no warning logged: %q", e.logs.String())
			}
		})
	}
}

func TestNoAuthWarns(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(d *Deps) { d.Auth = nil })
	if logs := e.logs.String(); !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "no authenticator") {
		t.Errorf("logs = %q", logs)
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

func TestActorFromEmptyContext(t *testing.T) {
	t.Parallel()
	if a, ok := ActorFrom(context.Background()); ok || a != (domain.Actor{}) {
		t.Errorf("ActorFrom(empty context) = (%+v, %v)", a, ok)
	}
	if c, ok := CallerFrom(context.Background()); ok || c.Method != AuthNone {
		t.Errorf("CallerFrom(empty context) = (%+v, %v)", c, ok)
	}
}
