package remotes_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// cryptInput is an encrypted remote that stores its data at path on the
// remote wrapped.
func cryptInput(name string, wrapped domain.RemoteID, path string) remotes.Input {
	return remotes.Input{
		Name: name, Type: "crypt", Options: map[string]string{"filename_encryption": "standard"},
		Secrets: map[string]string{"password": "crypt-password-value", "password2": "crypt-salt-value"},
		Wrapped: remotes.Wrapped{RemoteID: wrapped, Path: path},
	}
}

// messages returns the field errors of a validation error by path.
func messages(t *testing.T, err error) map[string]string {
	t.Helper()
	var verr *domain.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("err = %v, want a *domain.ValidationError", err)
	}
	out := map[string]string{}
	for _, f := range verr.Fields {
		out[f.Field] = f.Message
	}
	return out
}

func TestWrappedCreateAndResolve(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	bucket := e.mustCreate(t, e.alice, s3Input("Bucket"))
	vault := e.mustCreate(t, e.alice, cryptInput("Vault", bucket.ID, "vault/data"))
	if vault.Wrapped != (remotes.Wrapped{RemoteID: bucket.ID, Path: "vault/data"}) || vault.Type != "crypt" ||
		!slices.Equal(vault.SecretKeys, []string{"password", "password2"}) {
		t.Errorf("Create(crypt) = %+v", vault)
	}
	if got, _ := e.svc.Get(ctx, e.alice, vault.ID); got.Wrapped != vault.Wrapped {
		t.Errorf("Get(crypt).Wrapped = %+v", got.Wrapped)
	}

	// Resolve follows the wrapped remote, with each type's capabilities.
	res, err := e.svc.Resolve(ctx, e.alice.UserID, []domain.RemoteID{vault.ID})
	if err != nil || len(res) != 2 {
		t.Fatalf("Resolve(crypt) = %d remotes, %v; want the crypt remote and its wrapped remote", len(res), err)
	}
	v, b := res[vault.ID], res[bucket.ID]
	if v.Wrapped != vault.Wrapped || v.Rclone.Backend != "crypt" || v.Rclone.Secrets["password"] != "crypt-password-value" ||
		v.Capabilities != (backend.Capabilities{Wrapper: true, NoChecksum: true}) {
		t.Errorf("resolved crypt = %+v", v)
	}
	if _, ok := v.Rclone.Options["remote"]; ok {
		t.Error("the resolved crypt remote has a remote option")
	}
	if b.Rclone.Backend != "s3" || !b.Wrapped.IsZero() || b.Capabilities.NoChecksum || !b.Capabilities.ResticRepository {
		t.Errorf("resolved wrapped remote = %+v", b)
	}

	// Infos: the type, the wrapped remote and the capabilities, for live
	// remotes of the owner only.
	infos, err := e.svc.Infos(ctx, e.alice.UserID, []domain.RemoteID{vault.ID, bucket.ID})
	if err != nil || len(infos) != 2 || infos[vault.ID].Type != "crypt" || infos[vault.ID].Wrapped != vault.Wrapped ||
		!infos[vault.ID].Capabilities.NoChecksum || infos[bucket.ID].Capabilities.NoChecksum || !infos[bucket.ID].Wrapped.IsZero() {
		t.Errorf("Infos = (%+v, %v)", infos, err)
	}
	if infos, err := e.svc.Infos(ctx, e.alice.UserID, nil); err != nil || len(infos) != 0 {
		t.Errorf("Infos(none) = (%v, %v)", infos, err)
	}
	bobs := e.mustCreate(t, e.bob, s3Input("Bob's"))
	for what, ids := range map[string][]domain.RemoteID{"foreign": {vault.ID, bobs.ID}, "missing": {domain.NewRemoteID()}} {
		_, err := e.svc.Infos(ctx, e.alice.UserID, ids)
		wantErr(t, "Infos("+what+")", err, domain.ErrNotFound)
	}
	if strings.Contains(e.logs.String(), "crypt-password-value") {
		t.Error("the log contains the crypt password")
	}
}

func TestWrappedValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	bucket := e.mustCreate(t, e.alice, s3Input("Bucket"))
	box := e.mustCreate(t, e.alice, remotes.Input{Name: "Box", Type: "sftp",
		Options: map[string]string{"host": "h", "user": "u", "path": "/srv"}, Secrets: map[string]string{"pass": "p"}})
	disk := e.mustCreate(t, e.alice, localInput("Disk", "/mnt/disk"))
	disk2 := e.mustCreate(t, e.alice, localInput("Disk 2", "/mnt/disk2"))
	pool := e.mustCreate(t, e.alice, remotes.Input{Name: "Pool", Type: "union", Members: []remotes.Member{
		{RemoteID: disk.ID, Mode: backend.UnionReadWrite}, {RemoteID: disk2.ID, Mode: backend.UnionReadWrite},
	}})
	vault := e.mustCreate(t, e.alice, cryptInput("Vault", bucket.ID, ""))
	bobs := e.mustCreate(t, e.bob, s3Input("Bob's"))
	gone := e.mustCreate(t, e.alice, s3Input("Gone"))
	if err := e.svc.Delete(ctx, e.alice, gone.ID); err != nil {
		t.Fatal(err)
	}
	const notYours = "is not one of your remotes"
	tests := []struct {
		name  string
		in    remotes.Input
		field string
		msg   string
	}{
		{"a wrapped remote on another type", func() remotes.Input {
			in := s3Input("x")
			in.Wrapped = remotes.Wrapped{RemoteID: bucket.ID}
			return in
		}(), "wrapped", "only encrypted remotes store their data in another remote"},
		{"a wrapped path on another type", func() remotes.Input {
			in := s3Input("x")
			in.Wrapped = remotes.Wrapped{Path: "x"}
			return in
		}(), "wrapped", "only encrypted remotes store their data in another remote"},
		{"no wrapped remote", cryptInput("x", domain.RemoteID{}, ""), "wrapped.remote_id", "is required"},
		{"a path without a remote", cryptInput("x", domain.RemoteID{}, "vault"), "wrapped.remote_id", "is required"},
		{"a missing remote", cryptInput("x", domain.NewRemoteID(), ""), "wrapped.remote_id", notYours},
		{"another user's remote", cryptInput("x", bobs.ID, ""), "wrapped.remote_id", notYours},
		{"a deleted remote", cryptInput("x", gone.ID, ""), "wrapped.remote_id", notYours},
		{"a local remote", cryptInput("x", disk.ID, ""), "wrapped.remote_id", "cannot be a folder of a worker"},
		{"a union", cryptInput("x", pool.ID, ""), "wrapped.remote_id", "cannot be a union or another encrypted remote"},
		{"another crypt remote", cryptInput("x", vault.ID, ""), "wrapped.remote_id", "cannot be a union or another encrypted remote"},
		{"a dot-dot path", cryptInput("x", box.ID, "a/../b"), "wrapped.path", `path must not contain ".." segments`},
		{"a line break", cryptInput("x", bucket.ID, "a\nb"), "wrapped.path", "path must not contain NUL bytes or line breaks"},
		{"a bad suffix", func() remotes.Input {
			in := cryptInput("x", bucket.ID, "")
			in.Options["suffix"] = "bin"
			return in
		}(), "options.suffix", ""},
		{"no password", func() remotes.Input {
			in := cryptInput("x", bucket.ID, "")
			delete(in.Secrets, "password")
			return in
		}(), "secrets.password", "is required"},
		{"the password as an option", func() remotes.Input {
			in := cryptInput("x", bucket.ID, "")
			in.Options["password"] = "leak-value"
			return in
		}(), "options.password", ""},
	}
	for _, tt := range tests {
		for op, call := range map[string]func() error{
			"Create": func() error { _, err := e.svc.Create(ctx, e.alice, tt.in); return err },
			"Draft":  func() error { _, _, err := e.svc.Draft(ctx, e.alice, domain.RemoteID{}, tt.in); return err },
		} {
			err := call()
			wantErr(t, op+"("+tt.name+")", err, domain.ErrInvalidArgument)
			if err == nil {
				continue
			}
			got := messages(t, err)
			if msg, ok := got[tt.field]; !ok || (tt.msg != "" && msg != tt.msg) {
				t.Errorf("%s(%s): fields %v, want %s %q", op, tt.name, got, tt.field, tt.msg)
			}
			for _, secret := range []string{"crypt-password-value", "leak-value"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("%s(%s): the error contains a secret: %v", op, tt.name, err)
				}
			}
		}
	}
	// A crypt remote cannot store its data in itself (on update).
	in := cryptInput("Vault", vault.ID, "")
	in.Secrets = nil
	_, err := e.svc.Update(ctx, e.alice, vault.ID, vault.Version, in)
	if got := messages(t, err); got["wrapped.remote_id"] != "an encrypted remote cannot store its data in itself" {
		t.Errorf("Update(wraps itself) fields = %v", got)
	}
	// Nor be a union member.
	_, err = e.svc.Create(ctx, e.alice, remotes.Input{Name: "Pool 2", Type: "union", Members: []remotes.Member{
		{RemoteID: disk.ID, Mode: backend.UnionReadWrite}, {RemoteID: vault.ID, Mode: backend.UnionReadWrite},
	}})
	if got := messages(t, err); got["members[1].remote_id"] != "cannot be a union member" {
		t.Errorf("Create(union of a crypt remote) fields = %v", got)
	}
	// Remotes of every kind that may hold one: s3, sftp with an absolute
	// root.
	e.mustCreate(t, e.alice, cryptInput("On box", box.ID, "secret"))
	if list, _ := e.svc.List(ctx, e.alice, remotes.ListFilter{Type: "crypt"}); len(list) != 2 {
		t.Errorf("crypt remotes = %v", list)
	}
}

