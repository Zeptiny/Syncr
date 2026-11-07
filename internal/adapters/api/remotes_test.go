package api

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
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

// remotesEnv is a test server with the remote and backend services, on a
// store of their own (the job services of testEnv do not use remotes).
type remotesEnv struct {
	*testEnv
	svc     *remotes.Service
	reg     *backend.Registry
	svcLogs *logBuffer
}

// newRemotesEnv serves the built-in backend types plus extra.
func newRemotesEnv(t *testing.T, extra ...backend.Type) *remotesEnv {
	t.Helper()
	reg, err := backend.NewRegistry(append(builtin.Types(), extra...)...)
	if err != nil {
		t.Fatal(err)
	}
	key := secrets.Key{ID: 1, Bytes: make([]byte, secrets.KeySize)}
	if _, err := crand.Read(key.Bytes); err != nil {
		t.Fatal(err)
	}
	cipher, err := secrets.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	store := memstore.New()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	for _, a := range []domain.Actor{testAdmin, testUser} {
		u := users.User{ID: a.UserID, Username: string(a.Role), Role: a.Role, PasswordHash: "x",
			CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now, Version: 1}
		if err := store.CreateUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	svcLogs := &logBuffer{}
	svc, err := remotes.New(remotes.Deps{
		Remotes: store, Cipher: cipher, Clock: clock.NewFake(now), IDs: &idgen.Sequential{}, Backends: reg,
		Logger: newTestLogger(svcLogs),
	}, remotes.Config{})
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(d *Deps) { d.Remotes, d.Backends = svc, reg })
	return &remotesEnv{testEnv: e, svc: svc, reg: reg, svcLogs: svcLogs}
}

func (e *remotesEnv) remotesFor(a domain.Actor) syncrv1connect.RemoteServiceClient {
	return syncrv1connect.NewRemoteServiceClient(e.clientWith(testActorHeader, actorHeader(a, "session")), e.srv.URL)
}

func (e *remotesEnv) backendsFor(a domain.Actor) syncrv1connect.BackendServiceClient {
	return syncrv1connect.NewBackendServiceClient(e.clientWith(testActorHeader, actorHeader(a, "session")), e.srv.URL)
}

func (e *remotesEnv) create(a domain.Actor, in *syncrv1.RemoteInput) *syncrv1.Remote {
	e.t.Helper()
	resp, err := e.remotesFor(a).CreateRemote(e.t.Context(), connect.NewRequest(&syncrv1.CreateRemoteRequest{Input: in}))
	if err != nil {
		e.t.Fatalf("CreateRemote(%s): %v", in.GetName(), err)
	}
	return resp.Msg.GetRemote()
}

func localRemoteInput(name, path string) *syncrv1.RemoteInput {
	return &syncrv1.RemoteInput{Name: name, BackendType: "local", Options: map[string]string{"path": path}}
}

func s3RemoteInput(name string) *syncrv1.RemoteInput {
	return &syncrv1.RemoteInput{
		Name: name, BackendType: "s3",
		Options: map[string]string{"provider": "Minio", "access_key_id": "minio", "endpoint": "https://minio.example"},
		Secrets: map[string]string{"secret_access_key": "s3-secret-value"},
	}
}

// violations returns the field violations of a Connect error's
// ValidationError detail.
func fieldViolations(t *testing.T, err error) map[string]string {
	t.Helper()
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("error %v is not a Connect error", err)
	}
	out := map[string]string{}
	for _, d := range cerr.Details() {
		msg, derr := d.Value()
		if derr != nil {
			t.Fatal(derr)
		}
		if v, ok := msg.(*syncrv1.ValidationError); ok {
			for _, fv := range v.GetViolations() {
				out[fv.GetField()] = fv.GetMessage()
			}
		}
	}
	return out
}

