package remotes_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

func isNotFound(err error) bool { return errors.Is(err, domain.ErrNotFound) }
func isDenied(err error) bool   { return errors.Is(err, domain.ErrPermissionDenied) }

func TestDraftNewRemote(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	in := s3Input("") // a remote can be tested before it is named
	rs, id, err := e.svc.Draft(ctx, e.alice, domain.RemoteID{}, in)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := rs[id]
	if id.IsZero() || !ok || len(rs) != 1 || r.ID != id || r.Type != "s3" || r.Rclone.Backend != "s3" ||
		r.Rclone.Secrets["secret_access_key"] != "s3-secret-value" {
		t.Errorf("Draft = id %s, %d remotes, type %q", id, len(rs), r.Type)
	}
	// Nothing is stored.
	if _, err := e.store.GetRemote(ctx, id); !isNotFound(err) {
		t.Errorf("the draft was stored: %v", err)
	}
	if list, _ := e.svc.List(ctx, e.alice, remotes.ListFilter{}); len(list) != 0 {
		t.Errorf("Draft stored %d remotes", len(list))
	}

	// It is validated as Create validates it.
	bad := s3Input("x")
	delete(bad.Secrets, "secret_access_key")
	_, _, err = e.svc.Draft(ctx, e.alice, domain.RemoteID{}, bad)
	wantField(t, "missing secret", err, "secrets.secret_access_key")
	_, _, err = e.svc.Draft(ctx, e.alice, domain.RemoteID{}, remotes.Input{Type: "nope"})
	wantField(t, "unknown type", err, "type")
	_, _, err = e.svc.Draft(ctx, e.alice, domain.RemoteID{}, remotes.Input{})
	wantField(t, "no type", err, "type")
	withClear := s3Input("x")
	withClear.ClearSecrets = []string{"secret_access_key"}
	_, _, err = e.svc.Draft(ctx, e.alice, domain.RemoteID{}, withClear)
	wantField(t, "clear_secrets on a new remote", err, "clear_secrets[0]")
	if _, _, err := e.svc.Draft(ctx, domain.Actor{}, domain.RemoteID{}, in); !isDenied(err) {
		t.Errorf("Draft without a user = %v", err)
	}
	if strings.Contains(e.logs.String(), "s3-secret-value") {
		t.Error("Draft logged a secret")
	}
}

func TestDraftStoredRemote(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	stored := e.mustCreate(t, e.alice, s3Input("Archive"))

	// Unsaved options, the stored secret kept.
	in := s3Input("Archive")
	in.Secrets = nil
	in.Options["endpoint"] = "https://other.example"
	rs, id, err := e.svc.Draft(ctx, e.alice, stored.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	r := rs[id]
	if id != stored.ID || r.Rclone.Options["endpoint"] != "https://other.example" || r.Rclone.Secrets["secret_access_key"] != "s3-secret-value" {
		t.Errorf("Draft(stored) = id %s endpoint %q, the stored secret kept: %v", id, r.Rclone.Options["endpoint"], r.Rclone.Secrets["secret_access_key"] == "s3-secret-value")
	}
	// A new secret replaces the stored one in the draft only.
	in.Secrets = map[string]string{"secret_access_key": "typed-secret"}
	rs, _, err = e.svc.Draft(ctx, e.alice, stored.ID, in)
	if err != nil || rs[stored.ID].Rclone.Secrets["secret_access_key"] != "typed-secret" {
		t.Errorf("Draft with a typed secret: %v", err)
	}
	if got := e.secretsOf(t, stored)["secret_access_key"]; got != "s3-secret-value" {
		t.Error("Draft changed the stored secret")
	}
	if cur, _ := e.svc.Get(ctx, e.alice, stored.ID); cur.Version != stored.Version || cur.Options["endpoint"] != "https://minio.example" {
		t.Errorf("Draft changed the stored remote: %+v", cur)
	}

	// Validated as Update validates it.
	other := s3Input("Archive")
	other.Type = "sftp"
	_, _, err = e.svc.Draft(ctx, e.alice, stored.ID, other)
	wantField(t, "type change", err, "type")
	clr := s3Input("Archive")
	clr.Secrets, clr.ClearSecrets = nil, []string{"secret_access_key"}
	_, _, err = e.svc.Draft(ctx, e.alice, stored.ID, clr)
	wantField(t, "clearing a required secret", err, "clear_secrets[0]")

	// Owner-only, admins included.
	for _, a := range []domain.Actor{e.bob, e.admin} {
		if _, _, err := e.svc.Draft(ctx, a, stored.ID, in); !isNotFound(err) {
			t.Errorf("Draft of another user's remote as %s = %v", a.Role, err)
		}
	}
	// Stored credentials that cannot be read.
	e.cipher.failDecrypt = true
	_, _, err = e.svc.Draft(ctx, e.alice, stored.ID, in)
	wantErr(t, "unreadable credentials", err, domain.ErrFailedPrecondition)
}

func TestDraftUnion(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	m1 := e.mustCreate(t, e.alice, s3Input("M1"))
	m2 := e.mustCreate(t, e.alice, localInput("M2", "/srv/pool"))
	foreign := e.mustCreate(t, e.bob, s3Input("Bob's"))
	union := remotes.Input{Type: "union", Members: []remotes.Member{
		{RemoteID: m1.ID, Path: "pool", Mode: backend.UnionReadWrite},
		{RemoteID: m2.ID, Mode: backend.UnionReadOnly},
	}}
	rs, id, err := e.svc.Draft(ctx, e.alice, domain.RemoteID{}, union)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 || len(rs[id].Members) != 2 || rs[m1.ID].Rclone.Secrets["secret_access_key"] != "s3-secret-value" || rs[m2.ID].Rclone.Root != "/srv/pool" {
		t.Errorf("Draft(union) resolved %d remotes, members %+v", len(rs), rs[id].Members)
	}
	union.Members[1].RemoteID = foreign.ID
	_, _, err = e.svc.Draft(ctx, e.alice, domain.RemoteID{}, union)
	wantField(t, "a foreign member", err, "members[1].remote_id")
}
