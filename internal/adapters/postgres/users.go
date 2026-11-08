package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Zeptiny/Syncr/internal/adapters/postgres/db"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Compile-time checks that Store implements the users ports.
var (
	_ users.Repository         = (*Store)(nil)
	_ users.SessionRepository  = (*Store)(nil)
	_ users.APITokenRepository = (*Store)(nil)
)

// firstUserLockID is the key of the transaction-level advisory lock that
// serializes CreateFirstUser (an arbitrary constant, distinct from
// migrationLockID).
const firstUserLockID int64 = 0x53594e4355 // "SYNCU"

// CreateUser implements users.Repository.
func (s *Store) CreateUser(ctx context.Context, u users.User) error {
	return mapErr(fmt.Sprintf("create user %s", u.ID), s.q.CreateUser(ctx, createUserParams(u)))
}

// CreateFirstUser implements users.Repository: in one transaction it takes
// a transaction-level advisory lock, counts the users and inserts u only if
// there are none. The lock makes concurrent calls run one after the other,
// and each count (a new statement under READ COMMITTED) sees the users
// committed before it, so exactly one of them inserts.
func (s *Store) CreateFirstUser(ctx context.Context, u users.User) error {
	op := fmt.Sprintf("create first user %s", u.ID)
	var inTx error // the transaction body's error, already mapped
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		inTx = createFirstUser(ctx, s.q.WithTx(tx), op, u)
		return inTx
	})
	if inTx != nil {
		return inTx
	}
	return mapErr(op, err) // Begin or Commit
}

func createFirstUser(ctx context.Context, q *db.Queries, op string, u users.User) error {
	if err := q.LockUsers(ctx, firstUserLockID); err != nil {
		return mapErr(op, err)
	}
	n, err := q.CountUsers(ctx)
	if err != nil {
		return mapErr(op, err)
	}
	if n > 0 {
		return fmt.Errorf("%s: a user exists: %w", op, domain.ErrFailedPrecondition)
	}
	return mapErr(op, q.CreateUser(ctx, createUserParams(u)))
}

// CountUsers implements users.Repository.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	n, err := s.q.CountUsers(ctx)
	if err != nil {
		return 0, mapErr("count users", err)
	}
	return int(n), nil
}

// GetUser implements users.Repository.
func (s *Store) GetUser(ctx context.Context, id domain.UserID) (users.User, error) {
	row, err := s.q.GetUser(ctx, pgID(id))
	if err != nil {
		return users.User{}, mapErr(fmt.Sprintf("get user %s", id), err)
	}
	return userFrom(row)
}

// GetUserByUsername implements users.Repository.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (users.User, error) {
	row, err := s.q.GetUserByUsername(ctx, username)
	if err != nil {
		return users.User{}, mapErr("get user by username", err)
	}
	return userFrom(row)
}

