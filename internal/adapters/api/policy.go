package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"

	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// access is what a procedure requires of its caller.
type access int

// Access levels, from least to most restricted.
const (
	// public procedures need no credentials; the authenticator is not
	// consulted.
	public access = iota
	// user procedures need an authenticated user (session or API token).
	user
	// userSession procedures need a session cookie: an API token cannot
	// change the password or mint and revoke tokens, so a leaked token
	// cannot entrench itself.
	userSession
	// admin procedures need an authenticated admin.
	admin
	// adminSession procedures need an admin with a session cookie: an
	// admin's API token cannot create accounts or set passwords, either of
	// which would give a leaked token a way to sign in and outlive its
	// revocation (as userSession does for the token holder's own account).
	adminSession
)

func (a access) String() string {
	switch a {
	case public:
		return "public"
	case user:
		return "user"
	case userSession:
		return "user (session)"
	case admin:
		return "admin"
	case adminSession:
		return "admin (session)"
	default:
		return "unknown"
	}
}

// apiServices are the services NewHandler serves, by full name. The worker
// link services (ControllerLinkService, EnrollmentService) are mounted by
// internal/controller/dispatch/active and authenticate workers themselves;
// WorkerLinkService is served by workers.
func apiServices() []string {
	return []string{
		syncrv1connect.SystemServiceName,
		syncrv1connect.SetupServiceName,
		syncrv1connect.AuthServiceName,
		syncrv1connect.UserServiceName,
		syncrv1connect.WorkerAdminServiceName,
		syncrv1connect.JobServiceName,
		syncrv1connect.BackendServiceName,
		syncrv1connect.RemoteServiceName,
		syncrv1connect.ScheduleServiceName,
		syncrv1connect.ChannelServiceName,
		syncrv1connect.ContactServiceName,
		syncrv1connect.DashboardServiceName,
		syncrv1connect.ResticServiceName,
		syncrv1connect.ProbeServiceName,
	}
}