func TestListBackends(t *testing.T) {
	t.Parallel()
	e := newRemotesEnv(t)
	resp, err := e.backendsFor(testUser).ListBackends(t.Context(), connect.NewRequest(&syncrv1.ListBackendsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	byID := map[string]*syncrv1.Backend{}
	for _, b := range resp.Msg.GetBackends() {
		ids = append(ids, b.GetId())
		byID[b.GetId()] = b
	}
	if want := []string{"s3", "b2", "local", "sftp", "union", "webdav"}; !slices.Equal(ids, want) {
		t.Errorf("backends = %v, want %v (by label)", ids, want)
	}
	field := func(b, name string) *syncrv1.BackendField {
		for _, f := range byID[b].GetFields() {
			if f.GetName() == name {
				return f
			}
		}
		t.Fatalf("backend %s has no field %s", b, name)
		return nil
	}
	if f := field("s3", "secret_access_key"); !f.GetSecret() || !f.GetRequired() || f.GetKind() != syncrv1.FieldKind_FIELD_KIND_STRING {
		t.Errorf("s3 secret_access_key = %v", f)
	}
	if f := field("s3", "provider"); f.GetKind() != syncrv1.FieldKind_FIELD_KIND_ENUM || f.GetDefaultValue() != "AWS" || len(f.GetChoices()) < 2 {
		t.Errorf("s3 provider = %v", f)
	}
	if f := field("union", "members"); f.GetKind() != syncrv1.FieldKind_FIELD_KIND_MEMBERS || !byID["union"].GetCapabilities().GetUnion() {
		t.Errorf("union members = %v", f)
	}
	if c := byID["local"].GetCapabilities(); !c.GetLocal() || !c.GetUnionMember() || c.GetUnion() {
		t.Errorf("local capabilities = %v", c)
	}

	// Unauthenticated callers get nothing.
	anon := syncrv1connect.NewBackendServiceClient(e.srv.Client(), e.srv.URL)
	_, err = anon.ListBackends(t.Context(), connect.NewRequest(&syncrv1.ListBackendsRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
}

func TestRemoteLifecycle(t *testing.T) {
	t.Parallel()
	e := newRemotesEnv(t)
	ctx := t.Context()
	c := e.remotesFor(testUser)

	r := e.create(testUser, s3RemoteInput("Archive"))
	if r.GetName() != "Archive" || r.GetBackendType() != "s3" || r.GetVersion() != 1 || !slices.Equal(r.GetSecretsSet(), []string{"secret_access_key"}) ||
		r.GetOptions()["endpoint"] != "https://minio.example" || r.GetCreatedAt() == nil {
		t.Errorf("CreateRemote = %v", r)
	}
	_, err := c.CreateRemote(ctx, connect.NewRequest(&syncrv1.CreateRemoteRequest{Input: s3RemoteInput("ARCHIVE")}))
	wantCode(t, err, connect.CodeAlreadyExists)

	d1 := e.create(testUser, localRemoteInput("disk1", "/mnt/d1"))
	d2 := e.create(testUser, localRemoteInput("disk2", "/mnt/d2"))
	pool := e.create(testUser, &syncrv1.RemoteInput{Name: "Pool", BackendType: "union", Members: []*syncrv1.UnionMember{
		{RemoteId: d1.GetId(), Path: "a"},
		{RemoteId: d2.GetId(), Mode: syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_ONLY},
	}})
	if m := pool.GetMembers(); len(m) != 2 || m[0].GetMode() != syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_WRITE || m[1].GetMode() != syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_ONLY {
		t.Errorf("union members = %v", m)
	}

	got, err := c.GetRemote(ctx, connect.NewRequest(&syncrv1.GetRemoteRequest{RemoteId: d1.GetId()}))
	if err != nil || !slices.Equal(got.Msg.GetRemote().GetUsedByUnions(), []string{pool.GetId()}) {
		t.Errorf("GetRemote(member) = (%v, %v), want used by the union", got, err)
	}
	list, err := c.ListRemotes(ctx, connect.NewRequest(&syncrv1.ListRemotesRequest{}))
	if err != nil || len(list.Msg.GetRemotes()) != 4 {
		t.Fatalf("ListRemotes = (%v, %v)", list, err)
	}
	for _, r := range list.Msg.GetRemotes() {
		if want := r.GetId() == d1.GetId() || r.GetId() == d2.GetId(); want != slices.Equal(r.GetUsedByUnions(), []string{pool.GetId()}) {
			t.Errorf("ListRemotes: %s used by %v", r.GetName(), r.GetUsedByUnions())
		}
	}
	locals, err := c.ListRemotes(ctx, connect.NewRequest(&syncrv1.ListRemotesRequest{BackendType: "local"}))
	if err != nil || len(locals.Msg.GetRemotes()) != 2 || len(locals.Msg.GetRemotes()[0].GetUsedByUnions()) != 1 {
		t.Errorf("ListRemotes(local) = (%v, %v)", locals, err)
	}

	// Deleting a member names the union.
	_, err = c.DeleteRemote(ctx, connect.NewRequest(&syncrv1.DeleteRemoteRequest{RemoteId: d1.GetId()}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	if !strings.Contains(err.Error(), `"Pool"`) {
		t.Errorf("the error does not name the union: %v", err)
	}

	// Update: stale version, then a real one.
	in := s3RemoteInput("Archive 2")
	in.Secrets = nil
	_, err = c.UpdateRemote(ctx, connect.NewRequest(&syncrv1.UpdateRemoteRequest{RemoteId: r.GetId(), Version: 7, Input: in}))
	wantCode(t, err, connect.CodeAborted)
	_, err = c.UpdateRemote(ctx, connect.NewRequest(&syncrv1.UpdateRemoteRequest{RemoteId: r.GetId(), Input: in}))
	wantCode(t, err, connect.CodeInvalidArgument)
	upd, err := c.UpdateRemote(ctx, connect.NewRequest(&syncrv1.UpdateRemoteRequest{RemoteId: r.GetId(), Version: 1, Input: in}))
	if err != nil || upd.Msg.GetRemote().GetName() != "Archive 2" || upd.Msg.GetRemote().GetVersion() != 2 ||
		!slices.Equal(upd.Msg.GetRemote().GetSecretsSet(), []string{"secret_access_key"}) {
		t.Errorf("UpdateRemote = (%v, %v), want the secret kept", upd, err)
	}

	if _, err := c.DeleteRemote(ctx, connect.NewRequest(&syncrv1.DeleteRemoteRequest{RemoteId: pool.GetId()})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteRemote(ctx, connect.NewRequest(&syncrv1.DeleteRemoteRequest{RemoteId: d1.GetId()})); err != nil {
		t.Errorf("DeleteRemote(member of a deleted union) = %v", err)
	}
	_, err = c.GetRemote(ctx, connect.NewRequest(&syncrv1.GetRemoteRequest{RemoteId: d1.GetId()}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = c.GetRemote(ctx, connect.NewRequest(&syncrv1.GetRemoteRequest{RemoteId: "nope"}))
	wantCode(t, err, connect.CodeInvalidArgument)
}

// TestRemotesOwnerOnly: another user's remote is NOT_FOUND for every RPC,
// admins included.
func TestRemotesOwnerOnly(t *testing.T) {
	t.Parallel()
	e := newRemotesEnv(t)
	ctx := t.Context()
	mine := e.create(testUser, localRemoteInput("mine", "/srv/mine"))
	admins := e.create(testAdmin, localRemoteInput("admins", "/srv/admin"))
	for _, tt := range []struct {
		who domain.Actor
		r   *syncrv1.Remote
	}{{testAdmin, mine}, {testUser, admins}} {
		c := e.remotesFor(tt.who)
		_, err := c.GetRemote(ctx, connect.NewRequest(&syncrv1.GetRemoteRequest{RemoteId: tt.r.GetId()}))
		wantCode(t, err, connect.CodeNotFound)
		_, err = c.UpdateRemote(ctx, connect.NewRequest(&syncrv1.UpdateRemoteRequest{
			RemoteId: tt.r.GetId(), Version: 1, Input: localRemoteInput("taken over", "/"),
		}))
		wantCode(t, err, connect.CodeNotFound)
		_, err = c.DeleteRemote(ctx, connect.NewRequest(&syncrv1.DeleteRemoteRequest{RemoteId: tt.r.GetId()}))
		wantCode(t, err, connect.CodeNotFound)
		list, err := c.ListRemotes(ctx, connect.NewRequest(&syncrv1.ListRemotesRequest{}))
		if err != nil || len(list.Msg.GetRemotes()) != 1 || list.Msg.GetRemotes()[0].GetId() == tt.r.GetId() {
			t.Errorf("ListRemotes(%s) = (%v, %v), want only their own", tt.who.Role, list, err)
		}
		// Nor can it be a member of the caller's union.
		own := list.Msg.GetRemotes()[0]
		_, err = c.CreateRemote(ctx, connect.NewRequest(&syncrv1.CreateRemoteRequest{Input: &syncrv1.RemoteInput{
			Name: "pool", BackendType: "union",
			Members: []*syncrv1.UnionMember{{RemoteId: own.GetId()}, {RemoteId: tt.r.GetId()}},
		}}))
		wantCode(t, err, connect.CodeInvalidArgument)
		if v := fieldViolations(t, err); v["members[1].remote_id"] == "" {
			t.Errorf("violations = %v, want members[1].remote_id", v)
		}
	}
}

func TestRemoteValidationErrors(t *testing.T) {
	t.Parallel()
	e := newRemotesEnv(t)
	c := e.remotesFor(testUser)
	for _, tt := range []struct {
		name  string
		in    *syncrv1.RemoteInput
		field string
	}{
		{"no input", nil, "name"},
		{"missing field", &syncrv1.RemoteInput{Name: "x", BackendType: "s3", Secrets: map[string]string{"secret_access_key": "s"}}, "options.access_key_id"},
		{"unknown type", &syncrv1.RemoteInput{Name: "x", BackendType: "gopher"}, "type"},
		{"bad member id", &syncrv1.RemoteInput{Name: "x", BackendType: "union", Members: []*syncrv1.UnionMember{{RemoteId: "zz"}}}, "members[0].remote_id"},
		{"bad member mode", &syncrv1.RemoteInput{Name: "x", BackendType: "union", Members: []*syncrv1.UnionMember{{Mode: 42}}}, "members[0].mode"},
		{"relative local path", localRemoteInput("x", "data"), "options.path"},
	} {
		_, err := c.CreateRemote(t.Context(), connect.NewRequest(&syncrv1.CreateRemoteRequest{Input: tt.in}))
		if codeOf(t, err) != connect.CodeInvalidArgument {
			t.Errorf("%s: err = %v, want InvalidArgument", tt.name, err)
			continue
		}
		if v := fieldViolations(t, err); v[tt.field] == "" {
			t.Errorf("%s: violations = %v, want %s", tt.name, v, tt.field)
		}
	}
}

// sentinel is a recognizable secret value.
func sentinel(typ, key string, round int) string {
	return fmt.Sprintf("SENTINEL-%s-%s-%d-c0ffee", typ, key, round)
}

// TestSecretsNeverEcho creates, reads, lists and updates a remote of every
// registered type with sentinel secret values, and checks that no response
// (as protojson) and no log line contains one.
func TestSecretsNeverEcho(t *testing.T) {
	t.Parallel()
	e := newRemotesEnv(t)
	checkSecretsNeverEcho(t, e)
}

// checkSecretsNeverEcho runs the secret echo check on every type of e's
// registry.
func checkSecretsNeverEcho(t *testing.T, e *remotesEnv) {
	t.Helper()
	ctx := t.Context()
	c := e.remotesFor(testUser)
	var seen []string // every sentinel sent
	var responses []string
	record := func(m proto.Message) {
		b, err := protojson.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		responses = append(responses, string(b))
	}
	recordErr := func(err error) {
		var cerr *connect.Error
		if errors.As(err, &cerr) {
			responses = append(responses, cerr.Error())
			for _, d := range cerr.Details() {
				if m, derr := d.Value(); derr == nil {
					record(m)
				}
			}
		}
	}
	m1 := e.create(testUser, localRemoteInput("member 1", "/srv/m1"))
	m2 := e.create(testUser, localRemoteInput("member 2", "/srv/m2"))

	for _, typ := range e.reg.Types() {
		cfg := exampleConfig(typ)
		in := &syncrv1.RemoteInput{Name: "remote " + typ.ID(), BackendType: typ.ID(), Options: cfg.Options, Secrets: map[string]string{}}
		for _, f := range typ.Fields() {
			if f.Secret {
				in.Secrets[f.Name] = sentinel(typ.ID(), f.Name, 1)
			}
		}
		if typ.Capabilities().Union {
			in.Members = []*syncrv1.UnionMember{{RemoteId: m1.GetId()}, {RemoteId: m2.GetId()}}
		}
		for _, v := range in.GetSecrets() {
			seen = append(seen, v)
		}

		created, err := c.CreateRemote(ctx, connect.NewRequest(&syncrv1.CreateRemoteRequest{Input: in}))
		if err != nil {
			t.Fatalf("%s: CreateRemote: %v", typ.ID(), err)
		}
		record(created.Msg)
		r := created.Msg.GetRemote()
		wantSet := slices.Sorted(maps.Keys(in.GetSecrets()))
		if !slices.Equal(r.GetSecretsSet(), wantSet) {
			t.Errorf("%s: secrets_set = %v, want %v", typ.ID(), r.GetSecretsSet(), wantSet)
		}

		got, err := c.GetRemote(ctx, connect.NewRequest(&syncrv1.GetRemoteRequest{RemoteId: r.GetId()}))
		if err != nil {
			t.Fatalf("%s: GetRemote: %v", typ.ID(), err)
		}
		record(got.Msg)

		// Update with new sentinels, then with none (kept).
		upd, _ := proto.Clone(in).(*syncrv1.RemoteInput)
		for k := range upd.Secrets {
			upd.Secrets[k] = sentinel(typ.ID(), k, 2)
			seen = append(seen, upd.Secrets[k])
		}
		updated, err := c.UpdateRemote(ctx, connect.NewRequest(&syncrv1.UpdateRemoteRequest{RemoteId: r.GetId(), Version: r.GetVersion(), Input: upd}))
		if err != nil {
			t.Fatalf("%s: UpdateRemote: %v", typ.ID(), err)
		}
		record(updated.Msg)
		upd.Secrets = nil
		kept, err := c.UpdateRemote(ctx, connect.NewRequest(&syncrv1.UpdateRemoteRequest{RemoteId: r.GetId(), Version: updated.Msg.GetRemote().GetVersion(), Input: upd}))
		if err != nil {
			t.Fatalf("%s: UpdateRemote(keep secrets): %v", typ.ID(), err)
		}
		record(kept.Msg)

		// Rejected inputs echo nothing either: a secret in options, an
		// unknown secret and a non-secret field sent as a secret.
		for _, f := range typ.Fields() {
			bad, _ := proto.Clone(upd).(*syncrv1.RemoteInput)
			bad.Secrets = map[string]string{"zz_unknown": sentinel(typ.ID(), "zz_unknown", 3)}
			if f.Secret {
				bad.Options[f.Name] = sentinel(typ.ID(), f.Name, 3)
			} else if f.Kind != backend.KindMembers {
				bad.Secrets[f.Name] = sentinel(typ.ID(), f.Name, 3)
			}
			for _, v := range bad.GetOptions() {
				if strings.HasPrefix(v, "SENTINEL-") {
					seen = append(seen, v)
				}
			}
			for _, v := range bad.GetSecrets() {
				seen = append(seen, v)
			}
			_, err := c.UpdateRemote(ctx, connect.NewRequest(&syncrv1.UpdateRemoteRequest{RemoteId: r.GetId(), Version: kept.Msg.GetRemote().GetVersion(), Input: bad}))
			wantCode(t, err, connect.CodeInvalidArgument)
			recordErr(err)
			_, err = c.CreateRemote(ctx, connect.NewRequest(&syncrv1.CreateRemoteRequest{Input: bad}))
			wantCode(t, err, connect.CodeInvalidArgument)
			recordErr(err)
		}

		// The secrets are stored: the dispatcher's view has the latest ones.
		uid := testUser.UserID
		res, err := e.svc.Resolve(ctx, uid, []domain.RemoteID{mustRemoteID(t, r.GetId())})
		if err != nil {
			t.Fatalf("%s: Resolve: %v", typ.ID(), err)
		}
		for k := range in.GetSecrets() {
			if !slices.Contains(slices.Collect(maps.Values(res[mustRemoteID(t, r.GetId())].Rclone.Secrets)), sentinel(typ.ID(), k, 2)) {
				t.Errorf("%s: secret %s was not stored", typ.ID(), k)
			}
		}
	}
	list, err := c.ListRemotes(ctx, connect.NewRequest(&syncrv1.ListRemotesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	record(list.Msg)

	logs := e.logs.String() + e.svcLogs.String()
	for _, s := range seen {
		for _, resp := range responses {
			if strings.Contains(resp, s) {
				t.Errorf("a response contains the secret %q: %s", s, resp)
			}
		}
		if strings.Contains(logs, s) {
			t.Errorf("the log contains the secret %q", s)
		}
	}
	if len(seen) == 0 || !strings.Contains(e.svcLogs.String(), "remote created") {
		t.Fatal("the check saw no secrets or no log lines")
	}
}

// exampleConfig returns a valid configuration of typ: its first example,
// or every field set to a value of its kind.
func exampleConfig(typ backend.Type) backend.Config {
	if ex, ok := typ.(backend.Exampler); ok {
		return ex.Examples()[0].Clone()
	}
	cfg := backend.Config{Options: map[string]string{}}
	for _, f := range typ.Fields() {
		if f.Secret || f.Kind == backend.KindMembers {
			continue
		}
		switch f.Kind {
		case backend.KindInt:
			cfg.Options[f.Name] = "1"
		case backend.KindBool:
			cfg.Options[f.Name] = "true"
		case backend.KindEnum:
			cfg.Options[f.Name] = f.Choices[0].Value
		default:
			cfg.Options[f.Name] = "value"
		}
	}
	return cfg
}

func mustRemoteID(t *testing.T, s string) domain.RemoteID {
	t.Helper()
	id, err := domain.ParseRemoteID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
