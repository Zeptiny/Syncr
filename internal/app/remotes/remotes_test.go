package remotes_test

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/adapters/memstore"
	"github.com/Zeptiny/Syncr/internal/adapters/secrets"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/builtin"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/clock"
)

var t0 = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

type env struct {
	svc    *remotes.Service
	store  *memstore.Store
	clock  *clock.Fake
	cipher *flakyCipher
	logs   *bytes.Buffer
	alice  domain.Actor
	bob    domain.Actor
	admin  domain.Actor
}

// flakyCipher is the real keyring, which can be made to fail decrypting.
type flakyCipher struct {
	*secrets.Keyring
	failDecrypt bool
}

func (c *flakyCipher) Decrypt(ct, ad []byte) ([]byte, error) {
	if c.failDecrypt {
		return nil, errors.New("decrypt failed")
	}
	return c.Keyring.Decrypt(ct, ad)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	key := secrets.Key{ID: 1, Bytes: make([]byte, secrets.KeySize)}
	if _, err := crand.Read(key.Bytes); err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := backend.NewRegistry(builtin.Types()...)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{store: memstore.New(), clock: clock.NewFake(t0), cipher: &flakyCipher{Keyring: ring}, logs: &bytes.Buffer{}}
	ids := &idgen.Sequential{}
	for i, a := range []*domain.Actor{&e.alice, &e.bob, &e.admin} {
		role := domain.RoleUser
		if i == 2 {
			role = domain.RoleAdmin
		}
		*a = domain.Actor{UserID: ids.NewUserID(), Role: role}
		u := users.User{ID: a.UserID, Username: []string{"alice", "bob", "admin"}[i], Role: role, PasswordHash: "x",
			CreatedAt: t0, UpdatedAt: t0, PasswordChangedAt: t0, Version: 1}
		if err := e.store.CreateUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	e.svc, err = remotes.New(remotes.Deps{
		Remotes: e.store, Cipher: e.cipher, Clock: e.clock, IDs: ids, Backends: reg,
		Logger: slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}, remotes.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func s3Input(name string) remotes.Input {
	return remotes.Input{
		Name: name, Type: "s3",
		Options: map[string]string{"provider": "Minio", "access_key_id": "minio", "endpoint": "https://minio.example", "bucket": "b", "region": ""},
		Secrets: map[string]string{"secret_access_key": "s3-secret-value"},
	}
}

func localInput(name, path string) remotes.Input {
	return remotes.Input{Name: name, Type: "local", Options: map[string]string{"path": path}}
}

func (e *env) mustCreate(t *testing.T, a domain.Actor, in remotes.Input) remotes.Remote {
	t.Helper()
	r, err := e.svc.Create(context.Background(), a, in)
	if err != nil {
		t.Fatalf("Create(%s): %v", in.Name, err)
	}
	return r
}

// fields returns the field paths of a validation error, or fails.
func fields(t *testing.T, err error) []string {
	t.Helper()
	var verr *domain.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("err = %v, want a *domain.ValidationError", err)
	}
	var out []string
	for _, f := range verr.Fields {
		out = append(out, f.Field)
	}
	return out
}

func wantField(t *testing.T, what string, err error, field string) {
	t.Helper()
	if got := fields(t, err); !slices.Contains(got, field) {
		t.Errorf("%s: fields %v, want %q", what, got, field)
	}
}

func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: err = %v, want %v", what, err, want)
	}
}