// newPolicy returns the access level of every API procedure. A procedure
// missing from it is denied (PermissionDenied); a test checks that every
// method of every service in apiServices has an entry.
//
// Roles only gate procedures here. Object scoping (a user's own remotes and
// jobs, admins seeing every job) is done by the use cases with the actor.
func newPolicy() map[string]access {
	return map[string]access{
		syncrv1connect.SystemServiceGetInfoProcedure: public,

		syncrv1connect.SetupServiceGetSetupStatusProcedure: public,
		syncrv1connect.SetupServiceCompleteSetupProcedure:  public,

		syncrv1connect.AuthServiceLoginProcedure:          public,
		syncrv1connect.AuthServiceLogoutProcedure:         user,
		syncrv1connect.AuthServiceGetSessionProcedure:     user,
		syncrv1connect.AuthServiceListAPITokensProcedure:  user,
		syncrv1connect.AuthServiceChangePasswordProcedure: userSession,
		syncrv1connect.AuthServiceCreateAPITokenProcedure: userSession,
		syncrv1connect.AuthServiceDeleteAPITokenProcedure: userSession,

		syncrv1connect.UserServiceListUsersProcedure: admin,
		syncrv1connect.UserServiceGetUserProcedure:   admin,
		// Creating an account or setting a password yields a credential
		// that signs in, so an admin's API token may do neither.
		syncrv1connect.UserServiceCreateUserProcedure:    adminSession,
		syncrv1connect.UserServiceUpdateUserProcedure:    admin,
		syncrv1connect.UserServiceResetPasswordProcedure: adminSession,
		syncrv1connect.UserServiceDeleteUserProcedure:    admin,

		// Workers are shared: every user may see them and run jobs on them;
		// only admins manage them.
		syncrv1connect.WorkerAdminServiceListWorkersProcedure:               user,
		syncrv1connect.WorkerAdminServiceGetWorkerProcedure:                 user,
		syncrv1connect.WorkerAdminServiceCreateEnrollmentTokenProcedure:     admin,
		syncrv1connect.WorkerAdminServiceGetPassiveInstallCommandsProcedure: admin,
		syncrv1connect.WorkerAdminServiceRegisterPassiveWorkerProcedure:     admin,
		syncrv1connect.WorkerAdminServiceRenameWorkerProcedure:              admin,
		syncrv1connect.WorkerAdminServiceRemoveWorkerProcedure:              admin,
		syncrv1connect.WorkerAdminServiceSetWorkerLabelsProcedure:           admin,
		syncrv1connect.WorkerAdminServiceGetWorkerUpgradeCommandsProcedure:  admin,

		syncrv1connect.JobServiceRunJobProcedure:         user,
		syncrv1connect.JobServiceGetJobProcedure:         user,
		syncrv1connect.JobServiceListJobsProcedure:       user,
		syncrv1connect.JobServiceCancelJobProcedure:      user,
		syncrv1connect.JobServiceWatchJobProcedure:       user,
		syncrv1connect.JobServiceListJobSamplesProcedure: user,

		syncrv1connect.BackendServiceListBackendsProcedure: user,

		syncrv1connect.RemoteServiceListRemotesProcedure:  user,
		syncrv1connect.RemoteServiceGetRemoteProcedure:    user,
		syncrv1connect.RemoteServiceCreateRemoteProcedure: user,
		syncrv1connect.RemoteServiceUpdateRemoteProcedure: user,
		syncrv1connect.RemoteServiceDeleteRemoteProcedure: user,

		// Schedules and contacts are owner-only (admins included); the use
		// cases scope them. SendTestNotification is rate limited by the
		// contacts use case.
		syncrv1connect.ScheduleServiceListSchedulesProcedure:      user,
		syncrv1connect.ScheduleServiceGetScheduleProcedure:        user,
		syncrv1connect.ScheduleServiceCreateScheduleProcedure:     user,
		syncrv1connect.ScheduleServiceUpdateScheduleProcedure:     user,
		syncrv1connect.ScheduleServiceSetScheduleEnabledProcedure: user,
		syncrv1connect.ScheduleServiceDeleteScheduleProcedure:     user,
		syncrv1connect.ScheduleServiceRunScheduleNowProcedure:     user,
		syncrv1connect.ScheduleServiceListScheduleRunsProcedure:   user,
		syncrv1connect.ScheduleServicePreviewScheduleProcedure:    user,

		syncrv1connect.ChannelServiceListChannelsProcedure: user,

		syncrv1connect.ContactServiceListContactsProcedure:         user,
		syncrv1connect.ContactServiceGetContactProcedure:           user,
		syncrv1connect.ContactServiceCreateContactProcedure:        user,
		syncrv1connect.ContactServiceUpdateContactProcedure:        user,
		syncrv1connect.ContactServiceDeleteContactProcedure:        user,
		syncrv1connect.ContactServiceSendTestNotificationProcedure: user,
		syncrv1connect.ContactServiceListDeliveriesProcedure:       user,
		syncrv1connect.ContactServiceRetryDeliveryProcedure:        user,

		syncrv1connect.DashboardServiceGetDashboardProcedure: user,

		// Repositories are owner-only, admins included: the scoping is in
		// the restic use cases.
		syncrv1connect.ResticServiceListRepositoriesProcedure: user,
		syncrv1connect.ResticServiceGetRepositoryProcedure:    user,
		syncrv1connect.ResticServiceCreateRepositoryProcedure: user,
		syncrv1connect.ResticServiceUpdateRepositoryProcedure: user,
		syncrv1connect.ResticServiceDeleteRepositoryProcedure: user,
		syncrv1connect.ResticServiceListSnapshotsProcedure:    user,
		syncrv1connect.ResticServiceBrowseSnapshotProcedure:   user,
		// Milestone 7: measuring, searching and comparing snapshots, owner
		// only like browsing them.
		syncrv1connect.ResticServiceMeasureSnapshotFolderProcedure: user,
		syncrv1connect.ResticServiceFindInRepositoryProcedure:      user,
		syncrv1connect.ResticServiceCompareSnapshotsProcedure:      user,

		// Any user may browse a worker's local roots; remotes are the
		// caller's own. The probe use cases scope and rate limit the calls.
		syncrv1connect.ProbeServiceListWorkerFilesProcedure: user,
		syncrv1connect.ProbeServiceListRemoteFilesProcedure: user,
		syncrv1connect.ProbeServiceTestRemoteProcedure:      user,
		// Milestone 6: any user may fetch a server's host keys (rate
		// limited by the probe use cases, like the probes above).
		syncrv1connect.ProbeServiceGetSftpHostKeysProcedure: user,
		// Milestone 7: any user may measure any worker's folders, as they
		// may list them; remotes stay the caller's own.
		syncrv1connect.ProbeServiceMeasureWorkerFolderProcedure: user,
		syncrv1connect.ProbeServiceMeasureRemoteFolderProcedure: user,
		syncrv1connect.ProbeServiceGetRemoteUsageProcedure:      user,
	}
}

