package api

import (
	"context"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// The users use cases the API calls are implemented by *users.Service
// (Milestone 3 track A), which adds the compile-time checks:
//
//	var (
//		_ IdentityService  = (*users.Service)(nil)
//		_ UserAdminService = (*users.Service)(nil)
//	)

// IdentityService is the part of the users use cases behind SetupService,
// AuthService and the session authenticator. See internal/app/users for the
// semantics.
type IdentityService interface {
	SetupRequired(ctx context.Context) (bool, error)
	CompleteSetup(ctx context.Context, p users.SetupParams) (users.User, users.IssuedSession, error)
	Login(ctx context.Context, p users.LoginParams) (users.User, users.IssuedSession, error)
	Logout(ctx context.Context, sessionToken string) error
	// AuthenticateSession resolves a raw session token (the cookie value).
	AuthenticateSession(ctx context.Context, token string) (domain.Actor, users.Session, error)
	// AuthenticateAPIToken resolves a raw personal API token (a bearer
	// token); the token is returned for its expiry.
	AuthenticateAPIToken(ctx context.Context, token string) (domain.Actor, users.APIToken, error)
	Me(ctx context.Context, actor domain.Actor) (users.User, error)
	ChangePassword(ctx context.Context, actor domain.Actor, sessionToken, current, next string) error
	CreateAPIToken(ctx context.Context, actor domain.Actor, name string, ttl time.Duration) (users.APIToken, string, error)
	ListAPITokens(ctx context.Context, actor domain.Actor) ([]users.APIToken, error)
	DeleteAPIToken(ctx context.Context, actor domain.Actor, id domain.APITokenID) error
}

// UserAdminService is the part of the users use cases behind UserService
// (admin only; the use cases check the role too).
type UserAdminService interface {
	List(ctx context.Context, actor domain.Actor) ([]users.User, error)
	Get(ctx context.Context, actor domain.Actor, id domain.UserID) (users.User, error)
	Create(ctx context.Context, actor domain.Actor, p users.CreateParams) (users.User, error)
	Update(ctx context.Context, actor domain.Actor, id domain.UserID, p users.UpdateParams) (users.User, error)
	ResetPassword(ctx context.Context, actor domain.Actor, id domain.UserID, password string) (users.User, error)
	Delete(ctx context.Context, actor domain.Actor, id domain.UserID) error
}
