package remotes_test

import (
	"context"
	"testing"

	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

func ftpInput(name string) remotes.Input {
	return remotes.Input{Name: name, Type: "ftp", Options: map[string]string{"host": "ftp.example", "user": "anonymous"}}
}

func b2Input(name string) remotes.Input {
	return remotes.Input{Name: name, Type: "b2", Options: map[string]string{"account": "0012345"}, Secrets: map[string]string{"key": "b2-secret-value"}}
}

// TestUnionNoChecksum: a union keeps no checksums when one of its members
// keeps none (an FTP member), in Infos (the API's and the scheduler's
// checks), Resolve (the dispatcher and the probes) and Draft; a union of
// S3 and B2 keeps its type's capabilities unchanged.
func TestUnionNoChecksum(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	s3 := e.mustCreate(t, e.alice, s3Input("bucket"))
	b2 := e.mustCreate(t, e.alice, b2Input("b2"))
	ftp := e.mustCreate(t, e.alice, ftpInput("ftp"))
	member := func(r remotes.Remote) remotes.Member {
		return remotes.Member{RemoteID: r.ID, Mode: backend.UnionReadWrite}
	}
	union := func(name string, ms ...remotes.Member) remotes.Input {
		return remotes.Input{Name: name, Type: "union", Options: map[string]string{}, Members: ms}
	}
	withFTP := e.mustCreate(t, e.alice, union("with ftp", member(s3), member(ftp)))
	plain := e.mustCreate(t, e.alice, union("plain", member(s3), member(b2)))
	unionCaps := backend.Capabilities{Union: true}

	infos, err := e.svc.Infos(ctx, e.alice.UserID, []domain.RemoteID{withFTP.ID, plain.ID, ftp.ID, s3.ID})
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[domain.RemoteID]bool{withFTP.ID: true, plain.ID: false, ftp.ID: true, s3.ID: false} {
		if got := infos[id].Capabilities.NoChecksum; got != want {
			t.Errorf("Infos: %s NoChecksum = %v, want %v", id, got, want)
		}
	}
	if got := infos[plain.ID].Capabilities; got != unionCaps {
		t.Errorf("Infos(union of S3 and B2) = %+v, want the union type's %+v", got, unionCaps)
	}
	if got, want := infos[withFTP.ID].Capabilities, (backend.Capabilities{Union: true, NoChecksum: true}); got != want {
		t.Errorf("Infos(union with FTP) = %+v, want %+v", got, want)
	}

	res, err := e.svc.Resolve(ctx, e.alice.UserID, []domain.RemoteID{withFTP.ID, plain.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !res[withFTP.ID].Capabilities.NoChecksum || res[plain.ID].Capabilities != unionCaps || !res[ftp.ID].Capabilities.NoChecksum ||
		res[s3.ID].Capabilities.NoChecksum || res[b2.ID].Capabilities.NoChecksum {
		t.Errorf("Resolve: union with FTP %+v, union of S3 and B2 %+v", res[withFTP.ID].Capabilities, res[plain.ID].Capabilities)
	}

	// A draft union (a remote test before saving) follows the same rule.
	draft, id, err := e.svc.Draft(ctx, e.alice, domain.RemoteID{}, union("draft", member(s3), member(ftp)))
	if err != nil || !draft[id].Capabilities.NoChecksum {
		t.Errorf("Draft(union with FTP) = %+v (%v)", draft[id].Capabilities, err)
	}
	draft, id, err = e.svc.Draft(ctx, e.alice, domain.RemoteID{}, union("draft", member(s3), member(b2)))
	if err != nil || draft[id].Capabilities != unionCaps {
		t.Errorf("Draft(union of S3 and B2) = %+v (%v)", draft[id].Capabilities, err)
	}
}
