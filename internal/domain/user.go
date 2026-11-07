package domain

import "fmt"

// Role is what a user may do. The string values are stable: they are stored
// and exposed through the API.
type Role string

// Roles.
const (
	// RoleAdmin manages users and workers and may view and cancel every
	// user's jobs. Remotes stay private to their owner, admins included.
	RoleAdmin Role = "admin"
	// RoleUser manages their own remotes and jobs and may use every worker.
	RoleUser Role = "user"
)

// ParseRole converts a stored or transmitted value to a Role. Errors wrap
// ErrInvalidArgument.
func ParseRole(s string) (Role, error) {
	r := Role(s)
	if !r.Valid() {
		return "", fmt.Errorf("%w: role %q", ErrInvalidArgument, s)
	}
	return r, nil
}

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return r == RoleAdmin || r == RoleUser }

// Actor is the authenticated caller of a use case: who acts and with which
// role. The API's authenticator derives it from the session or API token,
// and handlers pass it explicitly to the services, which scope every read
// and write by it. The zero Actor is nobody: it owns nothing and is not an
// admin.
type Actor struct {
	UserID UserID
	Role   Role
}

// IsAdmin reports whether the actor has the admin role.
func (a Actor) IsAdmin() bool { return a.Role == RoleAdmin }

// Owns reports whether owner is the actor (and the actor is someone).
func (a Actor) Owns(owner UserID) bool { return !a.UserID.IsZero() && a.UserID == owner }

// CanView reports whether the actor may see an object of owner: their own,
// or anyone's for an admin. It is the rule for jobs; remotes are strictly
// owner-only (use Owns).
func (a Actor) CanView(owner UserID) bool { return a.Owns(owner) || a.IsAdmin() }