// guard is the interceptor of every API service: it looks up the
// procedure's access level, authenticates the call unless it is public
// (the Authenticator puts the Caller into the context) and checks the
// caller against the level. It runs before any handler code, for unary and
// streaming calls, and repeats the check while an authenticated stream is
// open (streamauth.go).
type guard struct {
	// auth authenticates calls; nil denies every non-public procedure.
	auth   Authenticator
	policy map[string]access
	log    *slog.Logger
	// reauth is how often an open authenticated stream is checked again.
	reauth time.Duration
}

var _ connect.Interceptor = (*guard)(nil)

func newGuard(auth Authenticator, log *slog.Logger) *guard {
	return &guard{auth: auth, policy: newPolicy(), log: log, reauth: defaultStreamReauth}
}

func (g *guard) check(ctx context.Context, header http.Header, spec connect.Spec, peer string) (context.Context, error) {
	level, ok := g.policy[spec.Procedure]
	if !ok {
		g.log.ErrorContext(ctx, "api procedure has no access policy; denying the call", "procedure", spec.Procedure)
		return ctx, connect.NewError(connect.CodePermissionDenied, errors.New("permission denied"))
	}
	if level == public {
		return ctx, nil
	}
	if g.auth == nil {
		return ctx, unauthenticated()
	}
	ctx, err := g.auth.Authenticate(ctx, header, spec)
	switch {
	case err == nil:
	case errors.Is(err, domain.ErrUnauthenticated):
		// The reason is logged, never returned: callers learn nothing about
		// what was wrong.
		g.log.WarnContext(ctx, "api authentication failed", "procedure", spec.Procedure, "peer", peer, "reason", err.Error())
		return ctx, unauthenticated()
	default:
		g.log.ErrorContext(ctx, "api authentication errored", "procedure", spec.Procedure, "error", err.Error())
		return ctx, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	c, ok := CallerFrom(ctx)
	if !ok {
		g.log.ErrorContext(ctx, "api authenticator accepted a call without identifying the caller", "procedure", spec.Procedure)
		return ctx, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	var denied string
	switch {
	case (level == admin || level == adminSession) && !c.Actor.IsAdmin():
		denied = "this call needs the admin role"
	case (level == userSession || level == adminSession) && c.Method != AuthSession:
		denied = "this call needs a web session; API tokens cannot make it"
	}
	if denied != "" {
		g.log.InfoContext(ctx, "api call denied", "procedure", spec.Procedure, "user_id", c.Actor.UserID.String(),
			"role", string(c.Actor.Role), "auth_method", c.Method.String(), "required", level.String())
		return ctx, connect.NewError(connect.CodePermissionDenied, errors.New(denied))
	}
	return ctx, nil
}

// unauthenticated is the error of a call without valid credentials.
func unauthenticated() error {
	cerr := connect.NewError(connect.CodeUnauthenticated, errors.New("missing or invalid credentials"))
	cerr.Meta().Set("WWW-Authenticate", `Bearer realm="syncr"`)
	return cerr
}

// WrapUnary implements connect.Interceptor.
func (g *guard) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		ctx, err := g.check(ctx, req.Header(), req.Spec(), req.Peer().Addr)
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

// WrapStreamingClient implements connect.Interceptor; clients are not
// affected.
func (g *guard) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler implements connect.Interceptor.
func (g *guard) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		actx, err := g.check(ctx, conn.RequestHeader(), conn.Spec(), conn.Peer().Addr)
		if err != nil {
			return err
		}
		if _, ok := CallerFrom(actx); !ok { // a public procedure
			return next(actx, conn)
		}
		return g.serveReauthenticated(ctx, actx, conn, next)
	}
}