// ListUsers implements users.Repository.
func (s *Store) ListUsers(ctx context.Context) ([]users.User, error) {
	rows, err := s.q.ListUsers(ctx)
	if err != nil {
		return nil, mapErr("list users", err)
	}
	out := make([]users.User, len(rows))
	for i, r := range rows {
		if out[i], err = userFrom(r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// UpdateUser implements users.Repository: UPDATE ... WHERE id = $1 AND
// version = $2 RETURNING *. When it matches nothing, a second query tells a
// missing user (ErrNotFound) from a version mismatch (ErrConflict).
func (s *Store) UpdateUser(ctx context.Context, u users.User) (users.User, error) {
	op := fmt.Sprintf("update user %s", u.ID)
	row, err := s.q.UpdateUser(ctx, db.UpdateUserParams{
		ID:                pgID(u.ID),
		ExpectedVersion:   u.Version,
		DisplayName:       u.DisplayName,
		Role:              string(u.Role),
		PasswordHash:      u.PasswordHash,
		DisabledAt:        pgOptTime(u.DisabledAt),
		PasswordChangedAt: pgTime(u.PasswordChangedAt),
		LastLoginAt:       pgOptTime(u.LastLoginAt),
		UpdatedAt:         pgTime(u.UpdatedAt),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		exists, qerr := s.q.UserExists(ctx, pgID(u.ID))
		if qerr != nil {
			return users.User{}, mapErr(op, qerr)
		}
		if !exists {
			return users.User{}, fmt.Errorf("%s: %w", op, domain.ErrNotFound)
		}
		return users.User{}, fmt.Errorf("%s: not at version %d: %w", op, u.Version, domain.ErrConflict)
	}
	if err != nil {
		return users.User{}, mapErr(op, err)
	}
	return userFrom(row)
}

// CountActiveAdmins implements users.Repository.
func (s *Store) CountActiveAdmins(ctx context.Context) (int, error) {
	n, err := s.q.CountActiveAdmins(ctx)
	if err != nil {
		return 0, mapErr("count active admins", err)
	}
	return int(n), nil
}

// DeleteUser implements users.Repository. The user's sessions and API
// tokens cascade; a user still referenced by remotes or jobs fails the
// statement with a foreign key violation, which (unlike mapErr's reading of
// one on insert, a missing referenced row) means the user owns objects:
// ErrFailedPrecondition.
func (s *Store) DeleteUser(ctx context.Context, id domain.UserID) error {
	op := fmt.Sprintf("delete user %s", id)
	n, err := s.q.DeleteUser(ctx, pgID(id))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == sqlstateForeignKeyViolation {
			return fmt.Errorf("%s: %w: still referenced (constraint %s)", op, domain.ErrFailedPrecondition, pgErr.ConstraintName)
		}
		return mapErr(op, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
	}
	return nil
}

// CreateSession implements users.SessionRepository. An unknown user is a
// foreign key violation, reported as ErrNotFound.
func (s *Store) CreateSession(ctx context.Context, sess users.Session) error {
	err := s.q.CreateSession(ctx, db.CreateSessionParams{
		IDHash:        sess.IDHash[:],
		UserID:        pgID(sess.UserID),
		CreatedAt:     pgTime(sess.CreatedAt),
		LastSeenAt:    pgTime(sess.LastSeenAt),
		IdleExpiresAt: pgTime(sess.IdleExpiresAt),
		ExpiresAt:     pgTime(sess.ExpiresAt),
		UserAgent:     cleanText(sess.UserAgent),
		ClientIp:      cleanText(sess.ClientIP),
	})
	return mapErr("create session", err)
}

// GetSession implements users.SessionRepository.
func (s *Store) GetSession(ctx context.Context, h users.TokenHash) (users.Session, error) {
	row, err := s.q.GetSession(ctx, h[:])
	if err != nil {
		return users.Session{}, mapErr("get session", err)
	}
	return sessionFrom(row)
}

// TouchSession implements users.SessionRepository.
func (s *Store) TouchSession(ctx context.Context, h users.TokenHash, at, idleExpiresAt time.Time) error {
	n, err := s.q.TouchSession(ctx, db.TouchSessionParams{IDHash: h[:], At: pgTime(at), IdleExpiresAt: pgTime(idleExpiresAt)})
	if err != nil {
		return mapErr("touch session", err)
	}
	if n == 0 {
		return fmt.Errorf("touch session: %w", domain.ErrNotFound)
	}
	return nil
}

// DeleteSession implements users.SessionRepository.
func (s *Store) DeleteSession(ctx context.Context, h users.TokenHash) error {
	return mapErr("delete session", s.q.DeleteSession(ctx, h[:]))
}

// DeleteUserSessions implements users.SessionRepository.
func (s *Store) DeleteUserSessions(ctx context.Context, user domain.UserID, keep *users.TokenHash) (int, error) {
	p := db.DeleteUserSessionsParams{UserID: pgID(user)}
	if keep != nil {
		p.Keep = keep[:]
	}
	n, err := s.q.DeleteUserSessions(ctx, p)
	if err != nil {
		return 0, mapErr(fmt.Sprintf("delete sessions of user %s", user), err)
	}
	return int(n), nil
}

// DeleteExpiredSessions implements users.SessionRepository.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) (int, error) {
	n, err := s.q.DeleteExpiredSessions(ctx, pgTime(now))
	if err != nil {
		return 0, mapErr("delete expired sessions", err)
	}
	return int(n), nil
}

// CreateAPIToken implements users.APITokenRepository. An unknown user is a
// foreign key violation, reported as ErrNotFound.
func (s *Store) CreateAPIToken(ctx context.Context, t users.APIToken) error {
	err := s.q.CreateAPIToken(ctx, db.CreateAPITokenParams{
		ID:         pgID(t.ID),
		UserID:     pgID(t.UserID),
		Name:       t.Name,
		Prefix:     t.Prefix,
		Hash:       t.Hash[:],
		CreatedAt:  pgTime(t.CreatedAt),
		ExpiresAt:  pgOptTime(t.ExpiresAt),
		LastUsedAt: pgOptTime(t.LastUsedAt),
	})
	return mapErr(fmt.Sprintf("create api token %s", t.ID), err)
}

// GetAPITokenByHash implements users.APITokenRepository.
func (s *Store) GetAPITokenByHash(ctx context.Context, h users.TokenHash) (users.APIToken, error) {
	row, err := s.q.GetAPITokenByHash(ctx, h[:])
	if err != nil {
		return users.APIToken{}, mapErr("get api token", err)
	}
	return apiTokenFrom(row)
}

// ListAPITokens implements users.APITokenRepository.
func (s *Store) ListAPITokens(ctx context.Context, user domain.UserID) ([]users.APIToken, error) {
	rows, err := s.q.ListAPITokens(ctx, pgID(user))
	if err != nil {
		return nil, mapErr(fmt.Sprintf("list api tokens of user %s", user), err)
	}
	out := make([]users.APIToken, len(rows))
	for i, r := range rows {
		if out[i], err = apiTokenFrom(r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DeleteAPIToken implements users.APITokenRepository: the user is part of
// the condition, so another user's token is not found.
func (s *Store) DeleteAPIToken(ctx context.Context, user domain.UserID, id domain.APITokenID) error {
	op := fmt.Sprintf("delete api token %s", id)
	n, err := s.q.DeleteAPIToken(ctx, db.DeleteAPITokenParams{ID: pgID(id), UserID: pgID(user)})
	if err != nil {
		return mapErr(op, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
	}
	return nil
}

// TouchAPIToken implements users.APITokenRepository.
func (s *Store) TouchAPIToken(ctx context.Context, id domain.APITokenID, at time.Time) error {
	op := fmt.Sprintf("touch api token %s", id)
	n, err := s.q.TouchAPIToken(ctx, db.TouchAPITokenParams{ID: pgID(id), At: pgTime(at)})
	if err != nil {
		return mapErr(op, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
	}
	return nil
}

// Conversions.

func createUserParams(u users.User) db.CreateUserParams {
	return db.CreateUserParams{
		ID:                pgID(u.ID),
		Username:          u.Username,
		DisplayName:       u.DisplayName,
		Role:              string(u.Role),
		PasswordHash:      u.PasswordHash,
		DisabledAt:        pgOptTime(u.DisabledAt),
		CreatedAt:         pgTime(u.CreatedAt),
		UpdatedAt:         pgTime(u.UpdatedAt),
		PasswordChangedAt: pgTime(u.PasswordChangedAt),
		LastLoginAt:       pgOptTime(u.LastLoginAt),
		Version:           u.Version,
	}
}

func userFrom(r db.User) (users.User, error) {
	u := users.User{
		Username:          r.Username,
		DisplayName:       r.DisplayName,
		Role:              domain.Role(r.Role),
		PasswordHash:      r.PasswordHash,
		DisabledAt:        timeFrom(r.DisabledAt),
		CreatedAt:         timeFrom(r.CreatedAt),
		UpdatedAt:         timeFrom(r.UpdatedAt),
		PasswordChangedAt: timeFrom(r.PasswordChangedAt),
		LastLoginAt:       timeFrom(r.LastLoginAt),
		Version:           r.Version,
	}
	if err := decodeID(&u.ID, r.ID); err != nil {
		return users.User{}, err
	}
	return u, nil
}

// tokenHashFrom decodes a stored SHA-256 hash.
func tokenHashFrom(what string, b []byte) (users.TokenHash, error) {
	var h users.TokenHash
	if len(b) != len(h) {
		return h, fmt.Errorf("decode %s: hash is %d bytes, want %d", what, len(b), len(h))
	}
	copy(h[:], b)
	return h, nil
}

func sessionFrom(r db.Session) (users.Session, error) {
	h, err := tokenHashFrom("session", r.IDHash)
	if err != nil {
		return users.Session{}, err
	}
	sess := users.Session{
		IDHash:        h,
		CreatedAt:     timeFrom(r.CreatedAt),
		LastSeenAt:    timeFrom(r.LastSeenAt),
		IdleExpiresAt: timeFrom(r.IdleExpiresAt),
		ExpiresAt:     timeFrom(r.ExpiresAt),
		UserAgent:     r.UserAgent,
		ClientIP:      r.ClientIp,
	}
	if err := decodeID(&sess.UserID, r.UserID); err != nil {
		return users.Session{}, err
	}
	return sess, nil
}

func apiTokenFrom(r db.ApiToken) (users.APIToken, error) {
	h, err := tokenHashFrom("api token", r.Hash)
	if err != nil {
		return users.APIToken{}, err
	}
	t := users.APIToken{
		Name:       r.Name,
		Prefix:     r.Prefix,
		Hash:       h,
		CreatedAt:  timeFrom(r.CreatedAt),
		ExpiresAt:  timeFrom(r.ExpiresAt),
		LastUsedAt: timeFrom(r.LastUsedAt),
	}
	if err := decodeID(&t.ID, r.ID); err != nil {
		return users.APIToken{}, err
	}
	if err := decodeID(&t.UserID, r.UserID); err != nil {
		return users.APIToken{}, err
	}
	return t, nil
}
