package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Zeptiny/Syncr/internal/adapters/postgres/db"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Compile-time check that Store implements the remotes port.
var _ remotes.Repository = (*Store)(nil)

// Remotes live in two tables: remotes, and union_members (a union's members
// in order). Every change of a remote and its member rows runs in one
// transaction (inTx). A member row is only inserted while its member remote
// is live, under a FOR SHARE lock, and DeleteRemote locks the remote FOR
// UPDATE before it checks the unions using it, so a remote can never be
// deleted while it is, or is becoming, a member of a live union.

// inTx runs fn in a transaction, committing if it returns nil. fn returns
// mapped errors; errors of the transaction itself are mapped with op.
func (s *Store) inTx(ctx context.Context, op string, fn func(q *db.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return mapErr(op+": begin", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // a no-op after Commit
	if err := fn(s.q.WithTx(tx)); err != nil {
		return err
	}
	return mapErr(op+": commit", tx.Commit(ctx))
}

// CreateRemote implements remotes.Repository.
func (s *Store) CreateRemote(ctx context.Context, r remotes.Remote) error {
	op := fmt.Sprintf("create remote %s", r.ID)
	options, err := encodeOptions(r.Options)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return s.inTx(ctx, op, func(q *db.Queries) error {
		if err := q.CreateRemote(ctx, db.CreateRemoteParams{
			ID:          pgID(r.ID),
			OwnerID:     pgID(r.OwnerID),
			Name:        r.Name,
			BackendType: r.Type,
			Options:     options,
			SecretsEnc:  r.SecretsEnc,
			SecretKeys:  textArray(r.SecretKeys),
			CreatedAt:   pgTime(r.CreatedAt),
			UpdatedAt:   pgTime(r.UpdatedAt),
			DeletedAt:   pgOptTime(r.DeletedAt),
			Version:     r.Version,
		}); err != nil {
			return mapErr(op, err)
		}
		return insertMembers(ctx, q, op, r.ID, r.Members)
	})
}

// insertMembers inserts the member rows of union id, in order.
func insertMembers(ctx context.Context, q *db.Queries, op string, id domain.RemoteID, members []remotes.Member) error {
	for i, m := range members {
		n, err := q.InsertUnionMember(ctx, db.InsertUnionMemberParams{
			UnionRemoteID:  pgID(id),
			Ord:            int32(i),
			MemberRemoteID: pgID(m.RemoteID),
			Path:           m.Path,
			Mode:           string(m.Mode),
		})
		if err != nil {
			return mapErr(fmt.Sprintf("%s: member %d", op, i), err)
		}
		if n == 0 {
			return fmt.Errorf("%s: member %d: remote %s: %w", op, i, m.RemoteID, domain.ErrNotFound)
		}
	}
	return nil
}

// GetRemote implements remotes.Repository.
func (s *Store) GetRemote(ctx context.Context, id domain.RemoteID) (remotes.Remote, error) {
	op := fmt.Sprintf("get remote %s", id)
	row, err := s.q.GetRemote(ctx, pgID(id))
	if err != nil {
		return remotes.Remote{}, mapErr(op, err)
	}
	out, err := s.withMembers(ctx, op, []db.Remote{row})
	if err != nil {
		return remotes.Remote{}, err
	}
	return out[0], nil
}

// GetRemotes implements remotes.Repository.
func (s *Store) GetRemotes(ctx context.Context, ids []domain.RemoteID) ([]remotes.Remote, error) {
	const op = "get remotes"
	if len(ids) == 0 {
		return []remotes.Remote{}, nil
	}
	pgIDs := make([]pgtype.UUID, len(ids))
	for i, id := range ids {
		pgIDs[i] = pgID(id)
	}
	rows, err := s.q.GetRemotes(ctx, pgIDs)
	if err != nil {
		return nil, mapErr(op, err)
	}
	return s.withMembers(ctx, op, rows)
}

// ListRemotes implements remotes.Repository.
func (s *Store) ListRemotes(ctx context.Context, f remotes.ListFilter) ([]remotes.Remote, error) {
	const op = "list remotes"
	var typ *string
	if f.Type != "" {
		typ = &f.Type
	}
	rows, err := s.q.ListRemotes(ctx, db.ListRemotesParams{OwnerID: pgOptID(f.OwnerID), BackendType: typ})
	if err != nil {
		return nil, mapErr(op, err)
	}
	return s.withMembers(ctx, op, rows)
}

// UpdateRemote implements remotes.Repository: UPDATE ... WHERE id AND
// version AND live RETURNING *, then the member rows are replaced, in one
// transaction. When the update matches nothing, a second query tells a
// missing or deleted remote (ErrNotFound) from a version mismatch
// (ErrConflict).
func (s *Store) UpdateRemote(ctx context.Context, r remotes.Remote) (remotes.Remote, error) {
	op := fmt.Sprintf("update remote %s", r.ID)
	options, err := encodeOptions(r.Options)
	if err != nil {
		return remotes.Remote{}, fmt.Errorf("%s: %w", op, err)
	}
	var out remotes.Remote
	err = s.inTx(ctx, op, func(q *db.Queries) error {
		row, err := q.UpdateRemote(ctx, db.UpdateRemoteParams{
			ID:              pgID(r.ID),
			ExpectedVersion: r.Version,
			Name:            r.Name,
			Options:         options,
			SecretsEnc:      r.SecretsEnc,
			SecretKeys:      textArray(r.SecretKeys),
			UpdatedAt:       pgTime(r.UpdatedAt),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			live, qerr := q.RemoteIsLive(ctx, pgID(r.ID))
			switch {
			case errors.Is(qerr, pgx.ErrNoRows) || (qerr == nil && !live):
				return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
			case qerr != nil:
				return mapErr(op, qerr)
			}
			return fmt.Errorf("%s: not at version %d: %w", op, r.Version, domain.ErrConflict)
		}
		if err != nil {
			return mapErr(op, err)
		}
		if err := q.DeleteUnionMembers(ctx, pgID(r.ID)); err != nil {
			return mapErr(op, err)
		}
		if err := insertMembers(ctx, q, op, r.ID, r.Members); err != nil {
			return err
		}
		if out, err = remoteFrom(row, nil); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
		out.Members = cloneMembers(r.Members)
		return nil
	})
	if err != nil {
		return remotes.Remote{}, err
	}
	return out, nil
}

// DeleteRemote implements remotes.Repository. The remote is locked first,
// so a member row being inserted concurrently either commits before the
// check (which then sees it) or waits and then finds the remote deleted.
func (s *Store) DeleteRemote(ctx context.Context, id domain.RemoteID, at time.Time) error {
	op := fmt.Sprintf("delete remote %s", id)
	return s.inTx(ctx, op, func(q *db.Queries) error {
		if _, err := q.LockLiveRemote(ctx, pgID(id)); err != nil {
			return mapErr(op, err) // no row: missing or deleted
		}
		unions, err := q.ListUnionsUsing(ctx, pgID(id))
		if err != nil {
			return mapErr(op, err)
		}
		if len(unions) > 0 {
			return fmt.Errorf("%s: it is a member of union %q: %w", op, unions[0].Name, domain.ErrFailedPrecondition)
		}
		return mapErr(op, q.SoftDeleteRemote(ctx, db.SoftDeleteRemoteParams{ID: pgID(id), DeletedAt: pgTime(at)}))
	})
}

// ListUnionsUsing implements remotes.Repository.
func (s *Store) ListUnionsUsing(ctx context.Context, member domain.RemoteID) ([]remotes.Remote, error) {
	op := fmt.Sprintf("list unions using remote %s", member)
	rows, err := s.q.ListUnionsUsing(ctx, pgID(member))
	if err != nil {
		return nil, mapErr(op, err)
	}
	return s.withMembers(ctx, op, rows)
}

// withMembers converts rows, loading the members of all of them with one
// query.
func (s *Store) withMembers(ctx context.Context, op string, rows []db.Remote) ([]remotes.Remote, error) {
	out := make([]remotes.Remote, 0, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]pgtype.UUID, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	memberRows, err := s.q.ListUnionMembers(ctx, ids)
	if err != nil {
		return nil, mapErr(op, err)
	}
	byUnion := map[pgtype.UUID][]db.UnionMember{}
	for _, m := range memberRows {
		byUnion[m.UnionRemoteID] = append(byUnion[m.UnionRemoteID], m)
	}
	for _, row := range rows {
		r, err := remoteFrom(row, byUnion[row.ID])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// remoteFrom converts a row and its member rows (in order).
func remoteFrom(row db.Remote, members []db.UnionMember) (remotes.Remote, error) {
	r := remotes.Remote{
		Name:       row.Name,
		Type:       row.BackendType,
		SecretsEnc: row.SecretsEnc,
		SecretKeys: row.SecretKeys,
		CreatedAt:  timeFrom(row.CreatedAt),
		UpdatedAt:  timeFrom(row.UpdatedAt),
		DeletedAt:  timeFrom(row.DeletedAt),
		Version:    row.Version,
	}
	if len(r.SecretKeys) == 0 {
		r.SecretKeys = nil
	}
	if err := decodeID(&r.ID, row.ID); err != nil {
		return remotes.Remote{}, err
	}
	if err := decodeID(&r.OwnerID, row.OwnerID); err != nil {
		return remotes.Remote{}, err
	}
	if err := json.Unmarshal(row.Options, &r.Options); err != nil {
		return remotes.Remote{}, fmt.Errorf("decode options of remote %s: %w", r.ID, err)
	}
	for _, m := range members {
		var id domain.RemoteID
		if err := decodeID(&id, m.MemberRemoteID); err != nil {
			return remotes.Remote{}, err
		}
		r.Members = append(r.Members, remotes.Member{RemoteID: id, Path: m.Path, Mode: backend.UnionMode(m.Mode)})
	}
	return r, nil
}

// encodeOptions encodes options as a JSON object ("{}" for none).
func encodeOptions(options map[string]string) ([]byte, error) {
	if options == nil {
		options = map[string]string{}
	}
	b, err := json.Marshal(options)
	if err != nil {
		return nil, fmt.Errorf("encode options: %w", err)
	}
	return b, nil
}

func cloneMembers(ms []remotes.Member) []remotes.Member {
	if len(ms) == 0 {
		return nil
	}
	return append([]remotes.Member(nil), ms...)
}