// secretsOf decrypts r's stored secrets.
func (e *env) secretsOf(t *testing.T, r remotes.Remote) map[string]string {
	t.Helper()
	stored, err := e.store.GetRemote(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SecretsEnc == nil {
		return map[string]string{}
	}
	plain, err := e.cipher.Decrypt(stored.SecretsEnc, remotes.SecretsAD(r.ID))
	if err != nil {
		t.Fatalf("decrypt with the remote's associated data: %v", err)
	}
	var out map[string]string
	if err := json.Unmarshal(plain, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestNew(t *testing.T) {
	t.Parallel()
	if _, err := remotes.New(remotes.Deps{}, remotes.Config{}); err == nil {
		t.Error("New without dependencies succeeded")
	}
}

func TestCreateGetList(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	r := e.mustCreate(t, e.alice, s3Input("  Archive "))
	if r.Name != "Archive" || r.OwnerID != e.alice.UserID || r.Type != "s3" || r.Version != 1 ||
		!r.CreatedAt.Equal(t0) || !r.UpdatedAt.Equal(t0) || !slices.Equal(r.SecretKeys, []string{"secret_access_key"}) {
		t.Errorf("Create = %+v", r)
	}
	if _, ok := r.Options["region"]; ok || r.Options["bucket"] != "b" {
		t.Errorf("options = %v: empty values must be dropped", r.Options)
	}
	if got := e.secretsOf(t, r); !maps.Equal(got, map[string]string{"secret_access_key": "s3-secret-value"}) {
		t.Errorf("stored secrets = %v", got)
	}
	stored, _ := e.store.GetRemote(ctx, r.ID)
	if _, err := e.cipher.Decrypt(stored.SecretsEnc, remotes.SecretsAD(domain.NewRemoteID())); err == nil {
		t.Error("the secrets decrypt with another remote's associated data")
	}

	if got, err := e.svc.Get(ctx, e.alice, r.ID); err != nil || got.ID != r.ID {
		t.Errorf("Get(owner) = (%+v, %v)", got, err)
	}
	for _, a := range []domain.Actor{e.bob, e.admin, {}} {
		_, err := e.svc.Get(ctx, a, r.ID)
		wantErr(t, "Get(not the owner)", err, domain.ErrNotFound)
	}
	_, err := e.svc.Get(ctx, e.alice, domain.RemoteID{})
	wantErr(t, "Get(zero id)", err, domain.ErrInvalidID)

	e.mustCreate(t, e.alice, localInput("photos", "/srv/photos"))
	e.mustCreate(t, e.bob, localInput("bobs", "/srv/bob"))
	list, err := e.svc.List(ctx, e.alice, remotes.ListFilter{OwnerID: e.bob.UserID})
	if err != nil || len(list) != 2 || list[0].Name != "Archive" || list[1].Name != "photos" {
		t.Errorf("List(alice) = (%v, %v), want her two remotes by name", list, err)
	}
	if list, _ := e.svc.List(ctx, e.alice, remotes.ListFilter{Type: "local"}); len(list) != 1 {
		t.Errorf("List(type local) = %v", list)
	}
	if list, _ := e.svc.List(ctx, e.admin, remotes.ListFilter{}); len(list) != 0 {
		t.Errorf("an admin lists other users' remotes: %v", list)
	}
	if list, err := e.svc.List(ctx, domain.Actor{}, remotes.ListFilter{}); err != nil || len(list) != 0 {
		t.Errorf("List(no user) = (%v, %v), want nothing", list, err)
	}
	_, err = e.svc.Create(ctx, domain.Actor{Role: domain.RoleAdmin}, s3Input("x"))
	wantErr(t, "Create(no user)", err, domain.ErrPermissionDenied)
}

func TestCreateValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	mod := func(f func(*remotes.Input)) remotes.Input {
		in := s3Input("ok")
		f(&in)
		return in
	}
	for _, tt := range []struct {
		name  string
		in    remotes.Input
		field string
	}{
		{"empty name", mod(func(in *remotes.Input) { in.Name = "  " }), "name"},
		{"long name", mod(func(in *remotes.Input) { in.Name = strings.Repeat("é", 65) }), "name"},
		{"control character", mod(func(in *remotes.Input) { in.Name = "a\x07b" }), "name"},
		{"no type", mod(func(in *remotes.Input) { in.Type = "" }), "type"},
		{"unknown type", mod(func(in *remotes.Input) { in.Type = "ftp" }), "type"},
		{"missing required", mod(func(in *remotes.Input) { delete(in.Options, "access_key_id") }), "options.access_key_id"},
		{"empty required secret", mod(func(in *remotes.Input) { in.Secrets["secret_access_key"] = "" }), "secrets.secret_access_key"},
		{"secret in options", mod(func(in *remotes.Input) { in.Options["secret_access_key"] = "leak" }), "options.secret_access_key"},
		{"cross-field rule", mod(func(in *remotes.Input) { delete(in.Options, "endpoint") }), "options.endpoint"},
		{"clear on create", mod(func(in *remotes.Input) { in.ClearSecrets = []string{"x"} }), "clear_secrets[0]"},
		{"members of a non-union", mod(func(in *remotes.Input) {
			in.Members = []remotes.Member{{RemoteID: domain.NewRemoteID(), Mode: backend.UnionReadWrite}}
		}), "members"},
	} {
		_, err := e.svc.Create(context.Background(), e.alice, tt.in)
		wantErr(t, tt.name, err, domain.ErrInvalidArgument)
		if err != nil {
			wantField(t, tt.name, err, tt.field)
			if strings.Contains(err.Error(), "s3-secret-value") || strings.Contains(err.Error(), "leak") {
				t.Errorf("%s: the error contains a secret: %v", tt.name, err)
			}
		}
	}
	if list, _ := e.svc.List(context.Background(), e.alice, remotes.ListFilter{}); len(list) != 0 {
		t.Errorf("invalid remotes were stored: %v", list)
	}
}

func TestNamesUniquePerOwner(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	first := e.mustCreate(t, e.alice, localInput("Photos", "/p"))
	_, err := e.svc.Create(ctx, e.alice, localInput("PHOTOS", "/q"))
	wantErr(t, "Create(same name)", err, domain.ErrAlreadyExists)
	e.mustCreate(t, e.bob, localInput("photos", "/p"))
	if err := e.svc.Delete(ctx, e.alice, first.ID); err != nil {
		t.Fatal(err)
	}
	e.mustCreate(t, e.alice, localInput("photos", "/p"))
}

func TestUpdate(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	in := remotes.Input{
		Name: "Box", Type: "sftp",
		Options: map[string]string{"host": "h", "user": "u", "port": "2222"},
		Secrets: map[string]string{"pass": "old-pass", "key_pem": "old-key"},
	}
	r := e.mustCreate(t, e.alice, in)
	e.clock.Advance(time.Minute)

	// Absent and empty secrets are kept; options are replaced as a whole.
	upd := remotes.Input{Name: "Box 2", Options: map[string]string{"host": "h2", "user": "u"}, Secrets: map[string]string{"pass": ""}}
	r2, err := e.svc.Update(ctx, e.alice, r.ID, r.Version, upd)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Name != "Box 2" || r2.Version != 2 || !r2.UpdatedAt.Equal(t0.Add(time.Minute)) || !maps.Equal(r2.Options, upd.Options) {
		t.Errorf("Update = %+v", r2)
	}
	if got := e.secretsOf(t, r2); !maps.Equal(got, in.Secrets) {
		t.Errorf("secrets after keep = %v, want %v", got, in.Secrets)
	}

	// A non-empty secret replaces; ClearSecrets removes an optional one.
	upd = remotes.Input{Name: "Box 2", Type: "sftp", Options: upd.Options, Secrets: map[string]string{"pass": "new-pass"}, ClearSecrets: []string{"key_pem"}}
	r3, err := e.svc.Update(ctx, e.alice, r.ID, r2.Version, upd)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.secretsOf(t, r3); !maps.Equal(got, map[string]string{"pass": "new-pass"}) || !slices.Equal(r3.SecretKeys, []string{"pass"}) {
		t.Errorf("secrets after replace and clear = %v (keys %v)", got, r3.SecretKeys)
	}

	for _, tt := range []struct {
		name  string
		in    remotes.Input
		field string
	}{
		// The merged configuration is validated: no password and no key.
		{"clear the last credential", remotes.Input{Name: "x", Options: upd.Options, ClearSecrets: []string{"pass"}}, "secrets.pass"},
		{"clear an unknown field", remotes.Input{Name: "x", Options: upd.Options, ClearSecrets: []string{"host"}}, "clear_secrets[0]"},
		{"set and clear", remotes.Input{Name: "x", Options: upd.Options, Secrets: map[string]string{"key_pem": "k"}, ClearSecrets: []string{"key_pem"}}, "clear_secrets[0]"},
		{"change the type", remotes.Input{Name: "x", Type: "s3", Options: upd.Options}, "type"},
		{"invalid option", remotes.Input{Name: "x", Options: map[string]string{"host": "h", "user": "u", "port": "many"}}, "options.port"},
	} {
		_, err := e.svc.Update(ctx, e.alice, r.ID, r3.Version, tt.in)
		wantErr(t, tt.name, err, domain.ErrInvalidArgument)
		if err != nil {
			wantField(t, tt.name, err, tt.field)
		}
	}
	// A required secret cannot be cleared.
	s3r := e.mustCreate(t, e.alice, s3Input("S3"))
	s3in := s3Input("S3")
	s3in.Secrets, s3in.ClearSecrets = nil, []string{"secret_access_key"}
	_, err = e.svc.Update(ctx, e.alice, s3r.ID, s3r.Version, s3in)
	wantField(t, "clear a required secret", err, "clear_secrets[0]")

	_, err = e.svc.Update(ctx, e.alice, r.ID, r2.Version, upd)
	wantErr(t, "Update(stale version)", err, domain.ErrConflict)
	_, err = e.svc.Update(ctx, e.bob, r.ID, r3.Version, upd)
	wantErr(t, "Update(not the owner)", err, domain.ErrNotFound)
	_, err = e.svc.Update(ctx, e.admin, r.ID, r3.Version, upd)
	wantErr(t, "Update(admin, not the owner)", err, domain.ErrNotFound)
	if cur, _ := e.svc.Get(ctx, e.alice, r.ID); cur.Version != r3.Version {
		t.Errorf("failed updates changed the remote: version %d", cur.Version)
	}

	other := e.mustCreate(t, e.alice, localInput("Taken", "/t"))
	_, err = e.svc.Update(ctx, e.alice, other.ID, other.Version, localInput("box 2", "/t"))
	wantErr(t, "Update(name taken)", err, domain.ErrAlreadyExists)
}

func TestUnions(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	disk1 := e.mustCreate(t, e.alice, localInput("disk1", "/mnt/d1"))
	disk2 := e.mustCreate(t, e.alice, localInput("disk2", `D:\pool`))
	bucket := e.mustCreate(t, e.alice, s3Input("bucket"))
	bobs := e.mustCreate(t, e.bob, localInput("bobs", "/b"))
	gone := e.mustCreate(t, e.alice, localInput("gone", "/g"))
	if err := e.svc.Delete(ctx, e.alice, gone.ID); err != nil {
		t.Fatal(err)
	}
	rw, ro := backend.UnionReadWrite, backend.UnionReadOnly
	pool := func(ms ...remotes.Member) remotes.Input {
		return remotes.Input{Name: "Pool", Type: "union", Options: map[string]string{"create_policy": "mfs"}, Members: ms}
	}
	union := e.mustCreate(t, e.alice, pool(remotes.Member{RemoteID: disk1.ID, Path: "a", Mode: rw}, remotes.Member{RemoteID: bucket.ID, Path: "x/y", Mode: ro}))
	if len(union.Members) != 2 || union.Members[1].Path != "x/y" {
		t.Errorf("union members = %+v", union.Members)
	}

	for _, tt := range []struct {
		name    string
		members []remotes.Member
		field   string
	}{
		{"one member", []remotes.Member{{RemoteID: disk1.ID, Mode: rw}}, "members"},
		{"another user's remote", []remotes.Member{{RemoteID: disk1.ID, Mode: rw}, {RemoteID: bobs.ID, Mode: rw}}, "members[1].remote_id"},
		{"a deleted remote", []remotes.Member{{RemoteID: disk1.ID, Mode: rw}, {RemoteID: gone.ID, Mode: rw}}, "members[1].remote_id"},
		{"a missing remote", []remotes.Member{{RemoteID: domain.NewRemoteID(), Mode: rw}, {RemoteID: disk1.ID, Mode: rw}}, "members[0].remote_id"},
		{"no remote", []remotes.Member{{Mode: rw}, {RemoteID: disk1.ID, Mode: rw}}, "members[0].remote_id"},
		{"a union", []remotes.Member{{RemoteID: disk1.ID, Mode: rw}, {RemoteID: union.ID, Mode: rw}}, "members[1].remote_id"},
		{"a dot-dot path", []remotes.Member{{RemoteID: disk1.ID, Mode: rw}, {RemoteID: disk2.ID, Path: `a\..\b`, Mode: rw}}, "members[1].path"},
		{"a mode suffix", []remotes.Member{{RemoteID: disk1.ID, Path: "x:ro", Mode: rw}, {RemoteID: disk2.ID, Mode: rw}}, "members[0].path"},
		{"an unknown mode", []remotes.Member{{RemoteID: disk1.ID, Mode: "rw"}, {RemoteID: disk2.ID, Mode: rw}}, "members[0].mode"},
		{"a duplicate", []remotes.Member{{RemoteID: disk1.ID, Path: "a/", Mode: rw}, {RemoteID: disk1.ID, Path: "a/", Mode: ro}}, "members[1].path"},
	} {
		in := pool(tt.members...)
		in.Name = "Other"
		_, err := e.svc.Create(ctx, e.alice, in)
		wantErr(t, tt.name, err, domain.ErrInvalidArgument)
		if err != nil {
			wantField(t, tt.name, err, tt.field)
		}
	}
	// The same message for another user's remote and a missing one.
	msgOf := func(id domain.RemoteID) string {
		_, err := e.svc.Create(ctx, e.alice, pool(remotes.Member{RemoteID: id, Mode: rw}, remotes.Member{RemoteID: disk1.ID, Mode: rw}))
		var verr *domain.ValidationError
		if !errors.As(err, &verr) {
			t.Fatalf("Create = %v", err)
		}
		return verr.Fields[0].Message
	}
	if foreign, missing := msgOf(bobs.ID), msgOf(domain.NewRemoteID()); foreign != missing {
		t.Errorf("another user's remote and a missing one are told apart: %q vs %q", foreign, missing)
	}
	// A union cannot contain itself (on update).
	in := pool(remotes.Member{RemoteID: union.ID, Mode: rw}, remotes.Member{RemoteID: disk1.ID, Mode: rw})
	_, err := e.svc.Update(ctx, e.alice, union.ID, union.Version, in)
	wantField(t, "union contains itself", err, "members[0].remote_id")
	// Too many members.
	many := make([]remotes.Member, 17)
	for i := range many {
		many[i] = remotes.Member{RemoteID: disk1.ID, Path: strings.Repeat("p", i+1), Mode: rw}
	}
	_, err = e.svc.Create(ctx, e.alice, pool(many...))
	wantField(t, "too many members", err, "members")

	using, err := e.svc.UnionsUsing(ctx, e.alice, disk1.ID)
	if err != nil || len(using) != 1 || using[0].ID != union.ID {
		t.Errorf("UnionsUsing(disk1) = (%v, %v)", using, err)
	}
	_, err = e.svc.UnionsUsing(ctx, e.bob, disk1.ID)
	wantErr(t, "UnionsUsing(not the owner)", err, domain.ErrNotFound)

	err = e.svc.Delete(ctx, e.alice, disk1.ID)
	wantErr(t, "Delete(member of a live union)", err, domain.ErrFailedPrecondition)
	if err == nil || !strings.Contains(err.Error(), `"Pool"`) {
		t.Errorf("the error does not name the union: %v", err)
	}
	if err := e.svc.Delete(ctx, e.alice, union.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Delete(ctx, e.alice, disk1.ID); err != nil {
		t.Errorf("Delete(member of a deleted union) = %v", err)
	}
	wantErr(t, "Delete(deleted)", e.svc.Delete(ctx, e.alice, disk1.ID), domain.ErrNotFound)
	wantErr(t, "Delete(not the owner)", e.svc.Delete(ctx, e.bob, disk2.ID), domain.ErrNotFound)
}

func TestUsableResolveNames(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	disk := e.mustCreate(t, e.alice, localInput("disk", "/mnt/d"))
	box := e.mustCreate(t, e.alice, remotes.Input{Name: "box", Type: "sftp",
		Options: map[string]string{"host": "h", "user": "u", "path": "/srv"}, Secrets: map[string]string{"pass": "box-pass"}})
	union := e.mustCreate(t, e.alice, remotes.Input{Name: "pool", Type: "union", Members: []remotes.Member{
		{RemoteID: disk.ID, Path: "p", Mode: backend.UnionReadWrite}, {RemoteID: box.ID, Mode: backend.UnionNoCreate},
	}})
	bobs := e.mustCreate(t, e.bob, localInput("bobs", "/b"))
	gone := e.mustCreate(t, e.alice, localInput("gone", "/g"))
	if err := e.svc.Delete(ctx, e.alice, gone.ID); err != nil {
		t.Fatal(err)
	}

	names, err := e.svc.Usable(ctx, e.alice.UserID, []domain.RemoteID{union.ID, disk.ID, disk.ID})
	if err != nil || !maps.Equal(names, map[domain.RemoteID]string{union.ID: "pool", disk.ID: "disk"}) {
		t.Errorf("Usable = (%v, %v)", names, err)
	}
	if names, err := e.svc.Usable(ctx, e.alice.UserID, nil); err != nil || len(names) != 0 {
		t.Errorf("Usable(none) = (%v, %v)", names, err)
	}
	for what, ids := range map[string][]domain.RemoteID{
		"another user's": {disk.ID, bobs.ID},
		"deleted":        {gone.ID},
		"missing":        {domain.NewRemoteID()},
	} {
		_, err := e.svc.Usable(ctx, e.alice.UserID, ids)
		wantErr(t, "Usable("+what+")", err, domain.ErrNotFound)
		_, err = e.svc.Resolve(ctx, e.alice.UserID, ids)
		wantErr(t, "Resolve("+what+")", err, domain.ErrNotFound)
	}
	_, err = e.svc.Usable(ctx, domain.UserID{}, []domain.RemoteID{disk.ID})
	wantErr(t, "Usable(no owner)", err, domain.ErrNotFound)

	res, err := e.svc.Resolve(ctx, e.alice.UserID, []domain.RemoteID{union.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 {
		t.Fatalf("Resolve(union) = %v, want the union and its two members", slices.Collect(maps.Keys(res)))
	}
	u, d, b := res[union.ID], res[disk.ID], res[box.ID]
	if u.Name != "pool" || u.Rclone.Backend != "union" || u.Rclone.Options["create_policy"] != "epmfs" || len(u.Members) != 2 {
		t.Errorf("resolved union = %+v", u)
	}
	if d.Rclone.Backend != "" || d.Rclone.Root != "/mnt/d" || d.Type != "local" {
		t.Errorf("resolved local member = %+v", d)
	}
	if b.Rclone.Backend != "sftp" || b.Rclone.Secrets["pass"] != "box-pass" || !slices.Equal(b.Rclone.ObscureKeys, []string{"pass"}) || b.Rclone.Root != "/srv" {
		t.Errorf("resolved sftp member = %+v", b)
	}

	// Display names: any owner, deleted ones included, missing ones omitted.
	missing := domain.NewRemoteID()
	refs, err := e.svc.Names(ctx, []domain.RemoteID{bobs.ID, gone.ID, missing})
	if err != nil || len(refs) != 2 || refs[bobs.ID].Name != "bobs" || !refs[gone.ID].Deleted || refs[gone.ID].Type != "local" {
		t.Errorf("Names = (%+v, %v)", refs, err)
	}

	// Credentials that cannot be read are neither "not found" nor
	// "invalid": they are a failed precondition, which the dispatcher turns
	// into an internal job error at once instead of retrying (see
	// TestResolveUnreadableCredentials).
	e.cipher.failDecrypt = true
	_, err = e.svc.Resolve(ctx, e.alice.UserID, []domain.RemoteID{box.ID})
	if !errors.Is(err, domain.ErrFailedPrecondition) || errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Resolve(decrypt failure) = %v", err)
	}
}

// TestResolveUnreadableCredentials: stored secrets that cannot be decrypted
// or decoded make Resolve fail with ErrFailedPrecondition (directly and as a
// union member), without the secret or the ciphertext in the message; a
// remote without secrets still resolves.
func TestResolveUnreadableCredentials(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	box := e.mustCreate(t, e.alice, remotes.Input{Name: "box", Type: "sftp",
		Options: map[string]string{"host": "h", "user": "u"}, Secrets: map[string]string{"pass": "box-pass-sentinel"}})
	disk := e.mustCreate(t, e.alice, localInput("disk", "/mnt/d"))
	union := e.mustCreate(t, e.alice, remotes.Input{Name: "pool", Type: "union", Members: []remotes.Member{
		{RemoteID: disk.ID, Mode: backend.UnionReadWrite}, {RemoteID: box.ID, Mode: backend.UnionReadWrite},
	}})

	// Not JSON: e.g. written by a bug, or a ciphertext of another record
	// that happens to authenticate.
	stored, err := e.store.GetRemote(ctx, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.SecretsEnc, err = e.cipher.Encrypt([]byte("box-pass-sentinel not json"), remotes.SecretsAD(box.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.UpdateRemote(ctx, stored); err != nil {
		t.Fatal(err)
	}
	for what, ids := range map[string][]domain.RemoteID{"directly": {box.ID}, "as a union member": {union.ID}} {
		_, err := e.svc.Resolve(ctx, e.alice.UserID, ids)
		if !errors.Is(err, domain.ErrFailedPrecondition) {
			t.Errorf("Resolve(corrupt secrets, %s) = %v, want ErrFailedPrecondition", what, err)
		}
		if err != nil && strings.Contains(err.Error(), "sentinel") {
			t.Errorf("Resolve(corrupt secrets, %s) error contains the plaintext: %v", what, err)
		}
	}

	// A ciphertext the keyring cannot open (lost key, other record).
	e.cipher.failDecrypt = true
	if _, err := e.svc.Resolve(ctx, e.alice.UserID, []domain.RemoteID{box.ID}); !errors.Is(err, domain.ErrFailedPrecondition) {
		t.Errorf("Resolve(undecryptable secrets) = %v, want ErrFailedPrecondition", err)
	}
	if res, err := e.svc.Resolve(ctx, e.alice.UserID, []domain.RemoteID{disk.ID}); err != nil || len(res) != 1 {
		t.Errorf("Resolve(no secrets) = (%v, %v)", res, err)
	}
}

// TestNeverLogsSecrets runs every use case with secret values and checks the
// log.
func TestNeverLogsSecrets(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	in := s3Input("Archive")
	in.Secrets["secret_access_key"] = "sentinel-secret-1"
	r := e.mustCreate(t, e.alice, in)
	in.Secrets["secret_access_key"] = "sentinel-secret-2"
	r, err := e.svc.Update(ctx, e.alice, r.ID, r.Version, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Options["secret_access_key"] = "sentinel-secret-3"
	if _, err := e.svc.Update(ctx, e.alice, r.ID, r.Version, in); err == nil {
		t.Fatal("a secret in options was accepted")
	}
	if _, err := e.svc.Resolve(ctx, e.alice.UserID, []domain.RemoteID{r.ID}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Delete(ctx, e.alice, r.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.logs.String(), "remote created") {
		t.Fatalf("nothing was logged: %q", e.logs.String())
	}
	if strings.Contains(e.logs.String(), "sentinel-secret") {
		t.Errorf("the log contains a secret:\n%s", e.logs.String())
	}
	if stored, _ := e.store.GetRemote(ctx, r.ID); stored.SecretsEnc != nil {
		t.Error("a deleted remote keeps its secrets")
	}
}
