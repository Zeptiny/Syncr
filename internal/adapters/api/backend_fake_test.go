package api

import (
	"slices"
	"testing"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/backendtest"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// fakeType is a backend type that exists only in this test. It proves that
// adding a backend type is one descriptor: registered next to the built-in
// types, it is served by ListBackends and usable through RemoteService with
// no other code change. It has no examples, so the contract suite
// synthesizes its configuration.
type fakeType struct{}

func (fakeType) ID() string          { return "fake" }
func (fakeType) Label() string       { return "Fake storage" }
func (fakeType) Description() string { return "A backend type for tests." }

func (fakeType) Fields() []backend.Field {
	return []backend.Field{
		{Name: "server", Label: "Server", Kind: backend.KindString, Required: true},
		{Name: "token", Label: "Token", Kind: backend.KindString, Required: true, Secret: true, Obscure: true},
		{Name: "tier", Label: "Tier", Kind: backend.KindEnum, Default: "hot", Advanced: true,
			Choices: []backend.Choice{{Value: "hot", Label: "Hot"}, {Value: "cold", Label: "Cold"}}},
		{Name: "base", Label: "Base path", Kind: backend.KindString, Rclone: backend.NotRclone},
	}
}

func (fakeType) Capabilities() backend.Capabilities {
	return backend.Capabilities{UnionMember: true}
}

func (f fakeType) Validate(cfg backend.Config) error {
	var v domain.Validation
	v.AddAll("", backend.CheckFields(f.Fields(), cfg))
	return v.Err()
}

func (f fakeType) ToRclone(cfg backend.Config) (backend.Rclone, error) {
	r := backend.MapFields(f.Fields(), cfg)
	r.Backend, r.Root = "fake", cfg.Options["base"]
	return r, nil
}

func TestFakeBackendIsOneFile(t *testing.T) {
	t.Parallel()
	backendtest.Run(t, fakeType{})

	e := newRemotesEnv(t, fakeType{})
	resp, err := e.backendsFor(testUser).ListBackends(t.Context(), connect.NewRequest(&syncrv1.ListBackendsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(resp.Msg.GetBackends(), func(b *syncrv1.Backend) bool { return b.GetId() == "fake" })
	if i < 0 {
		t.Fatal("ListBackends does not list the fake type")
	}
	fake := resp.Msg.GetBackends()[i]
	var names []string
	for _, f := range fake.GetFields() {
		names = append(names, f.GetName())
	}
	if fake.GetLabel() != "Fake storage" || !slices.Equal(names, []string{"server", "token", "tier", "base"}) ||
		!fake.GetFields()[1].GetSecret() || fake.GetFields()[2].GetDefaultValue() != "hot" || !fake.GetCapabilities().GetUnionMember() {
		t.Errorf("fake backend = %v", fake)
	}

	r := e.create(testUser, &syncrv1.RemoteInput{
		Name: "My fake", BackendType: "fake",
		Options: map[string]string{"server": "fake.example", "base": "root/dir"},
		Secrets: map[string]string{"token": "fake-token-value"},
	})
	if r.GetBackendType() != "fake" || !slices.Equal(r.GetSecretsSet(), []string{"token"}) {
		t.Errorf("CreateRemote(fake) = %v", r)
	}
	res, err := e.svc.Resolve(t.Context(), testUser.UserID, []domain.RemoteID{mustRemoteID(t, r.GetId())})
	got := res[mustRemoteID(t, r.GetId())].Rclone
	if err != nil || got.Backend != "fake" || got.Root != "root/dir" || got.Secrets["token"] != "fake-token-value" || got.Options["tier"] != "hot" {
		t.Errorf("Resolve(fake) = (%+v, %v)", got, err)
	}
	// It can be a union member, and its secrets never echo.
	e.create(testUser, &syncrv1.RemoteInput{Name: "Pool", BackendType: "union", Members: []*syncrv1.UnionMember{
		{RemoteId: r.GetId(), Path: "a"}, {RemoteId: r.GetId(), Path: "b"},
	}})
	checkSecretsNeverEcho(t, e)
}
