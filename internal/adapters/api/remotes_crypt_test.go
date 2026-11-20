package api

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// everyFieldSet fails the test for each field of m that is not set, so a
// field added to the message without a conversion fails it.
func everyFieldSet(t *testing.T, m proto.Message) {
	t.Helper()
	r := m.ProtoReflect()
	fields := r.Descriptor().Fields()
	for i := range fields.Len() {
		if fd := fields.Get(i); !r.Has(fd) {
			t.Errorf("%s.%s is not set by the conversion (or the test)", r.Descriptor().Name(), fd.Name())
		}
	}
}

func TestRemoteToProto(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	id, member, wrapped, union, wrapper := domain.NewRemoteID(), domain.NewRemoteID(), domain.NewRemoteID(), domain.NewRemoteID(), domain.NewRemoteID()
	// Not a real remote (a union that wraps): every field set.
	r := remotes.Remote{
		ID: id, OwnerID: domain.NewUserID(), Name: "Vault", Type: "crypt",
		Options: map[string]string{"filename_encryption": "off"}, SecretsEnc: []byte("never shown"), SecretKeys: []string{"password"},
		Members:   []remotes.Member{{RemoteID: member, Path: "m", Mode: backend.UnionReadOnly}},
		Wrapped:   remotes.Wrapped{RemoteID: wrapped, Path: "vault/data"},
		CreatedAt: at, UpdatedAt: at.Add(time.Minute), Version: 3,
	}
	using := []remotes.Remote{
		{ID: union, Name: "Pool", Members: []remotes.Member{{RemoteID: id}}},
		{ID: wrapper, Name: "Inner", Wrapped: remotes.Wrapped{RemoteID: id, Path: "x"}},
	}
	got := remoteToProto(r, using)
	want := &syncrv1.Remote{
		Id: id.String(), Name: "Vault", BackendType: "crypt", Options: map[string]string{"filename_encryption": "off"},
		SecretsSet: []string{"password"}, CreatedAt: ts(at), UpdatedAt: ts(at.Add(time.Minute)), Version: 3,
		Members:      []*syncrv1.UnionMember{{RemoteId: member.String(), Path: "m", Mode: syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_ONLY}},
		UsedByUnions: []string{union.String()},
		Wrapped:      &syncrv1.WrappedRemote{RemoteId: wrapped.String(), Path: "vault/data"},
		WrappedBy:    []string{wrapper.String()},
	}
	if !proto.Equal(got, want) {
		t.Errorf("remoteToProto =\n%v\nwant\n%v", got, want)
	}
	everyFieldSet(t, got)

	// A remote that wraps nothing has no wrapped message; none uses it.
	plain := remoteToProto(remotes.Remote{ID: id, Name: "S3", Type: "s3", Version: 1}, nil)
	if plain.GetWrapped() != nil || len(plain.GetWrappedBy()) != 0 || len(plain.GetUsedByUnions()) != 0 {
		t.Errorf("plain remote = %v", plain)
	}
}

func TestRemoteInputFromProto(t *testing.T) {
	t.Parallel()
	member, wrapped := domain.NewRemoteID(), domain.NewRemoteID()
	in := &syncrv1.RemoteInput{
		Name: "Vault", BackendType: "crypt", Options: map[string]string{"suffix": "none"}, Secrets: map[string]string{"password": "pw"},
		ClearSecrets: []string{"password2"},
		Members:      []*syncrv1.UnionMember{{RemoteId: member.String(), Path: "m", Mode: syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_NO_CREATE}},
		Wrapped:      &syncrv1.WrappedRemote{RemoteId: wrapped.String(), Path: "vault"},
	}
	everyFieldSet(t, in)
	got, err := remoteInputFromProto(in)
	if err != nil || got.Name != "Vault" || got.Type != "crypt" || got.Options["suffix"] != "none" || got.Secrets["password"] != "pw" ||
		!slices.Equal(got.ClearSecrets, []string{"password2"}) ||
		!slices.Equal(got.Members, []remotes.Member{{RemoteID: member, Path: "m", Mode: backend.UnionNoCreate}}) ||
		got.Wrapped != (remotes.Wrapped{RemoteID: wrapped, Path: "vault"}) {
		t.Errorf("remoteInputFromProto = (%+v, %v)", got, err)
	}
	// A path alone is kept (the service says the remote is required); a
	// malformed ID is a field error.
	got, err = remoteInputFromProto(&syncrv1.RemoteInput{Wrapped: &syncrv1.WrappedRemote{Path: "p"}})
	if err != nil || got.Wrapped != (remotes.Wrapped{Path: "p"}) {
		t.Errorf("remoteInputFromProto(path only) = (%+v, %v)", got.Wrapped, err)
	}
	_, err = remoteInputFromProto(&syncrv1.RemoteInput{Wrapped: &syncrv1.WrappedRemote{RemoteId: "nope"}})
	var verr *domain.ValidationError
	if !errors.As(err, &verr) || len(verr.Fields) != 1 || verr.Fields[0].Field != "wrapped.remote_id" || verr.Fields[0].Message != "is not a valid ID" {
		t.Errorf("remoteInputFromProto(bad id) = %v", err)
	}
}