// TestWrappedFixedFields: the password, the salt and the name settings
// keep their values; the name and the wrapped remote may change.
func TestWrappedFixedFields(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	bucket := e.mustCreate(t, e.alice, s3Input("Bucket"))
	other := e.mustCreate(t, e.alice, s3Input("Other"))
	vault := e.mustCreate(t, e.alice, cryptInput("Vault", bucket.ID, "vault"))

	keep := func(mutate func(*remotes.Input)) remotes.Input {
		in := cryptInput("Vault", bucket.ID, "vault")
		in.Secrets = nil
		if mutate != nil {
			mutate(&in)
		}
		return in
	}
	tests := []struct {
		name  string
		in    remotes.Input
		field string // "" when accepted
	}{
		{"another file name encryption", keep(func(in *remotes.Input) { in.Options["filename_encryption"] = "off" }), "options.filename_encryption"},
		{"the default written out", keep(func(in *remotes.Input) { in.Options["directory_name_encryption"] = "true" }), ""},
		{"a default left out", keep(func(in *remotes.Input) { delete(in.Options, "filename_encryption") }), ""},
		{"another default", keep(func(in *remotes.Input) { in.Options["directory_name_encryption"] = "false" }), "options.directory_name_encryption"},
		{"another encoding", keep(func(in *remotes.Input) { in.Options["filename_encoding"] = "base64" }), "options.filename_encoding"},
		{"another suffix", keep(func(in *remotes.Input) { in.Options["suffix"] = ".enc" }), "options.suffix"},
		{"a new password", keep(func(in *remotes.Input) { in.Secrets = map[string]string{"password": "another-value"} }), "secrets.password"},
		{"the same password typed again", keep(func(in *remotes.Input) { in.Secrets = map[string]string{"password": "crypt-password-value"} }), "secrets.password"},
		{"a new salt", keep(func(in *remotes.Input) { in.Secrets = map[string]string{"password2": "another-value"} }), "secrets.password2"},
		{"an empty password (kept)", keep(func(in *remotes.Input) { in.Secrets = map[string]string{"password": ""} }), ""},
		{"clearing the salt", keep(func(in *remotes.Input) { in.ClearSecrets = []string{"password2"} }), "clear_secrets[0]"},
		{"clearing the password", keep(func(in *remotes.Input) { in.ClearSecrets = []string{"password"} }), "clear_secrets[0]"},
		{"a new name and folder", keep(func(in *remotes.Input) { in.Name, in.Wrapped.Path = "Vault 2", "moved" }), ""},
		{"another wrapped remote", keep(func(in *remotes.Input) { in.Wrapped = remotes.Wrapped{RemoteID: other.ID, Path: "vault"} }), ""},
	}
	for _, tt := range tests {
		cur, err := e.svc.Get(ctx, e.alice, vault.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, _, draftErr := e.svc.Draft(ctx, e.alice, vault.ID, tt.in)
		updated, err := e.svc.Update(ctx, e.alice, vault.ID, cur.Version, tt.in)
		for op, err := range map[string]error{"Draft": draftErr, "Update": err} {
			if tt.field == "" {
				if err != nil {
					t.Errorf("%s(%s) = %v, want accepted", op, tt.name, err)
				}
				continue
			}
			if got := messages(t, err); got[tt.field] != "cannot be changed after the remote is created" {
				t.Errorf("%s(%s) fields = %v, want %s", op, tt.name, got, tt.field)
			}
			if strings.Contains(err.Error(), "another-value") || strings.Contains(err.Error(), "crypt-password-value") {
				t.Errorf("%s(%s): the error contains a secret", op, tt.name)
			}
		}
		if tt.field == "" && err == nil && updated.Wrapped != tt.in.Wrapped {
			t.Errorf("Update(%s).Wrapped = %+v", tt.name, updated.Wrapped)
		}
	}
	if got := e.secretsOf(t, vault); !maps.Equal(got, map[string]string{"password": "crypt-password-value", "password2": "crypt-salt-value"}) {
		t.Errorf("the stored secrets changed: %v", got)
	}

	// Fixed fields of other types are untouched by the rule: an s3
	// remote's secret still rotates.
	in := s3Input("Bucket")
	in.Secrets["secret_access_key"] = "rotated"
	if _, err := e.svc.Update(ctx, e.alice, bucket.ID, bucket.Version, in); err != nil {
		t.Errorf("Update(s3 secret) = %v", err)
	}
}

func TestWrappedDraft(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	bucket := e.mustCreate(t, e.alice, s3Input("Bucket"))
	rs, id, err := e.svc.Draft(ctx, e.alice, domain.RemoteID{}, cryptInput("", bucket.ID, "vault"))
	if err != nil {
		t.Fatal(err)
	}
	d := rs[id]
	if len(rs) != 2 || d.Wrapped.RemoteID != bucket.ID || !d.Capabilities.Wrapper || rs[bucket.ID].Rclone.Secrets["secret_access_key"] != "s3-secret-value" {
		t.Errorf("Draft(crypt) = %d remotes, draft %+v", len(rs), d)
	}
	if list, _ := e.svc.List(ctx, e.alice, remotes.ListFilter{Type: "crypt"}); len(list) != 0 {
		t.Errorf("Draft stored a remote: %v", list)
	}
}

func TestWrappedDelete(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	bucket := e.mustCreate(t, e.alice, s3Input("Bucket"))
	disk := e.mustCreate(t, e.alice, localInput("Disk", "/mnt/disk"))
	vault := e.mustCreate(t, e.alice, cryptInput("Vault", bucket.ID, "vault"))
	pool := e.mustCreate(t, e.alice, remotes.Input{Name: "Pool", Type: "union", Members: []remotes.Member{
		{RemoteID: bucket.ID, Mode: backend.UnionReadWrite}, {RemoteID: disk.ID, Mode: backend.UnionReadWrite},
	}})

	using, err := e.svc.RemotesUsing(ctx, e.alice, bucket.ID)
	if err != nil || !slices.Equal([]domain.RemoteID{using[0].ID, using[1].ID}, []domain.RemoteID{pool.ID, vault.ID}) {
		t.Errorf("RemotesUsing(bucket) = (%v, %v), want the union and the crypt remote by name", using, err)
	}
	if using, err := e.svc.RemotesUsing(ctx, e.alice, disk.ID); err != nil || len(using) != 1 || using[0].ID != pool.ID {
		t.Errorf("RemotesUsing(disk) = (%v, %v)", using, err)
	}
	_, err = e.svc.RemotesUsing(ctx, e.bob, bucket.ID)
	wantErr(t, "RemotesUsing(not the owner)", err, domain.ErrNotFound)

	// The union is first by name; once it is gone, the crypt remote is
	// named.
	err = e.svc.Delete(ctx, e.alice, bucket.ID)
	wantErr(t, "Delete(used)", err, domain.ErrFailedPrecondition)
	if err == nil || !strings.Contains(err.Error(), `the remote is a member of union "Pool"; remove it from the union first`) {
		t.Errorf("the refusal does not name the union: %v", err)
	}
	if err := e.svc.Delete(ctx, e.alice, pool.ID); err != nil {
		t.Fatal(err)
	}
	err = e.svc.Delete(ctx, e.alice, bucket.ID)
	wantErr(t, "Delete(wrapped)", err, domain.ErrFailedPrecondition)
	if err == nil || !strings.Contains(err.Error(), `the remote stores the data of encrypted remote "Vault"; delete that remote first`) {
		t.Errorf("the refusal does not name the crypt remote: %v", err)
	}
	if err := e.svc.Delete(ctx, e.alice, vault.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Delete(ctx, e.alice, bucket.ID); err != nil {
		t.Errorf("Delete(no longer wrapped) = %v", err)
	}
	// A crypt remote whose wrapped remote is gone cannot be resolved.
	if _, err := e.svc.Resolve(ctx, e.alice.UserID, []domain.RemoteID{vault.ID}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Resolve(deleted crypt) = %v", err)
	}
}