// cryptRemoteInput is an encrypted remote that stores its data at path on
// remote wrapped.
func cryptRemoteInput(name, wrapped, path string) *syncrv1.RemoteInput {
	return &syncrv1.RemoteInput{
		Name: name, BackendType: "crypt", Secrets: map[string]string{"password": "crypt-secret-value"},
		Wrapped: &syncrv1.WrappedRemote{RemoteId: wrapped, Path: path},
	}
}

func TestCryptRemoteLifecycle(t *testing.T) {
	t.Parallel()
	e := newRemotesEnv(t)
	ctx := t.Context()
	c := e.remotesFor(testUser)
	bucket := e.create(testUser, s3RemoteInput("Bucket"))
	disk := e.create(testUser, localRemoteInput("Disk", "/mnt/disk"))
	pool := e.create(testUser, &syncrv1.RemoteInput{Name: "Pool", BackendType: "union", Members: []*syncrv1.UnionMember{
		{RemoteId: bucket.GetId(), Path: "pool"}, {RemoteId: disk.GetId()},
	}})
	vault := e.create(testUser, cryptRemoteInput("Vault", bucket.GetId(), "vault"))
	if w := vault.GetWrapped(); w.GetRemoteId() != bucket.GetId() || w.GetPath() != "vault" || !slices.Equal(vault.GetSecretsSet(), []string{"password"}) {
		t.Errorf("CreateRemote(crypt) = %v", vault)
	}

	// The wrapped remote names its wrapper apart from its unions.
	got, err := c.GetRemote(ctx, connect.NewRequest(&syncrv1.GetRemoteRequest{RemoteId: bucket.GetId()}))
	if err != nil || !slices.Equal(got.Msg.GetRemote().GetWrappedBy(), []string{vault.GetId()}) ||
		!slices.Equal(got.Msg.GetRemote().GetUsedByUnions(), []string{pool.GetId()}) {
		t.Errorf("GetRemote(wrapped) = (%v, %v)", got, err)
	}
	list, err := c.ListRemotes(ctx, connect.NewRequest(&syncrv1.ListRemotesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range list.Msg.GetRemotes() {
		wantWrappedBy, wantUnions := []string{}, []string{}
		switch r.GetId() {
		case bucket.GetId():
			wantWrappedBy, wantUnions = []string{vault.GetId()}, []string{pool.GetId()}
		case disk.GetId():
			wantUnions = []string{pool.GetId()}
		case vault.GetId():
			if r.GetWrapped().GetRemoteId() != bucket.GetId() {
				t.Errorf("ListRemotes: the crypt remote's wrapped = %v", r.GetWrapped())
			}
		}
		if !slices.Equal(r.GetWrappedBy(), wantWrappedBy) || !slices.Equal(r.GetUsedByUnions(), wantUnions) {
			t.Errorf("ListRemotes: %s wrapped by %v, used by %v", r.GetName(), r.GetWrappedBy(), r.GetUsedByUnions())
		}
	}

	// Fixed fields: another value is a field error; the name and the
	// folder may change.
	upd := cryptRemoteInput("Vault 2", bucket.GetId(), "moved")
	upd.Secrets, upd.Options = nil, map[string]string{"filename_encryption": "off"}
	_, err = c.UpdateRemote(ctx, connect.NewRequest(&syncrv1.UpdateRemoteRequest{RemoteId: vault.GetId(), Version: vault.GetVersion(), Input: upd}))
	wantCode(t, err, connect.CodeInvalidArgument)
	if v := fieldViolations(t, err); v["options.filename_encryption"] != "cannot be changed after the remote is created" {
		t.Errorf("UpdateRemote(fixed option) violations = %v", v)
	}
	upd.Options = nil
	upd.ClearSecrets = []string{"password"}
	_, err = c.UpdateRemote(ctx, connect.NewRequest(&syncrv1.UpdateRemoteRequest{RemoteId: vault.GetId(), Version: vault.GetVersion(), Input: upd}))
	if v := fieldViolations(t, err); v["clear_secrets[0]"] != "cannot be changed after the remote is created" {
		t.Errorf("UpdateRemote(clear the password) violations = %v", v)
	}
	upd.ClearSecrets = nil
	moved, err := c.UpdateRemote(ctx, connect.NewRequest(&syncrv1.UpdateRemoteRequest{RemoteId: vault.GetId(), Version: vault.GetVersion(), Input: upd}))
	if err != nil || moved.Msg.GetRemote().GetName() != "Vault 2" || moved.Msg.GetRemote().GetWrapped().GetPath() != "moved" ||
		!slices.Equal(moved.Msg.GetRemote().GetSecretsSet(), []string{"password"}) {
		t.Errorf("UpdateRemote(name and folder) = (%v, %v)", moved, err)
	}

	// The wrapped remote cannot be deleted while the crypt remote stores
	// its data in it.
	if _, err := c.DeleteRemote(ctx, connect.NewRequest(&syncrv1.DeleteRemoteRequest{RemoteId: pool.GetId()})); err != nil {
		t.Fatal(err)
	}
	_, err = c.DeleteRemote(ctx, connect.NewRequest(&syncrv1.DeleteRemoteRequest{RemoteId: bucket.GetId()}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	if err == nil || !strings.Contains(err.Error(), `the remote stores the data of encrypted remote "Vault 2"; delete that remote first`) {
		t.Errorf("DeleteRemote(wrapped) = %v", err)
	}
	if _, err := c.DeleteRemote(ctx, connect.NewRequest(&syncrv1.DeleteRemoteRequest{RemoteId: vault.GetId()})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteRemote(ctx, connect.NewRequest(&syncrv1.DeleteRemoteRequest{RemoteId: bucket.GetId()})); err != nil {
		t.Errorf("DeleteRemote(no longer wrapped) = %v", err)
	}
}

func TestCryptRemoteValidation(t *testing.T) {
	t.Parallel()
	e := newRemotesEnv(t)
	c := e.remotesFor(testUser)
	bucket := e.create(testUser, s3RemoteInput("Bucket"))
	disk := e.create(testUser, localRemoteInput("Disk", "/mnt/disk"))
	vault := e.create(testUser, cryptRemoteInput("Vault", bucket.GetId(), ""))
	admins := e.create(testAdmin, s3RemoteInput("Admin's"))
	withWrapped := s3RemoteInput("x")
	withWrapped.Wrapped = &syncrv1.WrappedRemote{RemoteId: bucket.GetId()}
	for _, tt := range []struct {
		name  string
		in    *syncrv1.RemoteInput
		field string
		msg   string
	}{
		{"not a wrapper", withWrapped, "wrapped", "only encrypted remotes store their data in another remote"},
		{"no wrapped remote", &syncrv1.RemoteInput{Name: "x", BackendType: "crypt", Secrets: map[string]string{"password": "p"}}, "wrapped.remote_id", "is required"},
		{"a malformed ID", cryptRemoteInput("x", "zz", ""), "wrapped.remote_id", "is not a valid ID"},
		{"another user's remote", cryptRemoteInput("x", admins.GetId(), ""), "wrapped.remote_id", "is not one of your remotes"},
		{"a missing remote", cryptRemoteInput("x", domain.NewRemoteID().String(), ""), "wrapped.remote_id", "is not one of your remotes"},
		{"a local remote", cryptRemoteInput("x", disk.GetId(), ""), "wrapped.remote_id", "cannot be a folder of a worker"},
		{"another crypt remote", cryptRemoteInput("x", vault.GetId(), ""), "wrapped.remote_id", "cannot be a union or another encrypted remote"},
		{"a dot-dot path", cryptRemoteInput("x", bucket.GetId(), "a/../b"), "wrapped.path", `path must not contain ".." segments`},
		{"a bad suffix", func() *syncrv1.RemoteInput {
			in := cryptRemoteInput("x", bucket.GetId(), "")
			in.Options = map[string]string{"suffix": "x/y"}
			return in
		}(), "options.suffix", ""},
	} {
		_, err := c.CreateRemote(t.Context(), connect.NewRequest(&syncrv1.CreateRemoteRequest{Input: tt.in}))
		if codeOf(t, err) != connect.CodeInvalidArgument {
			t.Errorf("%s: err = %v, want InvalidArgument", tt.name, err)
			continue
		}
		v := fieldViolations(t, err)
		if msg, ok := v[tt.field]; !ok || (tt.msg != "" && msg != tt.msg) {
			t.Errorf("%s: violations = %v, want %s %q", tt.name, v, tt.field, tt.msg)
		}
		if strings.Contains(err.Error(), "crypt-secret-value") {
			t.Errorf("%s: the error contains the password", tt.name)
		}
	}
	// A crypt remote cannot be a union member.
	_, err := c.CreateRemote(t.Context(), connect.NewRequest(&syncrv1.CreateRemoteRequest{Input: &syncrv1.RemoteInput{
		Name: "Pool", BackendType: "union", Members: []*syncrv1.UnionMember{{RemoteId: disk.GetId()}, {RemoteId: vault.GetId()}},
	}}))
	if v := fieldViolations(t, err); v["members[1].remote_id"] != "cannot be a union member" {
		t.Errorf("a union of a crypt remote: violations = %v", v)
	}
}
